// Package main tests. Exercises githubLitePlugin's adapter layer against an
// httptest fake GitHub and a fake Host — no go-plugin subprocess needed.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/kandev/kandev/pkg/pluginsdk"
	"github.com/stretchr/testify/require"

	githublite "kandev-plugin-github-lite/server/githublite"
)

// ---------------------------------------------------------------------------
// Fake Host
// ---------------------------------------------------------------------------

type fakeHost struct {
	pluginsdk.UnimplementedHostData

	mu     sync.Mutex
	state  map[string]map[string]map[string]map[string]any // scope -> scopeID -> key -> value
	config map[string]any
	tasks  map[string]*pluginsdk.Task
	repos  []pluginsdk.Repository
}

func newFakeHost(config map[string]any) *fakeHost {
	if config == nil {
		config = map[string]any{"github_token": "test-token"}
	}
	return &fakeHost{
		state:  make(map[string]map[string]map[string]map[string]any),
		config: config,
		tasks:  make(map[string]*pluginsdk.Task),
	}
}

func (h *fakeHost) GetConfig(context.Context) (map[string]any, error) {
	return h.config, nil
}

func (h *fakeHost) GetState(_ context.Context, scope, scopeID, key string) (map[string]any, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	value, found := h.state[scope][scopeID][key]
	return value, found, nil
}

func (h *fakeHost) SetState(_ context.Context, scope, scopeID, key string, value map[string]any) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.state[scope] == nil {
		h.state[scope] = make(map[string]map[string]map[string]any)
	}
	if h.state[scope][scopeID] == nil {
		h.state[scope][scopeID] = make(map[string]map[string]any)
	}
	h.state[scope][scopeID][key] = value
	return nil
}

func (h *fakeHost) ListState(_ context.Context, scope, scopeID string) ([]pluginsdk.StateEntry, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var entries []pluginsdk.StateEntry
	for key, value := range h.state[scope][scopeID] {
		entries = append(entries, pluginsdk.StateEntry{Key: key, Value: value})
	}
	return entries, nil
}

func (h *fakeHost) DeleteState(_ context.Context, scope, scopeID, key string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.state[scope][scopeID], key)
	return nil
}

func (h *fakeHost) RevealSecret(context.Context, string) (string, error)    { return "", nil }
func (h *fakeHost) GetSecret(context.Context, string) (string, bool, error) { return "", false, nil }
func (h *fakeHost) SetSecret(context.Context, string, string) error         { return nil }
func (h *fakeHost) DeleteSecret(context.Context, string) error              { return nil }
func (h *fakeHost) EmitEvent(context.Context, string, map[string]any) error { return nil }

func (h *fakeHost) Tasks() pluginsdk.TaskReader { return fakeTaskReader{host: h} }

type fakeTaskReader struct{ host *fakeHost }

func (r fakeTaskReader) Get(_ context.Context, id string) (*pluginsdk.Task, error) {
	r.host.mu.Lock()
	defer r.host.mu.Unlock()
	task, found := r.host.tasks[id]
	if !found {
		return nil, fmt.Errorf("task %s not found", id)
	}
	return task, nil
}

func (r fakeTaskReader) List(context.Context, pluginsdk.TaskFilter, pluginsdk.Page) ([]pluginsdk.Task, *pluginsdk.PageInfo, error) {
	return nil, nil, fmt.Errorf("not used")
}

func (r fakeTaskReader) Create(context.Context, pluginsdk.CreateTaskInput) (*pluginsdk.Task, error) {
	return nil, fmt.Errorf("not used")
}

func (r fakeTaskReader) Update(context.Context, pluginsdk.UpdateTaskInput) (*pluginsdk.Task, error) {
	return nil, fmt.Errorf("not used")
}

func (r fakeTaskReader) Move(context.Context, pluginsdk.MoveTaskInput) (*pluginsdk.MoveTaskOutcome, error) {
	return nil, fmt.Errorf("not used")
}

func (h *fakeHost) Repositories() pluginsdk.RepositoryReader { return fakeRepositoryReader{host: h} }

type fakeRepositoryReader struct{ host *fakeHost }

func (r fakeRepositoryReader) List(_ context.Context, workspaceID string, page pluginsdk.Page) ([]pluginsdk.Repository, *pluginsdk.PageInfo, error) {
	r.host.mu.Lock()
	defer r.host.mu.Unlock()
	rows := make([]pluginsdk.Repository, 0)
	for _, row := range r.host.repos {
		if row.WorkspaceID == workspaceID {
			rows = append(rows, row)
		}
	}
	return rows, &pluginsdk.PageInfo{HasMore: false}, nil
}

