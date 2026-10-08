# kandev-plugin-github-lite

A lean GitHub integration for
[kandev](https://github.com/kdlbs/kandev): just the task-oriented GitHub
surface, engineered to keep REST API usage minimal. No dashboards, no issue
browsing, no search API, and — most importantly — **no background poller**.
Every GitHub call happens because you did something, and every response is
TTL-cached so refreshing a task costs nothing.

## What it does

- **Link pull requests to tasks.** A task action ("Link GitHub pull request")
  opens the host's task-link dialog; paste a PR URL or an `owner/repo#123`
  reference. Linked PRs appear on the task's Reviews panel with live status:
  review state (approved / changes requested / pending), CI checks, comments,
  additions/deletions, and lifecycle (draft, merged, closed).
- **Auto-attach by branch.** When a task is connected to a repository checkout
  and has a checked-out branch, any *open* PR whose head matches that branch
  shows up on the task automatically — resolved per refresh, no standing
  loop.
- **Reference pull requests in chat.** Typing an `owner/repo#123` or PR URL
  reference in the composer resolves against GitHub; references fall back to
  listing open PRs across your workspace repositories (via ordinary repo
  listing endpoints, never the search API).
- **Repository pickers.** The plugin registers a `github-lite` repository
  provider so task-create-from-URL and repository connection flows can pick
  GitHub repos. Listing uses `/user/repos` with capped pagination.

## API-frugal by design

| Technique | Effect |
| --- | --- |
| No poller (recipe contract) | Zero calls while you're not looking at a task |
| TTL cache (default 300s, configurable) | Repeated refreshes of the same task reuse responses |
| ~4 REST calls per PR status fetch | PR + reviews + checks + comments, each cached independently |
| No GitHub search API | Search API has a 30 req/min limit regardless of token — never touched |
| Single bounded rate-limit retry (≤10s) | 403/429 answers wait once, briefly, then surface as errors |
| Capped pagination (default 3 pages) | Repository listing can't spiral |
| Provider id `github-lite` | Coexists with the built-in GitHub integration; takes over URLs only when the core one is unauthenticated |

## Setup

### 1. Build the package

Requirements: Go 1.24+, Node 24, and a sibling checkout of kandev at
`../kandev-sdk` (the `replace` in `go.mod` points there):

```sh
git clone https://github.com/kdlbs/kandev ../kandev-sdk
cd ../kandev-sdk && git checkout 570600439036e81f8e9e1c63f15c4abce8a6c846
```

That revision is pinned in `.kandev-sdk-ref` and is the exact SDK contract
this plugin builds against. Then:

```sh
make package        # or package-host for your machine only
```

`dist/` (or `.build/`) now contains
`kandev-plugin-github-lite-<version>.tar.gz`. Verify it:

```sh
make verify-package
```

### 2. Install in kandev

**Settings → Plugins → Install from package**, pick the tarball, then
configure at **Settings → Plugins → GitHub Lite**:

- **GitHub access token** (secret, required) — a classic PAT or fine-grained
  token with read access to your repositories. The token's owner login
  becomes the connection scope.
- **GitHub API URL** (default `https://api.github.com`) — for GitHub Enterprise.
- **Cache TTL seconds** (default `300`) — how long REST responses are reused.
- **Auto-attach by branch** (default on) — open PRs matching a task's
  checkout branch appear on the task without linking.
- **Max list pages** (default `3`) — pagination cap for repository listing.

The plugin requires kandev `0.97.0` or newer (`min_kandev_version` in
`manifest.yaml`).

> **Tip:** if you install this plugin, you can leave the built-in GitHub
> integration unauthenticated — the plugin owns the GitHub surface from
> there, and the built-in poller stops making calls.

## Development

```sh
make test          # backend + recipe + UI tests, package/verifier negative tests
make typecheck-recipes
make fmt vet
make package-host  # host-platform tarball in .build/
```

- `server/githublite/` — the plugin backend: REST client with TTL cache
  (`client.go`), repository/change-request/review/reference adapters, and
  association storage (Host state, keyed per task, immutable identity =
  GitHub's numeric repository ID so renames can't break links).
- `recipes/source-control/` — the source-control recipe, vendored and owned
  here (recipes are copy-and-own by design). One deliberate extension: server
  `ReviewSummary` carries a `Detail` map and `normalizeReview` passes it
  through, so the host's Review panel renders the full PR detail without a
  second fetch.
- `ui/register.ts` → `ui/bundle.js` — the frontend half: registers the
  `github-lite` provider, the link action, and the review provider; maps
  plugin detail documents into the host's `ChangeRequestDetailModel`.
- `tests/` — vitest for the UI half; `server/plugin_test.go` runs the backend
  against an httptest fake GitHub and a fake Host.

### Conventions worth keeping

- **Never call the GitHub search API** — its 30 req/min limit is the exact
  burn this plugin exists to avoid.
- **Add features as pull-based refreshes**, not loops. The recipe contract
  forbids background pollers; the host asks, the plugin answers from cache or
  one bounded burst.
- **Identity is the numeric repository ID** (`GET /repositories/{id}`),
  never `owner/name` — renames keep working.
- **404s and rate-limit errors never unlink associations** — a missing PR is
  reported, not silently forgotten.

## Release

Tag `vX.Y.Z` matching `manifest.yaml`/`Makefile` versions, push the tag, and
attach the packaged tarball to the GitHub release
(`scripts/verify-release-version.sh TAG PACKAGE` checks consistency).
