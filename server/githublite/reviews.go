package githublite

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kandev/kandev/pkg/pluginsdk"

	recipe "kandev-plugin-github-lite/recipes/source-control/server"
)

// prStatus bundles everything one PR read fetches. The PR payload itself is
// cached; the reviews/checks/comments fetches are separate cache entries, so
// a task with two linked PRs visible for ten minutes costs a fixed handful of
// calls, not a stream of them.
type prStatus struct {
	pull      *apiPullRequest
	reviews   []apiReview
	checkRuns []apiCheckRun
	comments  []apiIssueComment
	fetchedAt time.Time
}

// reviewState computes the semantic review state: any pending reviewer makes
// the PR pending; the latest review per reviewer decides; changes_requested
// beats approved; approved wins when nobody requested changes.
func reviewState(reviews []apiReview, requested int) string {
	latest := make(map[string]string)
	for _, review := range reviews {
		switch review.State {
		case "APPROVED", "CHANGES_REQUESTED":
			latest[review.User.Login] = review.State // last one wins per reviewer
		}
	}
	approved, changes := 0, 0
	for _, state := range latest {
		if state == "APPROVED" {
			approved++
		} else {
			changes++
		}
	}
	if changes > 0 {
		return "changes_requested"
	}
	if approved > 0 {
		return "approved"
	}
	if requested > 0 {
		return "pending"
	}
	return "review_requested"
}

// checksState folds check-run conclusions into the four-state pipeline.
func checksState(checkRuns []apiCheckRun) string {
	pending, failed, succeeded := 0, 0, 0
	for _, run := range checkRuns {
		switch {
		case run.Status != "completed":
			pending++
		case run.Conclusion == "success":
			succeeded++
		case run.Conclusion == "failure" || run.Conclusion == "cancelled" ||
			run.Conclusion == "timed_out" || run.Conclusion == "action_required":
			failed++
		default: // neutral / skipped
		}
	}
	if len(checkRuns) == 0 {
		return "neutral"
	}
	if failed > 0 {
		return "failure"
	}
	if pending > 0 {
		return "pending"
	}
	if succeeded > 0 {
		return "success"
	}
	return "neutral"
}

// prLifecycleState maps PR wire state to the four review states the host
// indicator renders: open | merged | closed | draft.
func prLifecycleState(pull *apiPullRequest) string {
	switch {
	case pull.Merged:
		return "merged"
	case pull.State == "closed":
		return "closed"
	case pull.Draft:
		return "draft"
	default:
		return "open"
	}
}

// repoSlugByID resolves the repository numeric id's owner/name (cached via
// GET /repositories/{id}), so head-lookups and detail URLs survive renames.
func (a *Adapters) repoSlugByID(ctx context.Context, repositoryID string) (owner, name string, err error) {
	var payload apiRepository
	if err := a.client.getJSON(ctx, "/repositories/"+repositoryID, &payload); err != nil {
		return "", "", fmt.Errorf("github-lite: resolving repository %s: %w", repositoryID, err)
	}
	if payload.ID == 0 {
		return "", "", fmt.Errorf("github-lite: repository %s not found", repositoryID)
	}
	return payload.Owner.Login, payload.Name, nil
}

// loadPRStatus fetches (through the TTL cache) everything one PR needs:
// the PR, its reviews, its check runs, and its issue comments — 4 calls,
// cached, on interaction only.
func (a *Adapters) loadPRStatus(ctx context.Context, owner, name string, number int64) (*prStatus, error) {
	pull, err := a.fetchPullRequest(ctx, owner, name, number)
	if err != nil {
		return nil, err
	}
	if pull == nil {
		return nil, nil // 404: genuinely absent
	}
	base := fmt.Sprintf("/repos/%s/%s", owner, name)
	var reviews []apiReview
	if err := a.client.getJSON(ctx, fmt.Sprintf("%s/pulls/%d/reviews?per_page=100", base, number), &reviews); err != nil {
		return nil, err
	}
	var checkPayload struct {
		TotalCount int           `json:"total_count"`
		CheckRuns  []apiCheckRun `json:"check_runs"`
	}
	if err := a.client.getJSON(ctx, fmt.Sprintf("%s/commits/%s/check-runs?per_page=100", base, pull.Head.SHA), &checkPayload); err != nil {
		// Checks are supplementary: a checks failure degrades the status chip
		// to neutral, never breaks the whole review read.
		checkPayload = struct {
			TotalCount int           `json:"total_count"`
			CheckRuns  []apiCheckRun `json:"check_runs"`
		}{}
	}
	var comments []apiIssueComment
	if err := a.client.getJSON(ctx, fmt.Sprintf("%s/issues/%d/comments?per_page=100", base, number), &comments); err != nil {
		comments = nil
	}
	return &prStatus{
		pull:      pull,
		reviews:   reviews,
		checkRuns: checkPayload.CheckRuns,
		comments:  comments,
		fetchedAt: time.Now(),
	}, nil
}