// ---------------------------------------------------------------------------
// Fake GitHub
// ---------------------------------------------------------------------------

// fakeGitHub is an httptest GitHub with just enough REST surface:
// /user, /user/repos, /repositories/{id}, /repos/{o}/{r}, /repos/{o}/{r}/pulls,
// /repos/{o}/{r}/pulls/{n}(+/reviews), /commits/{sha}/check-runs,
// /issues/{n}/comments.
type fakeGitHub struct {
	t        *testing.T
	mu       sync.Mutex
	user     map[string]any
	repos    []map[string]any
	pulls    map[string]map[string]any // "owner/repo#n" -> pull
	reviews  map[string][]map[string]any
	checks   map[string][]map[string]any
	comments map[string][]map[string]any

	requests    []string
	limitedOnce bool
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	return &fakeGitHub{
		t:    t,
		user: map[string]any{"login": "testuser"},
		repos: []map[string]any{
			{"id": 5001, "name": "widgets", "full_name": "acme/widgets", "owner": map[string]any{"login": "acme"}, "clone_url": "https://github.com/acme/widgets.git", "default_branch": "main"},
		},
		pulls:    make(map[string]map[string]any),
		reviews:  make(map[string][]map[string]any),
		checks:   make(map[string][]map[string]any),
		comments: make(map[string][]map[string]any),
	}
}

func (f *fakeGitHub) addPull(owner, repo string, number int, head, base string) {
	key := fmt.Sprintf("%s/%s#%d", owner, repo, number)
	f.pulls[key] = map[string]any{
		"number": number, "state": "open", "title": fmt.Sprintf("PR %d", number),
		"html_url":  fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, repo, number),
		"user":      map[string]any{"login": "alice"},
		"head":      map[string]any{"ref": head, "sha": "abc123", "repo": map[string]any{"id": 5001, "full_name": owner + "/" + repo}},
		"base":      map[string]any{"ref": base, "repo": map[string]any{"id": 5001, "full_name": owner + "/" + repo}},
		"additions": 10, "deletions": 2, "mergeable": true,
	}
}

func (f *fakeGitHub) record(path string) {
	f.mu.Lock()
	f.requests = append(f.requests, path)
	f.mu.Unlock()
}

