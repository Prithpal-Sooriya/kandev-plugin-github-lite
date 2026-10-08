package githublite

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	recipe "kandev-plugin-github-lite/recipes/source-control/server"
)

// pullReferencePattern matches the reference shapes the Link dialog and the
// composer accept: a github.com PR URL, owner/repo#N, or a bare #N. A bare #N
// only resolves inside Search when a workspace repository context pins it.
var pullReferencePattern = regexp.MustCompile(`^(?:https?://)?(?:www\.)?github\.com/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)/pull/([0-9]+)/?(?:#.*)?$`)

// parsePullReference parses owner/repo/number out of a PR URL or slug forms:
//   - https://github.com/owner/repo/pull/123
//   - github.com/owner/repo/pull/123
//   - owner/repo#123
func parsePullReference(reference string) (owner, repo string, number int64, ok bool) {
	reference = strings.TrimSpace(reference)
	if match := pullReferencePattern.FindStringSubmatch(reference); match != nil {
		parsed, err := strconv.ParseInt(match[3], 10, 64)
		if err != nil {
			return "", "", 0, false
		}
		return match[1], match[2], parsed, true
	}
	if strings.Contains(reference, "#") {
		parts := strings.SplitN(reference, "#", 2)
		if owner, repo, ok := parseGitHubURL(parts[0]); ok {
			parsed, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
			if err == nil && parsed > 0 {
				return owner, repo, parsed, true
			}
		}
	}
	return "", "", 0, false
}

// fetchPullRequest loads one PR by owner/repo/number. A 404 leaves payload
// zero-valued and returns (nil, nil) — callers decide what absence means.
func (a *Adapters) fetchPullRequest(ctx context.Context, owner, repo string, number int64) (*apiPullRequest, error) {
	var payload apiPullRequest
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, repo, number)
	if err := a.client.getJSON(ctx, path, &payload); err != nil {
		return nil, err
	}
	if payload.Number == 0 {
		return nil, nil
	}
	return &payload, nil
}

// pullRepositoryIdentity resolves the PR's repository as the recipe's
// immutable identity (connection scope + GitHub numeric repository id).
func (a *Adapters) pullRepositoryIdentity(ctx context.Context, pull *apiPullRequest) (string, string, error) {
	if pull == nil || pull.Base.Repo.ID == 0 {
		return "", "", fmt.Errorf("github-lite: pull request repository missing")
	}
	return pull.Base.Repo.FullName, strconv.FormatInt(pull.Base.Repo.ID, 10), nil
}

// ResolveReference maps a pasted PR reference to an immutable ChangeRequest.
func (a *Adapters) ResolveReference(ctx context.Context, workspaceID, reference string) (recipe.ChangeRequest, error) {
	if err := a.init(ctx); err != nil {
		return recipe.ChangeRequest{}, err
	}
	owner, repo, number, ok := parsePullReference(reference)
	if !ok {
		return recipe.ChangeRequest{}, fmt.Errorf("github-lite: %q is not a GitHub pull request reference", reference)
	}
	scope, err := a.ConnectionScope(ctx, workspaceID)
	if err != nil {
		return recipe.ChangeRequest{}, err
	}
	pull, err := a.fetchPullRequest(ctx, owner, repo, number)
	if err != nil {
		return recipe.ChangeRequest{}, fmt.Errorf("github-lite: fetching %s/%s#%d: %w", owner, repo, number, err)
	}
	if pull == nil {
		return recipe.ChangeRequest{}, fmt.Errorf("github-lite: pull request %s/%s#%d not found or not visible to the token", owner, repo, number)
	}
	_, repositoryID, err := a.pullRepositoryIdentity(ctx, pull)
	if err != nil {
		return recipe.ChangeRequest{}, err
	}
	return recipe.ChangeRequest{
		Identity: recipe.ChangeRequestIdentity{
			ConnectionScope: scope,
			RepositoryID:    repositoryID,
			Number:          pull.Number,
		},
		Title: pull.Title,
		URL:   pull.HTMLURL,
	}, nil
}

// Create opens a pull request on the task's attached repository. `source` is
// the task's verified checkout branch (the head); input.Destination is the
// base. This is the only POST to GitHub in the entire plugin.
func (a *Adapters) Create(ctx context.Context, repository recipe.Repository, source string, input recipe.CreateChangeRequestInput) (recipe.ChangeRequest, error) {
	if err := a.init(ctx); err != nil {
		return recipe.ChangeRequest{}, err
	}
	owner, repo, ok := ownerName(repository)
	if !ok {
		// Resolve display slugs through the immutable id first.
		resolved, err := a.Resolve(ctx, "", recipe.RepositoryIdentity{
			ConnectionScope: repository.ConnectionScope,
			RepositoryID:    repository.RepositoryID,
		})
		if err != nil {
			return recipe.ChangeRequest{}, err
		}
		owner, repo, ok = ownerName(resolved)
		if !ok {
			return recipe.ChangeRequest{}, fmt.Errorf("github-lite: repository has no owner/name")
		}
	}
	if strings.TrimSpace(source) == "" {
		return recipe.ChangeRequest{}, fmt.Errorf("github-lite: task has no checkout branch to open a pull request from")
	}
	if strings.TrimSpace(input.Destination) == "" {
		return recipe.ChangeRequest{}, fmt.Errorf("github-lite: destination base branch is empty")
	}

	payload := struct {
		Title string `json:"title"`
		Head  string `json:"head"`
		Base  string `json:"base"`
		Body  string `json:"body,omitempty"`
		Draft bool   `json:"draft"`
	}{
		Title: input.Title,
		Head:  source,
		Base:  input.Destination,
		Body:  input.Description,
		Draft: input.Draft,
	}
	var created apiPullRequest
	path := fmt.Sprintf("/repos/%s/%s/pulls", owner, repo)
	if err := a.client.postJSON(ctx, path, payload, &created); err != nil {
		return recipe.ChangeRequest{}, fmt.Errorf("github-lite: creating pull request on %s/%s: %w", owner, repo, err)
	}
	if created.Number == 0 {
		return recipe.ChangeRequest{}, fmt.Errorf("github-lite: creating pull request on %s/%s: unexpected response", owner, repo)
	}
	return recipe.ChangeRequest{
		Identity: recipe.ChangeRequestIdentity{
			ConnectionScope: repository.ConnectionScope,
			RepositoryID:    repository.RepositoryID,
			Number:          created.Number,
		},
		Title: created.Title,
		URL:   created.HTMLURL,
	}, nil
}
