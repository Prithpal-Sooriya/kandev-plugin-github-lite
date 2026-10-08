package githublite

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	recipe "kandev-plugin-github-lite/recipes/source-control/server"
)

// githubURLPattern matches github.com repository URLs of the shapes kandev's
// URL pickers actually see: owner/repo, owner/repo.git, and anything after
// (tree/<ref>, pull/N, blob path) — the slug is what matters.
var githubURLPattern = regexp.MustCompile(`^(?:https?://)?(?:www\.)?github\.com/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+?)(?:\.git)?(?:/.*)?$`)

// parseGitHubURL extracts owner/repo from a github.com URL or bare slug.
func parseGitHubURL(raw string) (owner, repo string, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", false
	}
	// Prefer the regexp for URL forms; it also handles trailing paths.
	if match := githubURLPattern.FindStringSubmatch(raw); match != nil {
		return match[1], match[2], true
	}
	// Bare "owner/repo" without a host.
	parts := strings.SplitN(raw, "/", 2)
	if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
		return parts[0], parts[1], true
	}
	return "", "", false
}

// repositoryFromAPI maps a GitHub repository payload to the recipe's
// credential-free Repository. RepositoryID is GitHub's immutable numeric
// repository id, so renames never break associations or cursors.
func repositoryFromAPI(repo apiRepository, scope string) recipe.Repository {
	return recipe.Repository{
		ProviderID:      providerID,
		ProviderHost:    providerHost,
		ConnectionScope: scope,
		RepositoryID:    strconv.FormatInt(repo.ID, 10),
		OwnerOrProject:  repo.Owner.Login,
		Name:            repo.Name,
		CloneURL:        repo.CloneURL,
		DefaultBranch:   repo.DefaultBranch,
	}
}

// ConnectionScope resolves the stable scope all responses are bound to: the
// token owner's login (one GET /user per process, cached forever after).
func (a *Adapters) ConnectionScope(ctx context.Context, workspaceID string) (string, error) {
	if err := a.init(ctx); err != nil {
		return "", err
	}
	a.scopeMu.Lock()
	scope := a.scope
	a.scopeMu.Unlock()
	if scope != "" {
		return scope, nil
	}
	var user apiUser
	if err := a.client.getJSON(ctx, "/user", &user); err != nil {
		return "", fmt.Errorf("github-lite: resolving token owner: %w", err)
	}
	if user.Login == "" {
		return "", fmt.Errorf("github-lite: token owner response missing login")
	}
	a.scopeMu.Lock()
	a.scope = user.Login
	a.scopeMu.Unlock()
	return user.Login, nil
}

// List pages repositories visible to the token. GitHub's search API is
// deliberately off-limits here (30 req/min ceiling regardless of token),
// so listing is core REST: GET /user/repos pages, filtered client-side by
// the query. Pages are TTL-cached, so paging through the picker is cheap;
// the page count is capped by config (default 3 pages of 100).
func (a *Adapters) List(ctx context.Context, workspaceID, query string, cursor recipe.RepositoryCursor, limit int) (recipe.RepositoryPage, error) {
	if err := a.init(ctx); err != nil {
		return recipe.RepositoryPage{}, err
	}
	scope, err := a.ConnectionScope(ctx, workspaceID)
	if err != nil {
		return recipe.RepositoryPage{}, err
	}
	if limit <= 0 || limit > 50 {
		limit = 50
	}
	if cursor.Remote != "" && cursor.Remote != providerID {
		return recipe.RepositoryPage{}, fmt.Errorf("github-lite: cursor belongs to %s", cursor.Remote)
	}

	page := 1
	if cursor.AfterRepositoryID != "" {
		parsed, err := strconv.Atoi(cursor.AfterRepositoryID)
		if err != nil || parsed < 1 {
			return recipe.RepositoryPage{}, fmt.Errorf("github-lite: invalid cursor")
		}
		page = parsed
	}
	if page > int(a.config.MaxListPages) {
		return recipe.RepositoryPage{}, fmt.Errorf("github-lite: cursor page %d beyond max %d", page, a.config.MaxListPages)
	}

	path := fmt.Sprintf("/user/repos?per_page=100&page=%d&sort=full_name&affiliation=owner,collaborator,organization_member&visibility=all", page)
	var repos []apiRepository
	if err := a.client.getJSON(ctx, path, &repos); err != nil {
		return recipe.RepositoryPage{}, fmt.Errorf("github-lite: listing repositories: %w", err)
	}

	needle := strings.ToLower(strings.TrimSpace(query))
	matched := make([]recipe.Repository, 0, limit)
	for _, repo := range repos {
		if needle != "" && !strings.Contains(strings.ToLower(repo.Name), needle) &&
			!strings.Contains(strings.ToLower(repo.FullName), needle) {
			continue
		}
		matched = append(matched, repositoryFromAPI(repo, scope))
		if len(matched) >= limit {
			break
		}
	}

	next := recipe.RepositoryCursor{}
	if page < int(a.config.MaxListPages) && len(repos) == 100 {
		next = recipe.RepositoryCursor{Remote: providerID, AfterRepositoryID: strconv.Itoa(page + 1)}
	}
	return recipe.RepositoryPage{Repositories: matched, Next: next}, nil
}