// reviewKey is the stable display key for a PR.
func reviewKey(repositoryID string, number int64) string {
	return fmt.Sprintf("%s/%s#%d", providerID, repositoryID, number)
}

// toReviewSummary folds a fully fetched PR into the recipe's ReviewSummary,
// including the detail document the host ChangeRequestDetail panel renders.
func (a *Adapters) toReviewSummary(ctx context.Context, status *prStatus, repositoryID string) recipe.ReviewSummary {
	pull := status.pull
	lifecycle := prLifecycleState(pull)
	pipeline := checksState(status.checkRuns)
	state := reviewState(status.reviews, len(pull.RequestedReviewers))

	checks := make([]recipe.ReviewTaskStatusCheck, 0, len(status.checkRuns))
	for _, run := range status.checkRuns {
		checks = append(checks, recipe.ReviewTaskStatusCheck{
			ID:     strconv.FormatInt(run.ID, 10),
			Label:  run.Name,
			State:  checkRunState(run),
			Detail: run.Conclusion,
			URL:    run.HTMLURL,
		})
	}

	reviewSummary := recipe.ReviewSummary{
		ProviderID:          providerID,
		ReviewKey:           reviewKey(repositoryID, pull.Number),
		Title:               pull.Title,
		URL:                 pull.HTMLURL,
		ConnectionScope:     a.scopeCached(),
		RepositoryID:        repositoryID,
		ChangeRequestNumber: pull.Number,
		State:               lifecycle,
		TaskStatus: &recipe.ReviewTaskStatus{
			Number:        pull.Number,
			State:         lifecycle,
			PipelineState: pipeline,
			Checks:        checks,
		},
		Detail: a.detailDocument(status, repositoryID, lifecycle, state, pipeline),
	}
	return reviewSummary
}

// checkRunState maps one check run to the four-state check model.
func checkRunState(run apiCheckRun) string {
	switch {
	case run.Status != "completed":
		return "pending"
	case run.Conclusion == "success":
		return "success"
	case run.Conclusion == "failure" || run.Conclusion == "cancelled" ||
		run.Conclusion == "timed_out" || run.Conclusion == "action_required":
		return "failure"
	default:
		return "neutral"
	}
}

// scopeCached reads the resolved scope without IO.
func (a *Adapters) scopeCached() string {
	a.scopeMu.Lock()
	defer a.scopeMu.Unlock()
	return a.scope
}

// detailDocument builds the ChangeRequestDetailModel-shaped payload the host
// panel renders (see apps/web/components/integrations/change-request-detail).
func (a *Adapters) detailDocument(status *prStatus, repositoryID string, lifecycle, review, pipeline string) map[string]any {
	pull := status.pull
	person := func(user apiUser) map[string]any {
		return map[string]any{
			"name":      user.Login,
			"url":       user.HTMLURL,
			"avatarUrl": user.Avatar,
		}
	}
	timeString := func(value *time.Time) string {
		if value == nil {
			return ""
		}
		return value.UTC().Format(time.RFC3339)
	}

	detailReviews := make([]map[string]any, 0, len(status.reviews))
	for _, item := range status.reviews {
		detailReviews = append(detailReviews, map[string]any{
			"id":        strconv.FormatInt(item.ID, 10),
			"author":    person(item.User),
			"state":     strings.ToLower(item.State),
			"body":      item.Body,
			"createdAt": timeString(item.SubmittedAt),
		})
	}
	requestedReviewers := make([]map[string]any, 0, len(pull.RequestedReviewers))
	for _, user := range pull.RequestedReviewers {
		requestedReviewers = append(requestedReviewers, person(user))
	}
	detailChecks := make([]map[string]any, 0, len(status.checkRuns))
	for _, run := range status.checkRuns {
		detailChecks = append(detailChecks, map[string]any{
			"id":          strconv.FormatInt(run.ID, 10),
			"name":        run.Name,
			"state":       checkRunState(run),
			"conclusion":  run.Conclusion,
			"url":         run.HTMLURL,
			"startedAt":   timeString(run.StartedAt),
			"completedAt": timeString(run.CompletedAt),
		})
	}
	detailComments := make([]map[string]any, 0, len(status.comments))
	for _, comment := range status.comments {
		detailComments = append(detailComments, map[string]any{
			"id":        strconv.FormatInt(comment.ID, 10),
			"author":    person(comment.User),
			"body":      comment.Body,
			"createdAt": timeString(comment.CreatedAt),
			"url":       comment.HTMLURL,
		})
	}

	return map[string]any{
		"providerId":         providerID,
		"reviewKey":          reviewKey(repositoryID, pull.Number),
		"number":             pull.Number,
		"title":              pull.Title,
		"url":                pull.HTMLURL,
		"state":              lifecycle,
		"draft":              pull.Draft,
		"author":             person(pull.User),
		"createdAt":          timeString(pull.CreatedAt),
		"mergedAt":           timeString(pull.MergedAt),
		"closedAt":           timeString(pull.ClosedAt),
		"sourceBranch":       pull.Head.Ref,
		"targetBranch":       pull.Base.Ref,
		"additions":          pull.Additions,
		"deletions":          pull.Deletions,
		"description":        pull.Body,
		"reviewState":        review,
		"pendingReviewCount": len(pull.RequestedReviewers),
		"reviews":            detailReviews,
		"requestedReviewers": requestedReviewers,
		"checks":             detailChecks,
		"comments":           detailComments,
		"lastSyncedAt":       status.fetchedAt.UTC().Format(time.RFC3339),
	}
}