func (f *fakeGitHub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.record(r.Method + " " + r.URL.Path + "?" + r.URL.RawQuery)
		if r.Header.Get("Authorization") != "Bearer test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		path := r.URL.Path

		if f.limitedOnce {
			// Answer one rate-limited response, then recover.
			f.limitedOnce = false
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.WriteHeader(http.StatusForbidden)
			return
		}

		switch {
		case path == "/user":
			writeJSON(w, f.user)
		case path == "/user/repos":
			writeJSON(w, f.repos)
		case strings.HasPrefix(path, "/repositories/"):
			id := strings.TrimPrefix(path, "/repositories/")
			for _, repo := range f.repos {
				if fmt.Sprint(repo["id"]) == id {
					writeJSON(w, repo)
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
		case strings.HasPrefix(path, "/repos/"):
			rest := strings.TrimPrefix(path, "/repos/")
			parts := strings.SplitN(rest, "/", 3)
			if len(parts) < 2 {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			slug := parts[0] + "/" + parts[1]
			switch {
			case len(parts) == 2:
				for _, repo := range f.repos {
					if repo["full_name"] == slug {
						writeJSON(w, repo)
						return
					}
				}
				w.WriteHeader(http.StatusNotFound)
			case len(parts) == 3 && parts[2] == "pulls" && r.URL.Query().Get("head") != "":
				head := r.URL.Query().Get("head")
				branch := strings.TrimPrefix(head, parts[0]+":")
				var matched []map[string]any
				for _, pull := range f.pulls {
					if pull["state"] == "open" && pull["head"].(map[string]any)["ref"] == branch {
						matched = append(matched, pull)
					}
				}
				writeJSON(w, matched)
			case len(parts) == 3 && parts[2] == "pulls":
				var pulls []map[string]any
				for _, pull := range f.pulls {
					if strings.HasPrefix(fmt.Sprint(pull["html_url"]), "https://github.com/"+slug+"/pull/") {
						pulls = append(pulls, pull)
					}
				}
				writeJSON(w, pulls)
			case strings.HasPrefix(parts[2], "pulls/") && strings.HasSuffix(parts[2], "/reviews"):
				number := strings.TrimSuffix(strings.TrimPrefix(parts[2], "pulls/"), "/reviews")
				writeJSON(w, f.reviews[slug+"#"+number])
			case strings.HasPrefix(parts[2], "pulls/") && strings.HasSuffix(parts[2], "/requested_reviewers"):
				writeJSON(w, map[string]any{})
			case strings.HasPrefix(parts[2], "pulls/") && strings.HasSuffix(parts[2], "/comments"):
				number := strings.TrimSuffix(strings.TrimPrefix(parts[2], "pulls/"), "/comments")
				writeJSON(w, f.comments[slug+"#"+number])
			case strings.HasPrefix(parts[2], "pulls/") && !strings.Contains(strings.TrimPrefix(parts[2], "pulls/"), "/"):
				number := strings.TrimPrefix(parts[2], "pulls/")
				writeJSON(w, f.pulls[slug+"#"+number])
			case strings.HasPrefix(parts[2], "commits/") && strings.HasSuffix(parts[2], "/check-runs"):
				number := ""
				for _, pull := range f.pulls {
					if pull["head"].(map[string]any)["sha"] == "abc123" {
						number = fmt.Sprint(pull["number"])
					}
				}
				writeJSON(w, map[string]any{"total_count": len(f.checks[slug+"#"+number]), "check_runs": f.checks[slug+"#"+number]})
			case strings.HasPrefix(parts[2], "issues/") && strings.HasSuffix(parts[2], "/comments"):
				number := strings.TrimSuffix(strings.TrimPrefix(parts[2], "issues/"), "/comments")
				writeJSON(w, f.comments[slug+"#"+number])
			default:
				if number, ok := parsePullPath(parts[2]); ok {
					pull, found := f.pulls[slug+"#"+number]
					if !found {
						w.WriteHeader(http.StatusNotFound)
						return
					}
					writeJSON(w, pull)
					return
				}
				w.WriteHeader(http.StatusNotFound)
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func parsePullPath(segment string) (string, bool) {
	if !strings.HasPrefix(segment, "pulls/") {
		return "", false
	}
	number := strings.TrimPrefix(segment, "pulls/")
	if number == "" || strings.Contains(number, "/") {
		return "", false
	}
	return number, true
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
	}
}

// ---------------------------------------------------------------------------
// Test harness
// ---------------------------------------------------------------------------

func newTestAdapters(t *testing.T, github *fakeGitHub, host *fakeHost) *githublite.Adapters {
	t.Helper()
	githublite.RateLimitSleepCap = 20 * 1 // 20ms in test
	server := httptest.NewServer(github.handler())
	t.Cleanup(server.Close)

	var resolved pluginsdk.Host = host
	adapters := githublite.NewAdapters(func() pluginsdk.Host { return resolved })
	adapters.SetBaseURLOverride(server.URL)
	return adapters
}

func TestInspectClaimsGitHubURL(t *testing.T) {
	github := newFakeGitHub(t)
	host := newFakeHost(nil)
	adapters := newTestAdapters(t, github, host)

	repository, err := adapters.Inspect(context.Background(), "workspace-1", "https://github.com/acme/widgets")
	require.NoError(t, err)
	require.NotNil(t, repository)
	require.Equal(t, "5001", repository.RepositoryID)
	require.Equal(t, "acme", repository.OwnerOrProject)
	require.Equal(t, "widgets", repository.Name)

	// Not a github.com URL: not owned.
	repository, err = adapters.Inspect(context.Background(), "workspace-1", "https://gitlab.com/acme/widgets")
	require.NoError(t, err)
	require.Nil(t, repository)
}

func TestInspectIncludesPullRequestDetail(t *testing.T) {
	github := newFakeGitHub(t)
	host := newFakeHost(nil)
	adapters := newTestAdapters(t, github, host)
	github.addPull("acme", "widgets", 42, "feat/assets-prefetch", "main")

	// A PR URL: the inspection carries the PR identity plus its head and base
	// branches so the task-create URL flow can preselect the PR's branch.
	repository, err := adapters.Inspect(context.Background(), "workspace-1", "https://github.com/acme/widgets/pull/42")
	require.NoError(t, err)
	require.NotNil(t, repository)
	require.Equal(t, "5001", repository.RepositoryID)
	require.Equal(t, "feat/assets-prefetch", repository.HeadBranch)
	require.Equal(t, "main", repository.BaseBranch)
	require.NotNil(t, repository.PullRequest)
	require.Equal(t, 42, repository.PullRequest.Number)
	require.Equal(t, "PR 42", repository.PullRequest.Title)

	// A plain repository URL stays PR-free: no extra GitHub call is made.
	repository, err = adapters.Inspect(context.Background(), "workspace-1", "https://github.com/acme/widgets")
	require.NoError(t, err)
	require.NotNil(t, repository)
	require.Empty(t, repository.HeadBranch)
	require.Nil(t, repository.PullRequest)

	// A PR URL whose PR is missing: the repository claim stands, only the
	// PR detail is absent.
	repository, err = adapters.Inspect(context.Background(), "workspace-1", "https://github.com/acme/widgets/pull/999")
	require.NoError(t, err)
	require.NotNil(t, repository)
	require.Nil(t, repository.PullRequest)
}

func TestInspectClaimsSSHRemoteURL(t *testing.T) {
	github := newFakeGitHub(t)
	host := newFakeHost(nil)
	adapters := newTestAdapters(t, github, host)

	// The scp-like form `git remote get-url origin` prints, with and without
	// the .git suffix, resolves to the same repository as its HTTPS twin.
	for _, url := range []string{
		"git@github.com:acme/widgets.git",
		"git@github.com:acme/widgets",
		"ssh://git@github.com/acme/widgets.git",
		"ssh://github.com/acme/widgets",
	} {
		repository, err := adapters.Inspect(context.Background(), "workspace-1", url)
		require.NoError(t, err, url)
		require.NotNil(t, repository, url)
		require.Equal(t, "5001", repository.RepositoryID, url)
		require.Equal(t, "acme", repository.OwnerOrProject, url)
		require.Equal(t, "widgets", repository.Name, url)
	}
}

func TestLinkAndForTaskRoundTrip(t *testing.T) {
	github := newFakeGitHub(t)
	github.addPull("acme", "widgets", 42, "feature/thing", "main")
	github.reviews["acme/widgets#42"] = []map[string]any{
		{"id": 1, "user": map[string]any{"login": "bob"}, "state": "APPROVED"},
	}
	github.checks["acme/widgets#42"] = []map[string]any{
		{"id": 10, "name": "ci", "status": "completed", "conclusion": "success", "html_url": "https://github.com/acme/widgets/actions/1"},
	}
	host := newFakeHost(nil)
	adapters := newTestAdapters(t, github, host)
	ctx := githublite.WithWorkspaceID(context.Background(), "workspace-1")

	change, err := adapters.ResolveReference(ctx, "workspace-1", "acme/widgets#42")
	require.NoError(t, err)
	require.Equal(t, int64(42), change.Identity.Number)
	require.Equal(t, "5001", change.Identity.RepositoryID)

	require.NoError(t, adapters.Link(ctx, "task-1", change.Identity))

	// Link is idempotent.
	require.NoError(t, adapters.Link(ctx, "task-1", change.Identity))

	reviews, err := adapters.ForTask(ctx, "workspace-1", "task-1")
	require.NoError(t, err)
	require.Len(t, reviews, 1)
	require.Equal(t, "github-lite", reviews[0].ProviderID)
	require.Equal(t, int64(42), reviews[0].ChangeRequestNumber)
	require.Equal(t, "open", reviews[0].State)
	require.NotNil(t, reviews[0].TaskStatus)
	require.Equal(t, "success", reviews[0].TaskStatus.PipelineState)
	require.Len(t, reviews[0].TaskStatus.Checks, 1)
	require.Equal(t, "approved", reviews[0].Detail["reviewState"])
	require.Equal(t, "feature/thing", reviews[0].Detail["sourceBranch"])

	// Unlink removes it.
	require.NoError(t, adapters.Unlink(ctx, "task-1", change.Identity))
	reviews, err = adapters.ForTask(ctx, "workspace-1", "task-1")
	require.NoError(t, err)
	require.Empty(t, reviews)
}

func TestAutoAttachMatchesCheckoutBranch(t *testing.T) {
	github := newFakeGitHub(t)
	github.addPull("acme", "widgets", 7, "feature/auto", "main")
	host := newFakeHost(nil)
	branch := "main"
	host.repos = []pluginsdk.Repository{{
		ID: "row-1", WorkspaceID: "workspace-1", Name: "widgets",
		ProviderID: "github", ProviderHost: "github.com", OwnerOrProject: "acme",
		RemoteURL: "https://github.com/acme/widgets.git", DefaultBranch: &branch,
	}}
	host.tasks["task-9"] = &pluginsdk.Task{
		ID: "task-9", WorkspaceID: "workspace-1",
		Repositories: []pluginsdk.TaskRepository{{RepositoryID: "row-1", CheckoutBranch: "feature/auto"}},
	}
	adapters := newTestAdapters(t, github, host)
	ctx := githublite.WithWorkspaceID(context.Background(), "workspace-1")

	reviews, err := adapters.ForTask(ctx, "workspace-1", "task-9")
	require.NoError(t, err)
	require.Len(t, reviews, 1)
	require.Equal(t, int64(7), reviews[0].ChangeRequestNumber)
	require.Equal(t, "5001", reviews[0].RepositoryID)
}

func TestRateLimitRetriesOnce(t *testing.T) {
	github := newFakeGitHub(t)
	github.addPull("acme", "widgets", 5, "feature/x", "main")
	github.mu.Lock()
	github.limitedOnce = true
	github.mu.Unlock()
	host := newFakeHost(nil)
	adapters := newTestAdapters(t, github, host)

	change, err := adapters.ResolveReference(context.Background(), "workspace-1", "acme/widgets#5")
	require.NoError(t, err)
	require.Equal(t, int64(5), change.Identity.Number)
}

func TestSearchResolvesExactReferenceAndWorkspacePulls(t *testing.T) {
	github := newFakeGitHub(t)
	github.addPull("acme", "widgets", 11, "feature/a", "main")
	github.addPull("acme", "widgets", 12, "feature/b", "main")
	host := newFakeHost(nil)
	host.repos = []pluginsdk.Repository{{
		ID: "row-1", WorkspaceID: "workspace-1", Name: "widgets",
		RemoteURL: "https://github.com/acme/widgets.git",
	}}
	adapters := newTestAdapters(t, github, host)
	ctx := githublite.WithWorkspaceID(context.Background(), "workspace-1")

	// Exact reference resolves in one fetch.
	candidates, err := adapters.Search(ctx, "workspace-1", "acme/widgets#11", 10)
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	require.Equal(t, "acme/widgets#11", candidates[0].ProviderLocalID)

	// Substring search across workspace repositories (no search API).
	candidates, err = adapters.Search(ctx, "workspace-1", "PR 12", 10)
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	require.Equal(t, "acme/widgets#12", candidates[0].ProviderLocalID)

	// Authorize performs a live check.
	allowed, err := adapters.Authorize(ctx, "workspace-1", "submission", map[string]any{
		"owner": "acme", "repo": "widgets", "number": 11,
	})
	require.NoError(t, err)
	require.True(t, allowed)

	allowed, err = adapters.Authorize(ctx, "workspace-1", "submission", map[string]any{
		"owner": "acme", "repo": "widgets", "number": 99,
	})
	require.NoError(t, err)
	require.False(t, allowed)
}

func TestResolveGitCredentialServesConfiguredToken(t *testing.T) {
	github := newFakeGitHub(t)
	host := newFakeHost(nil)
	adapters := newTestAdapters(t, github, host)

	response, err := adapters.ResolveGitCredential(context.Background(), &pluginsdk.ResolveGitCredentialRequest{
		WorkspaceID: "workspace-1", Host: "github.com", Path: "acme/widgets.git",
	})
	require.NoError(t, err)
	require.NotNil(t, response)
	require.Equal(t, "x-access-token", response.Username)
	// The configured token, transiently (newFakeHost injects test-token).
	require.Equal(t, "test-token", response.Secret)

	// Only github.com is served: another host must be refused, not answered.
	_, err = adapters.ResolveGitCredential(context.Background(), &pluginsdk.ResolveGitCredentialRequest{
		WorkspaceID: "workspace-1", Host: "gitlab.com", Path: "acme/widgets.git",
	})
	require.Error(t, err)

	// The lease binding is non-secret, stable for the same token, and empty
	// (revoked) for a different one.
	binding, err := adapters.GetGitCredentialBinding(context.Background(), &pluginsdk.GitCredentialBindingRequest{
		WorkspaceID: "workspace-1", Host: "github.com", Path: "acme/widgets.git",
	})
	require.NoError(t, err)
	require.NotEmpty(t, binding.Binding)
	require.NotContains(t, binding.Binding, "test-token")
	again, err := adapters.GetGitCredentialBinding(context.Background(), &pluginsdk.GitCredentialBindingRequest{
		WorkspaceID: "workspace-1", Host: "github.com", Path: "acme/widgets.git",
	})
	require.NoError(t, err)
	require.Equal(t, binding.Binding, again.Binding)
}
