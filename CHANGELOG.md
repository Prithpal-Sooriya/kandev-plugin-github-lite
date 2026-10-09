# Changelog

All notable changes to this plugin are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com); versions match
`manifest.yaml` (and the `Makefile`).

## [0.1.4] — 2026-02-04

### Fixed

- Task agents failed to start with "resolve workspace Git credential: resolve
  plugin Git credential: plugin does not implement git credential resolver":
  kandev resolves clone credentials for a repository by asking the plugin that
  owns the repository provider, and fails closed when that plugin does not
  implement the Git credential extension. The plugin now implements it: github.com
  clones authenticate with the configured token (username `x-access-token`,
  secret never logged or persisted, only github.com hosts served), and lease
  validation gets a non-secret token digest whose change (a token rotation
  restarts the plugin) revokes already-issued clone leases.

## [0.1.3] — 2026-02-04

### Fixed

- Task creation from a pasted repository/PR URL failed with "The selected
  repository could not be verified" even though the plugin claimed the URL:
  the recipe's repository descriptor serialized its repository id as
  `repository_id`, but kandev's backend decodes provider inspections with the
  key `provider_repository_id` and rejects the descriptor as invalid when the
  id decodes empty. The descriptor now serializes the host's key (the recipe
  UI accepts the legacy spelling on input), and a host-contract regression
  test pins the full required key set.

## [0.1.2] — 2026-02-04

### Added

- SSH/scp remote URLs (`git@github.com:owner/repo(.git)`,
  `ssh://git@github.com/owner/repo(.git)`) now resolve to the same repository
  as their HTTPS twin. Previously the scp form fell through to the bare-slug
  fallback and mis-parsed `git@github.com:owner` as the owner, so a remote URL
  pasted from `git remote get-url origin` 404'd and task creation failed with
  "The selected repository could not be verified".
- `repositories.inspect` now logs one line per invocation (URL + outcome:
  claimed / unclaimed / not visible to token / error) to stderr, which kandev
  re-emits into its backend log — previously there was no way to see which
  URL an inspect received when a picker or task-create flow misbehaved.

### Fixed

- Ticket PR staleness: a review surface left open (task view, PR status
  strip) never re-fetched, so a PR merged on GitHub kept showing its
  pre-merge state on the ticket indefinitely. The recipe now runs a
  visibility-gated freshness loop that re-refreshes the tasks and workspaces
  the host has actually shown (60s sweep while the window is visible, an
  immediate sweep on focus, 10-minute task / 30-minute workspace windows).
  The plugin server's TTL cache dedupes the GitHub traffic — quota burn is
  unchanged; the loop only decides how quickly a TTL-expired change becomes
  visible.

## [0.1.1] — 2026-02-04

### Fixed

- Manifest declared category "integrations", which kandev's install-time
  manifest validation rejects (`unknown category "integrations"` — the
  allowed enum is connector / automation / tools / analytics / canvas).
  v0.1.0 tarballs could not install at all; categories are now
  `connector, tools`. `scripts/verify-package.sh` gained an allowlist check
  so a bad category can never ship again.

## [0.1.0] — 2026-02-04

## [0.1.0] — 2026-02-04

### Added

- `github-lite` repository provider: claim/inspect/list/branches against
  `https://github.com/...` URLs, powered by `/user/repos` (capped pagination)
  and `GET /repositories/{id}` for rename-proof numeric identity.
- Pull-request change requests: resolve `owner/repo#N` or PR URLs via the
  task-link dialog; link/unlink per task (Host state, idempotent).
- Task review provider: linked PRs render on the task's Reviews panel with
  review state, CI checks (check-runs on the head SHA), comments, and
  lifecycle — fetched as four TTL-cached REST calls on refresh.
- Auto-attach: tasks connected to a repository with a checkout branch pick up
  open PRs whose head matches that branch, per refresh.
- Composer references: exact `owner/repo#N`/URL references resolve live;
  free-text search lists open PRs across workspace repositories.
- Config schema: GitHub access token (secret), API URL, cache TTL, auto-attach
  toggle, max list pages.
