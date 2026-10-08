package githublite

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/kandev/kandev/pkg/pluginsdk"
)

// Search finds pull-request reference candidates for the composer: a PR
// reference typed or pasted directly (resolved with one exact fetch), or
// open PRs across the workspace's GitHub repositories matched by title or
// number substring (bounded, TTL-cached, core REST only — never the search
// API, whose 30 req/min ceiling exists regardless of token type).
func (a *Adapters) Search(ctx context.Context, workspaceID, query string, limit int) ([]pluginsdk.EntityReferenceCandidate, error) {
	if err := a.init(ctx); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 10
	}
	candidates := make([]pluginsdk.EntityReferenceCandidate, 0, limit)

	// Exact PR reference (URL or owner/repo#N): one live fetch.
	if owner, repo, number, ok := parsePullReference(query); ok {
		if pull, err := a.fetchPullRequest(ctx, owner, repo, number); err == nil && pull != nil {
			candidates = append(candidates, pluginsdk.EntityReferenceCandidate{
				ProviderLocalID: fmt.Sprintf("%s/%s#%d", owner, repo, pull.Number),
				Title:           pull.Title,
				URL:             pull.HTMLURL,
				Attributes:      map[string]any{"owner": owner, "repo": repo, "number": pull.Number},
			})
			return candidates, nil
		}
		// Unresolvable reference: fall through to workspace matching so the
		// composer still offers something useful.
	}

	// Workspace repositories are local Host data (no GitHub call), deduped by
	// owner/name, capped so a reference search stays frugal.
	slugSeen := make(map[string]bool)
	slugs := a.workspaceGitHubSlugs(ctx, workspaceID)
	for _, slug := range slugs {
		if slugSeen[slug.owner+"/"+slug.repo] {
			continue
		}
		slugSeen[slug.owner+"/"+slug.repo] = true
		if len(candidates) >= limit {
			break
		}
		candidates = append(candidates, a.searchRepositoryPulls(ctx, workspaceID, slug, query, limit-len(candidates))...)
	}
	return candidates, nil
}

// repositorySlug is a workspace repository's owner/name.
type repositorySlug struct {
	owner string
	repo  string
}

// workspaceGitHubSlugs lists the workspace's github.com repository rows as
// owner/name pairs, reading the local Host repository rows only.
func (a *Adapters) workspaceGitHubSlugs(ctx context.Context, workspaceID string) []repositorySlug {
	host := a.host()
	if host == nil {
		return nil
	}
	rows, err := a.workspaceRepositories(ctx, host, workspaceID)
	if err != nil {
		return nil
	}
	slugs := make([]repositorySlug, 0, len(rows))
	for _, row := range rows {
		owner, repo, ok := parseGitHubURL(row.RemoteURL)
		if !ok {
			if row.OwnerOrProject != "" && row.Name != "" {
				owner, repo = row.OwnerOrProject, row.Name
			} else {
				continue
			}
		}
		slugs = append(slugs, repositorySlug{owner: owner, repo: repo})
	}
	return slugs
}

// searchRepositoryPulls lists one repository's open PRs (cached) and matches
// the query against titles and numbers.
func (a *Adapters) searchRepositoryPulls(ctx context.Context, workspaceID string, slug repositorySlug, query string, limit int) []pluginsdk.EntityReferenceCandidate {
	var pulls []apiPullRequest
	path := fmt.Sprintf("/repos/%s/%s/pulls?state=open&per_page=30&sort=updated&direction=desc", slug.owner, slug.repo)
	if err := a.client.getJSON(ctx, path, &pulls); err != nil {
		return nil
	}
	needle := strings.ToLower(strings.TrimSpace(query))
	matched := make([]pluginsdk.EntityReferenceCandidate, 0, limit)
	for _, pull := range pulls {
		if pull.Number == 0 {
			continue
		}
		if needle != "" &&
			!strings.Contains(strings.ToLower(pull.Title), needle) &&
			!strings.Contains(strconv.FormatInt(pull.Number, 10), needle) {
			continue
		}
		matched = append(matched, pluginsdk.EntityReferenceCandidate{
			ProviderLocalID: fmt.Sprintf("%s/%s#%d", slug.owner, slug.repo, pull.Number),
			Title:           pull.Title,
			URL:             pull.HTMLURL,
			Attributes:      map[string]any{"owner": slug.owner, "repo": slug.repo, "number": pull.Number},
		})
		if len(matched) >= limit {
			break
		}
	}
	return matched
}

// Authorize live-checks one candidate reference. Search results are never
// trusted as authorization — this performs a fresh exact fetch.
func (a *Adapters) Authorize(ctx context.Context, workspaceID, purpose string, reference map[string]any) (bool, error) {
	if err := a.init(ctx); err != nil {
		return false, err
	}
	if purpose != "search" && purpose != "submission" {
		return false, fmt.Errorf("github-lite: unknown reference purpose %q", purpose)
	}
	owner, ownerOK := reference["owner"].(string)
	repo, repoOK := reference["repo"].(string)
	if !ownerOK || !repoOK || owner == "" || repo == "" {
		return false, nil
	}
	number, ok := toInt(reference["number"])
	if !ok || number <= 0 {
		return false, nil
	}
	pull, err := a.fetchPullRequest(ctx, owner, repo, number)
	if err != nil || pull == nil {
		return false, nil // not authorized when unavailable or errored
	}
	return true, nil
}