// Inspect claims a github.com URL by fetching the repository. Returns
// (nil, nil) when the URL is not a github.com repository or the repository
// is not visible to the token — that null hands ownership back to the host
// (or to kandev's core integration when it is authenticated).
func (a *Adapters) Inspect(ctx context.Context, workspaceID, rawURL string) (*recipe.Repository, error) {
	if err := a.init(ctx); err != nil {
		return nil, err
	}
	owner, repo, ok := parseGitHubURL(rawURL)
	if !ok {
		return nil, nil
	}
	scope, err := a.ConnectionScope(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	var payload apiRepository
	if err := a.client.getJSON(ctx, "/repos/"+owner+"/"+repo, &payload); err != nil {
		// Not owned: a 404 leaves payload untouched; surface other errors.
		return nil, err
	}
	if payload.ID == 0 {
		return nil, nil // not found or not visible
	}
	inspected := repositoryFromAPI(payload, scope)
	return &inspected, nil
}

// Resolve maps an immutable repository identity back to a full repository
// via GET /repositories/{id} — the rename-proof global endpoint.
func (a *Adapters) Resolve(ctx context.Context, workspaceID string, identity recipe.RepositoryIdentity) (recipe.Repository, error) {
	if err := a.init(ctx); err != nil {
		return recipe.Repository{}, err
	}
	if identity.ConnectionScope == "" || identity.RepositoryID == "" {
		return recipe.Repository{}, fmt.Errorf("github-lite: incomplete repository identity")
	}
	if _, err := strconv.ParseInt(identity.RepositoryID, 10, 64); err != nil {
		return recipe.Repository{}, fmt.Errorf("github-lite: repository id %q is not GitHub's numeric id", identity.RepositoryID)
	}
	var payload apiRepository
	if err := a.client.getJSON(ctx, "/repositories/"+identity.RepositoryID, &payload); err != nil {
		return recipe.Repository{}, fmt.Errorf("github-lite: resolving repository %s: %w", identity.RepositoryID, err)
	}
	if payload.ID == 0 {
		return recipe.Repository{}, fmt.Errorf("github-lite: repository %s not found", identity.RepositoryID)
	}
	return repositoryFromAPI(payload, identity.ConnectionScope), nil
}

// ListBranches returns up to 100 branches, newest-first by GitHub's order.
func (a *Adapters) ListBranches(ctx context.Context, workspaceID string, repository recipe.Repository) ([]recipe.Branch, error) {
	if err := a.init(ctx); err != nil {
		return nil, err
	}
	owner, name, ok := ownerName(repository)
	if !ok {
		// Resolve by immutable id first, then retry with its fresh slugs.
		resolved, err := a.Resolve(ctx, workspaceID, recipe.RepositoryIdentity{
			ConnectionScope: repository.ConnectionScope,
			RepositoryID:    repository.RepositoryID,
		})
		if err != nil {
			return nil, err
		}
		owner, name, ok = ownerName(resolved)
		if !ok {
			return nil, fmt.Errorf("github-lite: repository has no owner/name")
		}
	}
	var branches []apiBranchRef
	if err := a.client.getJSON(ctx, fmt.Sprintf("/repos/%s/%s/branches?per_page=100", owner, name), &branches); err != nil {
		return nil, fmt.Errorf("github-lite: listing branches: %w", err)
	}
	out := make([]recipe.Branch, 0, len(branches))
	for _, branch := range branches {
		isDefault := branch.Name == repository.DefaultBranch
		out = append(out, recipe.Branch{Name: branch.Name, Commit: branch.Commit.SHA, IsDefault: isDefault})
	}
	return out, nil
}

// ownerName extracts owner/repo from a recipe Repository's display slugs.
func ownerName(repository recipe.Repository) (owner, name string, ok bool) {
	if repository.OwnerOrProject == "" || repository.Name == "" {
		return "", "", false
	}
	return repository.OwnerOrProject, repository.Name, true
}
