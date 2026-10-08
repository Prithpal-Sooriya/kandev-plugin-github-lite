# Changelog

All notable changes to this plugin are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com); versions match
`manifest.yaml` (and the `Makefile`).

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