// ForTask returns the task's pull requests: the linked associations,
// plus (when auto-attach is enabled) open PRs whose head branch equals a
// task repository's checkout branch — each fresh match is persisted as the
// task's association (auto-link), so it survives the refresh that found it.
// All fetches are TTL-cached; dedupe is by immutable repository id + PR
// number.
func (a *Adapters) ForTask(ctx context.Context, workspaceID, taskID string) ([]recipe.ReviewSummary, error) {
	if err := a.init(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(workspaceID) == "" || strings.TrimSpace(taskID) == "" {
		return nil, fmt.Errorf("github-lite: task reviews require workspace and task")
	}

	// Warm the connection scope before any summary is built: every summary's
	// identity carries it, and on a cold process (fresh install, app restart)
	// it has not been resolved yet — a summary without it is rejected by the
	// recipe layer as an incomplete identity, failing the whole refresh. One
	// /user call per process lifetime (per-process cached); a transient
	// failure fails this refresh and the next sweep retries.
	if _, err := a.ConnectionScope(ctx, workspaceID); err != nil {
		return nil, err
	}

	seen := make(map[string]bool)
	summaries := make([]recipe.ReviewSummary, 0)

	// 1. Linked associations first — they survive renames via numeric ids.
	document, err := a.readAssociationDocument(ctx, workspaceID, taskID)
	if err != nil {
		return nil, err
	}
	for _, record := range document.Associations {
		if _, err := strconv.ParseInt(record.RepositoryID, 10, 64); err != nil {
			continue // not a github-lite identity (another provider's row)
		}
		owner, name, err := a.repoSlugByID(ctx, record.RepositoryID)
		if err != nil {
			continue
		}
		status, err := a.loadPRStatus(ctx, owner, name, record.Number)
		if err != nil || status == nil {
			continue
		}
		key := record.RepositoryID + "#" + strconv.FormatInt(record.Number, 10)
		seen[key] = true
		summaries = append(summaries, a.toReviewSummary(ctx, status, record.RepositoryID))
	}

	// 2. Auto-attach: match each task repository's checkout branch to an open
	//    PR head. Host data only (no GitHub calls) to enumerate candidates.
	if a.config.AutoAttach {
		host := a.host()
		if host == nil {
			return summaries, nil
		}
		task, err := host.Tasks().Get(ctx, taskID)
		if err == nil && task != nil {
			rows, err := a.workspaceRepositories(ctx, host, workspaceID)
			if err == nil {
				byID := make(map[string]pluginsdk.Repository, len(rows))
				for _, row := range rows {
					byID[row.ID] = row
				}
				for _, taskRepo := range task.Repositories {
					row, found := byID[taskRepo.RepositoryID]
					if !found || taskRepo.CheckoutBranch == "" {
						continue
					}
					owner, name, ok := parseGitHubURL(row.RemoteURL)
					if !ok {
						owner, name = row.OwnerOrProject, row.Name
						if owner == "" || name == "" {
							continue
						}
					}
					a.attachOpenHeadPRs(ctx, workspaceID, taskID, owner, name, taskRepo.CheckoutBranch, document, seen, &summaries)
				}
			}
		}
	}

	sort.SliceStable(summaries, func(i, j int) bool {
		return summaries[i].ChangeRequestNumber < summaries[j].ChangeRequestNumber
	})
	return summaries, nil
}

// attachOpenHeadPRs finds open PRs whose head is owner/name:branch (one core
// REST call, cached) and appends unseen ones to summaries. Each fresh match
// is also persisted as a task association — the exact durable link a manual
// change_requests.link writes — so a PR an agent pushed and opened lights up
// the task's linked-PR surfaces instead of only appearing transiently in the
// reviews panel. Persistence is idempotent (Link skips already-stored
// identities) and best-effort: a failed write is logged and never fails the
// reviews read. This keeps auto-linking pull-based: it runs inside the same
// reviews refresh the host already triggers on panel mounts and freshness
// sweeps, adds zero GitHub calls beyond the one pulls?head= lookup auto-attach
// already makes, and never touches a task whose PR is not visible to the
// configured token.
func (a *Adapters) attachOpenHeadPRs(ctx context.Context, workspaceID, taskID, owner, name, branch string, document associationDocument, seen map[string]bool, summaries *[]recipe.ReviewSummary) {
	var pulls []apiPullRequest
	path := fmt.Sprintf("/repos/%s/%s/pulls?head=%s:%s&state=open&per_page=10", owner, name, owner, branch)
	if err := a.client.getJSON(ctx, path, &pulls); err != nil {
		return
	}
	for _, pull := range pulls {
		if pull.Number == 0 || pull.Head.Ref != branch {
			continue
		}
		repositoryID := strconv.FormatInt(pull.Base.Repo.ID, 10)
		if repositoryID == "0" {
			continue
		}
		key := repositoryID + "#" + strconv.FormatInt(pull.Number, 10)
		if seen[key] || document.tombstoned(repositoryID, pull.Number) {
			continue
		}
		status, err := a.loadPRStatus(ctx, owner, name, pull.Number)
		if err != nil || status == nil {
			continue
		}
		seen[key] = true
		*summaries = append(*summaries, a.toReviewSummary(ctx, status, repositoryID))

		// Auto-link: persist the match as the task's association (same store
		// as a manual link). The scope is already warm — ForTask resolved it.
		identity := recipe.ChangeRequestIdentity{
			ConnectionScope: a.scopeCached(),
			RepositoryID:    repositoryID,
			Number:         pull.Number,
		}
		if err := a.Link(ctx, taskID, identity); err != nil {
			logger.Warn("auto-link persist failed", "workspace", workspaceID, "task_id", taskID, "pr_number", pull.Number, "error", err.Error())
		} else {
			logger.Info("auto-linked pull request to task", "workspace", workspaceID, "task_id", taskID, "repository_id", repositoryID, "pr_number", pull.Number, "head_branch", branch)
		}
	}
}

// Associations lists every task's linked pull requests in a workspace, for
// workspace-wide indicator surfaces.
func (a *Adapters) Associations(ctx context.Context, workspaceID string) ([]recipe.ReviewAssociation, error) {
	if err := a.init(ctx); err != nil {
		return nil, err
	}
	host := a.host()
	if host == nil {
		return nil, fmt.Errorf("github-lite: Host not injected yet")
	}
	entries, err := host.ListState(ctx, "workspace", workspaceID)
	if err != nil {
		return nil, fmt.Errorf("github-lite: listing workspace state: %w", err)
	}
	out := make([]recipe.ReviewAssociation, 0)
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Key, associationStateKey+":") {
			continue
		}
		records, err := a.recordsFromEntry(entry)
		if err != nil {
			continue
		}
		taskID := strings.TrimPrefix(entry.Key, associationStateKey+":")
		for _, record := range records {
			out = append(out, recipe.ReviewAssociation{
				ProviderID:          providerID,
				TaskID:              taskID,
				ReviewKey:           reviewKey(record.RepositoryID, record.Number),
				ConnectionScope:     record.ConnectionScope,
				RepositoryID:        record.RepositoryID,
				ChangeRequestNumber: record.Number,
			})
		}
	}
	return out, nil
}

// recordsFromEntry decodes one Host state entry's value into association records.
func (a *Adapters) recordsFromEntry(entry pluginsdk.StateEntry) ([]associationRecord, error) {
	encoded, err := json.Marshal(entry.Value)
	if err != nil {
		return nil, err
	}
	var document associationDocument
	if err := json.Unmarshal(encoded, &document); err != nil {
		return nil, err
	}
	return document.Associations, nil
}
