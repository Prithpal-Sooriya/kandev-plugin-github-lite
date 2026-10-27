package githublite

// The auto-fix loop: a bounded, user-triggered multi-round CI fix session for
// one (task, pull request) pair. It mirrors the shape of kandev's built-in
// GitHub CI automation — capped rounds, settle windows, "new or changed"
// feedback deltas, a no-progress stop — while keeping this plugin's ethos
// intact: core REST only, all reads TTL-cached, no standing pollers, and no
// GitHub traffic at all until someone presses the button. The loop itself is
// a bounded goroutine, never a background worker: it exists only between an
// explicit Start press and its terminal state.
//
// The loop's prompt is sent to the task's primary session through
// Host.Messages().Send — the same delivery path the built-in automation uses
// (queue while running, resume-and-prompt when idle). Plugin-sent messages
// are NOT host-side mention-expanded, so the prompt carries its own complete
// instructions rather than relying on "@ci-auto-fix" resolving to kandev's
// saved prompt. The mention stays at the top of every round purely as a
// readable task tag.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kandev/kandev/pkg/pluginsdk"

	recipe "kandev-plugin-github-lite/recipes/source-control/server"
)

// autoFixStateKey is the Host state key prefix (workspace scope) holding one
// auto-fix loop's status document per (task, repository, PR): the UI polls it
// through change_requests.auto_fix_status, which costs zero GitHub calls.
const autoFixStateKey = "github_lite_auto_fix"

// Bounded loop timing. All knobs are vars (not consts) so tests can shrink
// them; the defaults are deliberately generous for slow CI, and every window
// is a cap, not a poll-until-forever.
var (
	// AutoFixRoundWaitPoll / Cap bound the host-local session polling while
	// the agent works on one round (Sessions().List — no GitHub traffic).
	AutoFixRoundWaitPoll = 10 * time.Second
	AutoFixRoundWaitCap  = 30 * time.Minute
	// AutoFixRoundStartGrace is how long a "sent"/"started" dispatch may take
	// to move its session into RUNNING before the round is declared stuck.
	AutoFixRoundStartGrace = 2 * time.Minute
	// AutoFixSettlePoll / Cap bound the GitHub settle polls after a round:
	// the agent pushed, checks re-run — wait (2 calls per tick) until the
	// head SHA advanced and no check is pending, or the cap hits.
	AutoFixSettlePoll = 60 * time.Second
	AutoFixSettleCap  = 10 * time.Minute
	// AutoFixPendingPoll / Cap bound the pending-only recheck: checks are
	// running but nothing is actionable — wait, re-read, and give up.
	AutoFixPendingPoll = 60 * time.Second
	AutoFixPendingCap  = 10 * time.Minute
	// AutoFixSendTimeout bounds one Host.Messages().Send round-trip.
	AutoFixSendTimeout = 15 * time.Second
)

const (
	autoFixStatusRunning     = "running"     // loop alive (working a round)
	autoFixStatusDone        = "done"        // PR green or no actionable feedback
	autoFixStatusStopped     = "stopped"     // user pressed stop
	autoFixStatusNeedsInput  = "needs_input" // agent asked the user something
	autoFixStatusError       = "error"
	autoFixStatusInterrupted = "interrupted" // plugin restarted mid-loop
)

// autoFixDocument is the persisted loop status. Everything the UI shows comes
// from here; live progress lives in this process's registry.
type autoFixDocument struct {
	TaskID       string `json:"task_id"`
	RepositoryID string `json:"repository_id"`
	Number       int64  `json:"number"`
	Status       string `json:"status"`
	Message      string `json:"message,omitempty"`
	Rounds       int    `json:"rounds"`     // rounds dispatched so far
	MaxRounds    int    `json:"max_rounds"` // cap for this run
	SessionID    string `json:"session_id,omitempty"`
	// LastSignature is the feedback signature of the last dispatched round —
	// kept for diagnostics; the no-progress guard is head-SHA based.
	LastSignature string            `json:"last_signature,omitempty"`
	Checkpoint    autoFixCheckpoint `json:"checkpoint"`
	StartedAt     int64             `json:"started_at"` // unix millis
	UpdatedAt     int64             `json:"updated_at"` // unix millis
}

// autoFixCheckpoint is what the loop has already dispatched feedback for.
// Comments (review-thread and issue) authored by the token owner are listed
// only while new or changed since the round that saw them, exactly like the
// built-in automation's delta — a re-press after needs_input/stopped resumes
// from this checkpoint instead of re-listing everything.
type autoFixCheckpoint struct {
	SeenReviewComments map[string]string `json:"seen_review_comments,omitempty"` // id -> updated_at (RFC3339)
	SeenIssueComments  map[string]string `json:"seen_issue_comments,omitempty"`  // id -> updated_at
	FailedChecks       map[string]string `json:"failed_checks,omitempty"`        // name -> conclusion (last dispatched)
	ConflictSeen       bool              `json:"conflict_seen,omitempty"`        // conflict state at last dispatched round
	HeadSHA            string            `json:"head_sha,omitempty"`             // head SHA at last dispatched round
}

// autoFixDelta is one round's actionable feedback, built fresh from GitHub
// against the checkpoint.
type autoFixDelta struct {
	Conflict          bool
	MergeableState    string
	FailedChecks      []apiCheckRun
	PendingChecks     int
	MyReviewComments  []apiReviewComment
	MyIssueComments   []apiIssueComment
	HeadSHA           string
	HeadBranch        string
	BaseBranch        string
	ReviewCommentsAll int // total review comments on the PR (context only)
	PullMerged        bool
	PullClosed        bool
	PullLifecycle     string // open | merged | closed | draft
}

func (d *autoFixDelta) actionable() bool {
	return d.Conflict || len(d.FailedChecks) > 0 || len(d.MyReviewComments) > 0 || len(d.MyIssueComments) > 0
}

// signature fingerprints the delta; used for diagnostics and to keep the
// persisted checkpoint honest, never as the sole stop condition.
func (d *autoFixDelta) signature() string {
	type flatCheck struct{ Name, Conclusion string }
	type flatComment struct {
		ID      string
		Body    string
		Author  string
		Updated string
	}
	checks := make([]flatCheck, 0, len(d.FailedChecks))
	for _, check := range d.FailedChecks {
		checks = append(checks, flatCheck{check.Name, check.Conclusion})
	}
	sort.Slice(checks, func(i, j int) bool { return checks[i].Name < checks[j].Name })
	reviews := make([]flatComment, 0, len(d.MyReviewComments))
	for _, comment := range d.MyReviewComments {
		reviews = append(reviews, flatComment{strconv.FormatInt(comment.ID, 10), comment.Body, comment.User.Login, timestampString(comment.UpdatedAt)})
	}
	sort.Slice(reviews, func(i, j int) bool { return reviews[i].ID < reviews[j].ID })
	issues := make([]flatComment, 0, len(d.MyIssueComments))
	for _, comment := range d.MyIssueComments {
		issues = append(issues, flatComment{strconv.FormatInt(comment.ID, 10), comment.Body, comment.User.Login, timestampString(comment.CreatedAt)})
	}
	sort.Slice(issues, func(i, j int) bool { return issues[i].ID < issues[j].ID })
	payload, _ := json.Marshal(struct {
		Conflict       bool          `json:"conflict"`
		MergeableState string        `json:"mergeable_state"`
		FailedChecks   []flatCheck   `json:"failed_checks"`
		Reviews        []flatComment `json:"reviews"`
		Issues         []flatComment `json:"issues"`
		HeadSHA        string        `json:"head_sha"`
	}{
		Conflict:       d.Conflict,
		MergeableState: d.MergeableState,
		FailedChecks:   checks,
		Reviews:        reviews,
		Issues:         issues,
		HeadSHA:        d.HeadSHA,
	})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// autoFixLoop is the in-process registry entry for one running loop.
type autoFixLoop struct {
	key    string
	cancel context.CancelFunc
	done   chan struct{}

	mu      sync.Mutex
	stopped bool // user pressed stop
	status  autoFixDocument
}

func (l *autoFixLoop) requestStop() {
	l.mu.Lock()
	l.stopped = true
	l.mu.Unlock()
	l.cancel()
}

func (l *autoFixLoop) wasStopped() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.stopped
}

// autoFixRegistryKey is the loop identity: one loop per (task, repo, PR).
func autoFixRegistryKey(taskID, repositoryID string, number int64) string {
	return taskID + ":" + repositoryID + ":" + strconv.FormatInt(number, 10)
}

func autoFixStateKeyFor(taskID, repositoryID string, number int64) string {
	return autoFixStateKey + ":" + autoFixRegistryKey(taskID, repositoryID, number)
}

// autoFixReportFromDoc converts the persisted document into the recipe's
// provider-neutral report.
func autoFixReportFromDoc(providerID string, doc autoFixDocument) recipe.AutoFixReport {
	return recipe.AutoFixReport{
		ProviderID:          providerID,
		TaskID:              doc.TaskID,
		RepositoryID:        doc.RepositoryID,
		ChangeRequestNumber: doc.Number,
		Running:             doc.Status == autoFixStatusRunning,
		Status:              doc.Status,
		Rounds:              doc.Rounds,
		MaxRounds:           doc.MaxRounds,
		Message:             doc.Message,
		UpdatedAt:           doc.UpdatedAt,
	}
}

// StartAutoFix begins (or reports) a bounded auto-fix loop for one linked PR.
// It validates the PR is open, registers the loop, persists the initial
// status, and returns immediately — the rounds run on a goroutine, because a
// plugin action call is capped at 15 seconds and one round's agent wait is
// minutes. Pressing while a loop runs is an idempotent no-op that reports the
// live status.
// activeAutoFixLoop resolves the live loop for a registry key, or nil. A
// loop whose in-memory status is terminal but whose goroutine is still
// winding down is given a bounded moment to exit — so a rapid re-press
// never reports a phantom "running" loop, and never starts a second one
// over the still-registered first.
func (a *Adapters) activeAutoFixLoop(key string) *autoFixLoop {
	for attempt := 0; attempt < 2; attempt++ {
		a.autoFixMu.Lock()
		loop, ok := a.autoFixLoops[key]
		if !ok {
			a.autoFixMu.Unlock()
			return nil
		}
		select {
		case <-loop.done:
			// Stale entry: the goroutine exited but the deferred cleanup has
			// not run yet. Retry the lookup once.
			a.autoFixMu.Unlock()
			time.Sleep(5 * time.Millisecond)
			continue
		default:
		}
		loop.mu.Lock()
		status := loop.status.Status
		loop.mu.Unlock()
		if status != autoFixStatusRunning {
			// Terminal in memory, goroutine still finishing (e.g. the stop
			// press raced the in-flight round). Wait briefly for it.
			a.autoFixMu.Unlock()
			select {
			case <-loop.done:
				time.Sleep(5 * time.Millisecond)
				continue // cleaned up (or about to be): retry once
			case <-time.After(500 * time.Millisecond):
				return loop // report its terminal state; never double-start
			}
		}
		a.autoFixMu.Unlock()
		return loop
	}
	return nil
}

func (a *Adapters) StartAutoFix(ctx context.Context, workspaceID, taskID string, identity recipe.ChangeRequestIdentity) (recipe.AutoFixReport, error) {
	if err := a.init(ctx); err != nil {
		return recipe.AutoFixReport{}, err
	}
	if !a.config.AutoFixEnabled {
		return recipe.AutoFixReport{}, fmt.Errorf("github-lite: the auto-fix loop is disabled in this plugin's settings")
	}
	if err := validateIdentityForAutoFix(workspaceID, taskID, identity); err != nil {
		return recipe.AutoFixReport{}, err
	}

	key := autoFixRegistryKey(taskID, identity.RepositoryID, identity.Number)

	// Already running? Report the live status — an idempotent press, not an
	// error and never a second loop.
	if loop := a.activeAutoFixLoop(key); loop != nil {
		return loopReport(providerID, loop), nil
	}

	// Resolve the PR (one TTL-cached read — the panel that rendered the
	// button almost always warmed it) and refuse hopeless starts up front.
	owner, name, err := a.repoSlugByID(ctx, identity.RepositoryID)
	if err != nil {
		return recipe.AutoFixReport{}, err
	}
	pull, err := a.fetchPullRequest(ctx, owner, name, identity.Number)
	if err != nil {
		return recipe.AutoFixReport{}, err
	}
	if pull == nil {
		return recipe.AutoFixReport{}, fmt.Errorf("github-lite: pull request %s/%s#%d not found", owner, name, identity.Number)
	}
	if pull.Merged || pull.State == "closed" {
		return recipe.AutoFixReport{}, fmt.Errorf("github-lite: pull request %s/%s#%d is already %s — nothing to auto-fix", owner, name, identity.Number, prLifecycleState(pull))
	}

	// Warm the token-owner scope once: "my comments" filtering needs it.
	scope, err := a.ConnectionScope(ctx, workspaceID)
	if err != nil {
		return recipe.AutoFixReport{}, err
	}

	now := nowMillis()
	maxRounds := int(a.config.AutoFixMaxRounds)
	if maxRounds <= 0 {
		maxRounds = 1
	}
	// Resume from any prior checkpoint (needs_input / stopped / interrupted
	// re-presses continue where the last loop left off), but reset run counts.
	doc, found, err := a.readAutoFixDocument(ctx, workspaceID, taskID, identity)
	if err != nil {
		return recipe.AutoFixReport{}, err
	}
	if !found {
		doc = autoFixDocument{Checkpoint: autoFixCheckpoint{}}
	}
	doc.TaskID = taskID
	doc.RepositoryID = identity.RepositoryID
	doc.Number = identity.Number
	doc.Status = autoFixStatusRunning
	doc.Message = ""
	doc.Rounds = 0
	doc.MaxRounds = maxRounds
	doc.SessionID = ""
	doc.StartedAt = now
	doc.UpdatedAt = now

	loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	loop := &autoFixLoop{
		key:    key,
		cancel: cancel,
		done:   make(chan struct{}),
		status: doc,
	}
	a.autoFixMu.Lock()
	a.autoFixLoops[key] = loop
	a.autoFixMu.Unlock()

	if err := a.writeAutoFixDocument(ctx, workspaceID, doc); err != nil {
		cancel()
		a.autoFixMu.Lock()
		delete(a.autoFixLoops, key)
		a.autoFixMu.Unlock()
		return recipe.AutoFixReport{}, fmt.Errorf("github-lite: persisting auto-fix state: %w", err)
	}

	target := autoFixTarget{
		workspaceID: workspaceID,
		taskID:      taskID,
		owner:       owner,
		name:        name,
		number:      identity.Number,
		scope:       scope,
	}
	go a.autoFixRun(loopCtx, loop, target)
	logger.Info("auto-fix loop started", "workspace", workspaceID, "task_id", taskID, "pr", fmt.Sprintf("%s/%s#%d", owner, name, identity.Number), "max_rounds", maxRounds)
	return autoFixReportFromDoc(providerID, doc), nil
}

// StopAutoFix cancels a running loop and marks it stopped. The goroutine
// notices the cancellation on its next tick; Stop persists the terminal
// status itself so the UI is correct immediately.
func (a *Adapters) StopAutoFix(ctx context.Context, workspaceID, taskID string, identity recipe.ChangeRequestIdentity) (recipe.AutoFixReport, error) {
	if err := a.init(ctx); err != nil {
		return recipe.AutoFixReport{}, err
	}
	if err := validateIdentityForAutoFix(workspaceID, taskID, identity); err != nil {
		return recipe.AutoFixReport{}, err
	}
	key := autoFixRegistryKey(taskID, identity.RepositoryID, identity.Number)

	loop := a.activeAutoFixLoop(key)
	if loop == nil {
		// Nothing live: normalize any stale record and report idle/stopped.
		doc, found, err := a.readAutoFixDocument(ctx, workspaceID, taskID, identity)
		if err != nil {
			return recipe.AutoFixReport{}, err
		}
		if !found {
			return idleAutoFixReport(providerID, taskID, identity), nil
		}
		if doc.Status == autoFixStatusRunning {
			// "running" with no live goroutine: interrupted by a restart —
			// but the doc may be stale relative to a just-finished loop, so
			// re-read once before rewriting.
			fresh, reread, err := a.readAutoFixDocument(ctx, workspaceID, taskID, identity)
			if err != nil {
				return recipe.AutoFixReport{}, err
			}
			if reread {
				doc = fresh
			}
			if doc.Status == autoFixStatusRunning {
				doc.Status = autoFixStatusInterrupted
				doc.Message = "loop was not running in this plugin process (interrupted)"
				doc.UpdatedAt = nowMillis()
				_ = a.writeAutoFixDocument(ctx, workspaceID, doc)
			}
		}
		return autoFixReportFromDoc(providerID, doc), nil
	}

	loop.requestStop()
	doc, found, err := a.readAutoFixDocument(ctx, workspaceID, taskID, identity)
	if err != nil || !found {
		if err != nil {
			return recipe.AutoFixReport{}, err
		}
		doc = loop.status
	}
	doc.Status = autoFixStatusStopped
	doc.Message = "stopped by user"
	doc.UpdatedAt = nowMillis()
	if err := a.writeAutoFixDocument(ctx, workspaceID, doc); err != nil {
		return recipe.AutoFixReport{}, fmt.Errorf("github-lite: persisting auto-fix stop: %w", err)
	}
	// Sync the in-memory status too: a rapid re-press between the stop and
	// the goroutine's exit must never report a phantom running loop.
	loop.mu.Lock()
	loop.status = doc
	loop.mu.Unlock()
	return autoFixReportFromDoc(providerID, doc), nil
}

// AutoFixStatus reads the persisted loop status — zero GitHub calls; the UI
// polls this while mounted. A document claiming "running" with no live
// goroutine in this process means the plugin restarted mid-loop: report it
// as interrupted and persist that correction so the panel stops claiming a
// phantom loop.
func (a *Adapters) AutoFixStatus(ctx context.Context, workspaceID, taskID string, identity recipe.ChangeRequestIdentity) (recipe.AutoFixReport, bool, error) {
	if err := a.init(ctx); err != nil {
		return recipe.AutoFixReport{}, false, err
	}
	if err := validateIdentityForAutoFix(workspaceID, taskID, identity); err != nil {
		return recipe.AutoFixReport{}, false, err
	}
	if !a.config.AutoFixEnabled {
		report := idleAutoFixReport(providerID, taskID, identity)
		report.Status = "unsupported"
		report.Message = "the auto-fix loop is disabled in this plugin's settings"
		return report, false, nil
	}
	live := a.activeAutoFixLoop(autoFixRegistryKey(taskID, identity.RepositoryID, identity.Number))
	doc, found, err := a.readAutoFixDocument(ctx, workspaceID, taskID, identity)
	if err != nil {
		return recipe.AutoFixReport{}, false, err
	}
	if !found {
		return idleAutoFixReport(providerID, taskID, identity), false, nil
	}
	if doc.Status == autoFixStatusRunning && live == nil {
		// "running" doc with no live goroutine — but the loop may have
		// finalized between the two reads. Re-read before declaring the
		// loop interrupted; only a fresh "running" doc counts.
		fresh, reread, err := a.readAutoFixDocument(ctx, workspaceID, taskID, identity)
		if err != nil {
			return recipe.AutoFixReport{}, false, err
		}
		if reread && fresh.Status == autoFixStatusRunning {
			fresh.Status = autoFixStatusInterrupted
			fresh.Message = "loop interrupted (plugin restarted) — press auto-fix to resume"
			fresh.UpdatedAt = nowMillis()
			_ = a.writeAutoFixDocument(ctx, workspaceID, fresh)
			doc = fresh
		} else if reread {
			doc = fresh
		}
	}
	return autoFixReportFromDoc(providerID, doc), true, nil
}

func idleAutoFixReport(providerID, taskID string, identity recipe.ChangeRequestIdentity) recipe.AutoFixReport {
	return recipe.AutoFixReport{
		ProviderID:          providerID,
		TaskID:              taskID,
		RepositoryID:        identity.RepositoryID,
		ChangeRequestNumber: identity.Number,
		Running:             false,
		Status:              "idle",
	}
}

func loopReport(providerID string, loop *autoFixLoop) recipe.AutoFixReport {
	loop.mu.Lock()
	defer loop.mu.Unlock()
	return autoFixReportFromDoc(providerID, loop.status)
}

func validateIdentityForAutoFix(workspaceID, taskID string, identity recipe.ChangeRequestIdentity) error {
	if strings.TrimSpace(workspaceID) == "" || strings.TrimSpace(taskID) == "" {
		return fmt.Errorf("github-lite: auto-fix requires workspace and task")
	}
	if strings.TrimSpace(identity.RepositoryID) == "" || identity.Number <= 0 {
		return fmt.Errorf("github-lite: auto-fix requires a complete change-request identity")
	}
	return nil
}

// autoFixTarget is the fixed identity one loop works on.
type autoFixTarget struct {
	workspaceID string
	taskID      string
	owner       string
	name        string
	number      int64
	scope       string // token owner login — "my comments" filter
}

// autoFixRun is the bounded loop goroutine. Every GitHub read it makes is
// paid by the Start press (scoped cache-forget first, so each round sees
// fresh state), every wait is a capped window, and the goroutine always
// terminates — in done, stopped, needs_input, error, or interrupted.
func (a *Adapters) autoFixRun(ctx context.Context, loop *autoFixLoop, target autoFixTarget) {
	defer func() {
		close(loop.done)
		a.autoFixMu.Lock()
		if a.autoFixLoops[loop.key] == loop {
			delete(a.autoFixLoops, loop.key)
		}
		a.autoFixMu.Unlock()
	}()

	slug := target.owner + "/" + target.name
	doc := loop.status

	finalize := func(status, message string) {
		if loop.wasStopped() {
			// StopAutoFix already persisted the terminal doc.
			return
		}
		doc.Status = status
		doc.Message = message
		doc.UpdatedAt = nowMillis()
		loop.mu.Lock()
		loop.status = doc
		loop.mu.Unlock()
		if err := a.writeAutoFixDocument(ctx, target.workspaceID, doc); err != nil {
			logger.Warn("auto-fix final state persist failed", "task_id", target.taskID, "pr", slug+"#"+strconv.FormatInt(target.number, 10), "error", err.Error())
		}
	}
	persist := func() {
		doc.UpdatedAt = nowMillis()
		loop.mu.Lock()
		loop.status = doc
		loop.mu.Unlock()
		if err := a.writeAutoFixDocument(ctx, target.workspaceID, doc); err != nil {
			logger.Warn("auto-fix state persist failed", "task_id", target.taskID, "pr", slug+"#"+strconv.FormatInt(target.number, 10), "error", err.Error())
		}
	}

	for round := 1; round <= doc.MaxRounds; round++ {
		if ctx.Err() != nil {
			finalize(autoFixStatusStopped, "stopped by user")
			return
		}

		// Fresh state for the round: user-paid freshness, scoped to this
		// repository only (the SyncTask pattern).
		a.client.forgetPathPrefix("/repos/" + slug + "/")
		delta, err := a.autoFixReadDelta(ctx, target, doc.Checkpoint)
		if err != nil {
			finalize(autoFixStatusError, "reading pull request state failed: "+err.Error())
			return
		}

		if delta.PullMerged || delta.PullClosed {
			finalize(autoFixStatusDone, "pull request is "+delta.PullLifecycle)
			return
		}

		if !delta.actionable() {
			if delta.PendingChecks > 0 {
				// Pending-only is non-actionable (the built-in prompt's rule):
				// wait a bounded window for checks to finish, re-read, and
				// if still nothing actionable, finish clean.
				settled, waitErr := a.autoFixWaitPending(ctx, target)
				if waitErr != nil {
					finalize(autoFixStatusError, waitErr.Error())
					return
				}
				if ctx.Err() != nil {
					finalize(autoFixStatusStopped, "stopped by user")
					return
				}
				if settled != nil && settled.actionable() {
					delta = settled
				} else {
					if settled == nil {
						finalize(autoFixStatusDone, "checks still pending after the wait window; nothing actionable — press auto-fix again when CI settles")
					} else {
						finalize(autoFixStatusDone, "pull request is green and up to date — nothing to auto-fix")
					}
					return
				}
			} else {
				finalize(autoFixStatusDone, "pull request is green and up to date — nothing to auto-fix")
				return
			}
		}

		// No-progress guard: unless new feedback appeared (new/changed
		// comments, new failing checks) since the last dispatched round, the
		// agent must at least have pushed (head SHA advanced). Nothing moved
		// — stop instead of re-sending the same ask.
		if round > 1 {
			advanced := delta.HeadSHA != "" && delta.HeadSHA != doc.Checkpoint.HeadSHA
			feedbackChanged := delta.feedbackDiffersFromCheckpoint(&doc.Checkpoint)
			if !advanced && !feedbackChanged {
				finalize(autoFixStatusDone, "no progress since the last round — the session pushed nothing and no new feedback arrived")
				return
			}
		}

		prompt := autoFixComposePrompt(target, round, doc.MaxRounds, delta)
		sendCtx, cancelSend := context.WithTimeout(ctx, AutoFixSendTimeout)
		host := a.host()
		if host == nil {
			cancelSend()
			finalize(autoFixStatusError, "Host is not available")
			return
		}
		dispatch, err := host.Messages().Send(sendCtx, target.taskID, "", prompt)
		cancelSend()
		if err != nil {
			finalize(autoFixStatusError, "sending the auto-fix prompt failed: "+err.Error())
			return
		}
		doc.Rounds = round
		doc.SessionID = dispatch.SessionID
		doc.LastSignature = delta.signature()
		doc.Status = autoFixStatusRunning
		doc.Message = "round " + strconv.Itoa(round) + "/" + strconv.Itoa(doc.MaxRounds) + " dispatched — waiting for the session"
		// Fold the dispatched round's observations into the checkpoint at
		// dispatch time — the feedback was sent, so a later stop/resume must
		// not re-list it. The head SHA recorded is the round's start commit.
		doc.Checkpoint = checkpointFromDelta(&doc.Checkpoint, delta)
		persist()

		// Wait for the round's turn to finish (host-local polling only).
		turn, waitErr := a.autoFixWaitRound(ctx, target, dispatch.SessionID)
		if waitErr != nil {
			var typed *autoFixWaitError
			if errors.As(waitErr, &typed) && typed.needsInput {
				finalize(autoFixStatusNeedsInput, "the session is waiting for your input — answer it and press auto-fix again to resume")
			} else if errors.Is(waitErr, context.Canceled) || ctx.Err() != nil {
				finalize(autoFixStatusStopped, "stopped by user")
			} else {
				finalize(autoFixStatusError, waitErr.Error())
			}
			return
		}
		if turn.sessionFailed {
			finalize(autoFixStatusError, "the task session "+turn.sessionState+" while working the round")
			return
		}

		// Settle: the agent pushed; CI re-runs on the new head. Wait (2
		// bounded calls per tick) until the head advanced and checks settled,
		// then the next round classifies its delta against the refreshed
		// checkpoint.
		a.autoFixWaitSettle(ctx, target, delta.HeadSHA)
		persist()

		if ctx.Err() != nil {
			finalize(autoFixStatusStopped, "stopped by user")
			return
		}
	}

	// Round cap reached. The next press (if feedback remains) resumes with a
	// fresh budget and the checkpoint intact.
	message := "round cap reached (" + strconv.Itoa(doc.MaxRounds) + ")"
	a.client.forgetPathPrefix("/repos/" + slug + "/")
	if delta, err := a.autoFixReadDelta(ctx, target, doc.Checkpoint); err == nil && !delta.actionable() {
		message = "pull request is green — round cap reached with nothing left to fix"
	}
	finalize(autoFixStatusDone, message)
}

// autoFixRoundOutcome carries what one agent-round wait observed.
type autoFixRoundOutcome struct {
	sessionState  string
	sessionFailed bool
	turnObserved  bool
}

// autoFixWaitError distinguishes stop-worthy wait failures.
type autoFixWaitError struct {
	err           error
	needsInput    bool
	sessionFailed bool
}

func (e *autoFixWaitError) Error() string {
	if e.err != nil {
		return e.err.Error()
	}
	return "round wait failed"
}

// autoFixWaitRound polls the task's session (host-local — no GitHub calls)
// until the dispatched prompt's turn finishes: RUNNING/STARTING means work
// in flight; IDLE/COMPLETED (after a turn was observed) means done;
// WAITING_FOR_INPUT parks the loop for the user; FAILED/CANCELLED errors.
func (a *Adapters) autoFixWaitRound(ctx context.Context, target autoFixTarget, sessionID string) (*autoFixRoundOutcome, error) {
	deadline := time.Now().Add(AutoFixRoundWaitCap)
	observed := sessionID == "" // unknown target: assume the turn is live
	startDeadline := time.Now().Add(AutoFixRoundStartGrace)
	outcome := &autoFixRoundOutcome{}
	for {
		if ctx.Err() != nil {
			return nil, &autoFixWaitError{err: ctx.Err()}
		}
		session, found, err := a.autoFixFindSession(ctx, target, sessionID)
		if err != nil {
			return nil, &autoFixWaitError{err: err}
		}
		if found {
			outcome.sessionState = session.State
			switch session.State {
			case "RUNNING", "STARTING":
				observed = true
			case "WAITING_FOR_INPUT":
				return nil, &autoFixWaitError{needsInput: true}
			case "FAILED", "CANCELLED":
				outcome.sessionFailed = true
				return outcome, nil
			case "IDLE", "COMPLETED":
				if observed {
					outcome.turnObserved = true
					return outcome, nil
				}
				if time.Now().After(startDeadline) {
					return nil, &autoFixWaitError{err: fmt.Errorf("prompt was dispatched but the session stayed %s", session.State)}
				}
			case "CREATED":
				// Never launched: the messenger should have started it; give
				// it the same grace window before giving up.
				if time.Now().After(startDeadline) {
					return nil, &autoFixWaitError{err: fmt.Errorf("session stayed CREATED after the prompt was dispatched")}
				}
			}
		} else if time.Now().After(startDeadline) {
			return nil, &autoFixWaitError{err: fmt.Errorf("target session not found after the prompt was dispatched")}
		}
		if time.Now().After(deadline) {
			return nil, &autoFixWaitError{err: fmt.Errorf("round exceeded the %s wait window", AutoFixRoundWaitCap)}
		}
		if err := sleepContext(ctx, AutoFixRoundWaitPoll); err != nil {
			return nil, &autoFixWaitError{err: err}
		}
	}
}

// autoFixFindSession locates the target session via Sessions().List with a
// task filter (host-local). An empty sessionID falls back to the task's
// sole/primary session.
func (a *Adapters) autoFixFindSession(ctx context.Context, target autoFixTarget, sessionID string) (*pluginsdk.Session, bool, error) {
	host := a.host()
	if host == nil {
		return nil, false, fmt.Errorf("github-lite: Host not available")
	}
	sessions, _, err := host.Sessions().List(ctx, pluginsdk.SessionFilter{TaskIDs: []string{target.taskID}}, pluginsdk.Page{Limit: 50})
	if err != nil {
		return nil, false, err
	}
	if sessionID != "" {
		for i := range sessions {
			if sessions[i].ID == sessionID {
				return &sessions[i], true, nil
			}
		}
		return nil, false, nil
	}
	if len(sessions) == 0 {
		return nil, false, nil
	}
	// Prefer the most recently started session (primary sessions are listed
	// newest-first by the host).
	return &sessions[0], true, nil
}

// autoFixWaitSettle waits, bounded, for the post-round CI to settle: the
// head SHA advancing past the round's start commit and no check pending.
// Two TTL-bypassed calls per tick; capped at AutoFixSettleCap regardless.
func (a *Adapters) autoFixWaitSettle(ctx context.Context, target autoFixTarget, roundStartSHA string) {
	deadline := time.Now().Add(AutoFixSettleCap)
	for {
		if ctx.Err() != nil {
			return
		}
		if err := sleepContext(ctx, AutoFixSettlePoll); err != nil {
			return
		}
		slug := target.owner + "/" + target.name
		a.client.forgetPathPrefix("/repos/" + slug + "/")
		pull, err := a.fetchPullRequest(ctx, target.owner, target.name, target.number)
		if err != nil || pull == nil {
			return // read trouble: let the next round's classification retry
		}
		advanced := pull.Head.SHA != roundStartSHA
		if !advanced {
			if time.Now().After(deadline) {
				return
			}
			continue
		}
		checks, err := a.fetchCheckRuns(ctx, target.owner, target.name, pull.Head.SHA)
		if err != nil {
			if time.Now().After(deadline) {
				return
			}
			continue
		}
		if countPendingChecks(checks) == 0 || time.Now().After(deadline) {
			return
		}
	}
}

// autoFixWaitPending is the bounded pending-only recheck: checks are running,
// nothing is actionable. Re-reads (scoped forget, 2 calls per tick) until
// something becomes actionable, everything is settled (returns nil delta),
// or the cap hits (returns nil, nil).
func (a *Adapters) autoFixWaitPending(ctx context.Context, target autoFixTarget) (*autoFixDelta, error) {
	deadline := time.Now().Add(AutoFixPendingCap)
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err := sleepContext(ctx, AutoFixPendingPoll); err != nil {
			return nil, err
		}
		slug := target.owner + "/" + target.name
		a.client.forgetPathPrefix("/repos/" + slug + "/")
		status, reviewComments, err := a.autoFixReadStatus(ctx, target)
		if err != nil {
			return nil, err
		}
		pull := status.pull
		if pull == nil {
			return nil, fmt.Errorf("pull request disappeared")
		}
		if pull.Merged || pull.State == "closed" {
			return &autoFixDelta{PullMerged: pull.Merged, PullClosed: pull.State == "closed", PullLifecycle: prLifecycleState(pull)}, nil
		}
		delta := autoFixClassify(status, reviewComments, autoFixCheckpoint{}, target.scope)
		if delta.actionable() {
			return delta, nil
		}
		if delta.PendingChecks == 0 {
			return &autoFixDelta{}, nil
		}
		if time.Now().After(deadline) {
			return nil, nil
		}
	}
}

// fetchCheckRuns reads one commit's check runs through the TTL cache.
func (a *Adapters) fetchCheckRuns(ctx context.Context, owner, name, sha string) ([]apiCheckRun, error) {
	var payload struct {
		TotalCount int           `json:"total_count"`
		CheckRuns  []apiCheckRun `json:"check_runs"`
	}
	path := fmt.Sprintf("/repos/%s/%s/commits/%s/check-runs?per_page=100", owner, name, sha)
	if err := a.client.getJSON(ctx, path, &payload); err != nil {
		return nil, err
	}
	return payload.CheckRuns, nil
}

// autoFixReadStatus loads the PR's full cached status (pull + reviews +
// checks + issue comments — the same four reads a panel render costs) plus
// the review-thread comments (one more cached read).
func (a *Adapters) autoFixReadStatus(ctx context.Context, target autoFixTarget) (*prStatus, []apiReviewComment, error) {
	status, err := a.loadPRStatus(ctx, target.owner, target.name, target.number)
	if err != nil {
		return nil, nil, err
	}
	if status == nil {
		return nil, nil, nil
	}
	var reviewComments []apiReviewComment
	base := fmt.Sprintf("/repos/%s/%s", target.owner, target.name)
	if err := a.client.getJSON(ctx, fmt.Sprintf("%s/pulls/%d/comments?per_page=100", base, target.number), &reviewComments); err != nil {
		reviewComments = nil // supplementary: degrade to issue comments only
	}
	return status, reviewComments, nil
}

// autoFixReadDelta reads fresh state (callers forget the scoped cache
// first) and classifies it against the checkpoint.
func (a *Adapters) autoFixReadDelta(ctx context.Context, target autoFixTarget, checkpoint autoFixCheckpoint) (*autoFixDelta, error) {
	status, reviewComments, err := a.autoFixReadStatus(ctx, target)
	if err != nil {
		return nil, err
	}
	if status == nil || status.pull == nil {
		return nil, fmt.Errorf("pull request %s/%s#%d not found", target.owner, target.name, target.number)
	}
	delta := autoFixClassify(status, reviewComments, checkpoint, target.scope)
	return delta, nil
}

// autoFixClassify folds one PR read into the round's actionable delta.
func autoFixClassify(status *prStatus, reviewComments []apiReviewComment, checkpoint autoFixCheckpoint, scope string) *autoFixDelta {
	pull := status.pull
	delta := &autoFixDelta{
		MergeableState:    pull.MergeableState,
		HeadSHA:           pull.Head.SHA,
		HeadBranch:        pull.Head.Ref,
		BaseBranch:        pull.Base.Ref,
		ReviewCommentsAll: len(reviewComments),
		PullMerged:        pull.Merged,
		PullClosed:        pull.State == "closed",
		PullLifecycle:     prLifecycleState(pull),
	}
	if !pull.Merged && pull.State == "open" && pull.MergeableState == "dirty" {
		delta.Conflict = true
	}
	for _, run := range status.checkRuns {
		switch checkRunState(run) {
		case "failure":
			delta.FailedChecks = append(delta.FailedChecks, run)
		case "pending":
			delta.PendingChecks++
		}
	}
	for _, comment := range reviewComments {
		if scope != "" && !strings.EqualFold(comment.User.Login, scope) {
			continue // only the token owner's comments are this loop's brief
		}
		id := strconv.FormatInt(comment.ID, 10)
		if seen, ok := checkpoint.SeenReviewComments[id]; ok && seen == timestampString(comment.UpdatedAt) {
			continue // already dispatched, unchanged since
		}
		delta.MyReviewComments = append(delta.MyReviewComments, comment)
	}
	for _, comment := range status.comments {
		if scope != "" && !strings.EqualFold(comment.User.Login, scope) {
			continue
		}
		id := strconv.FormatInt(comment.ID, 10)
		if seen, ok := checkpoint.SeenIssueComments[id]; ok && seen == timestampString(comment.CreatedAt) {
			continue
		}
		delta.MyIssueComments = append(delta.MyIssueComments, comment)
	}
	return delta
}

// feedbackDiffersFromCheckpoint reports whether anything in the feedback
// delta is new or changed relative to the checkpoint: new or edited comments,
// a conflict appearing or clearing, or a changed failing-check set.
// (Comment deltas are already folded into MyReviewComments/MyIssueComments
// by autoFixClassify — non-empty means new feedback. A conflict that
// persists across rounds is NOT new feedback — re-sending the same ask is
// the no-progress case.)
func (d *autoFixDelta) feedbackDiffersFromCheckpoint(checkpoint *autoFixCheckpoint) bool {
	if len(d.MyReviewComments) > 0 || len(d.MyIssueComments) > 0 {
		return true
	}
	if d.Conflict != checkpoint.ConflictSeen {
		return true
	}
	if len(d.FailedChecks) != len(checkpoint.FailedChecks) {
		return true
	}
	for _, check := range d.FailedChecks {
		if conclusion, ok := checkpoint.FailedChecks[check.Name]; !ok || conclusion != check.Conclusion {
			return true
		}
	}
	return false
}

// checkpointFromDelta folds a dispatched round's observations into the
// checkpoint the next round classifies against.
func checkpointFromDelta(previous *autoFixCheckpoint, delta *autoFixDelta) autoFixCheckpoint {
	next := autoFixCheckpoint{
		SeenReviewComments: map[string]string{},
		SeenIssueComments:  map[string]string{},
		FailedChecks:       map[string]string{},
		ConflictSeen:       delta.Conflict,
		HeadSHA:            delta.HeadSHA,
	}
	for id, updated := range previous.SeenReviewComments {
		next.SeenReviewComments[id] = updated
	}
	for id, updated := range previous.SeenIssueComments {
		next.SeenIssueComments[id] = updated
	}
	for name, conclusion := range previous.FailedChecks {
		next.FailedChecks[name] = conclusion
	}
	for _, comment := range delta.MyReviewComments {
		next.SeenReviewComments[strconv.FormatInt(comment.ID, 10)] = timestampString(comment.UpdatedAt)
	}
	for _, comment := range delta.MyIssueComments {
		next.SeenIssueComments[strconv.FormatInt(comment.ID, 10)] = timestampString(comment.CreatedAt)
	}
	// The failed-check map mirrors the CURRENT failing set (not a union):
	// checks that now pass leave the map, so a regression counts as new.
	next.FailedChecks = map[string]string{}
	for _, check := range delta.FailedChecks {
		next.FailedChecks[check.Name] = check.Conclusion
	}
	return next
}

// autoFixComposePrompt builds the round's message. Plugin-sent messages are
// not host-side mention-expanded, so the instructions are self-contained;
// "@ci-auto-fix" leads only as a readable task tag. The snapshot sections
// mirror the built-in automation's rendering so the shape is familiar.
func autoFixComposePrompt(target autoFixTarget, round, maxRounds int, delta *autoFixDelta) string {
	var b strings.Builder
	prLabel := fmt.Sprintf("%s/%s#%d", target.owner, target.name, target.number)
	b.WriteString(fmt.Sprintf("@ci-auto-fix — round %d of %d on %s. Keep this pull request green, conflict-free, and responsive to the reviewer feedback below.\n", round, maxRounds, prLabel))

	b.WriteString("\nCurrent pull request state:")
	b.WriteString("\n- state: " + delta.PullLifecycle)
	b.WriteString("; mergeable: " + autoFixMergeableWord(delta))
	if delta.HeadBranch != "" {
		b.WriteString("; head branch: " + delta.HeadBranch)
	}
	if delta.BaseBranch != "" {
		b.WriteString("; base branch: " + delta.BaseBranch)
	}
	if delta.HeadSHA != "" {
		b.WriteString("; head commit: " + delta.HeadSHA)
	}
	b.WriteString("\n")

	if len(delta.FailedChecks) > 0 {
		b.WriteString("\nFailing checks:")
		for _, check := range delta.FailedChecks {
			line := fmt.Sprintf("\n- %s: %s", check.Name, check.Conclusion)
			if check.HTMLURL != "" {
				line += " (" + check.HTMLURL + ")"
			}
			b.WriteString(line)
		}
	}
	if delta.Conflict {
		b.WriteString("\n\nMerge conflict:")
		b.WriteString("\n- the pull request has real conflicts with its base branch — merge the base branch into the head branch and resolve them")
	}
	if len(delta.MyReviewComments) > 0 {
		b.WriteString("\n\nNew or changed review comments from " + target.scope + " to address:")
		for _, comment := range delta.MyReviewComments {
			location := comment.Path
			if comment.Line > 0 {
				location += fmt.Sprintf(":%d", comment.Line)
			}
			b.WriteString(fmt.Sprintf("\n- %s %s", location, strings.TrimSpace(comment.Body)))
		}
	}
	if len(delta.MyIssueComments) > 0 {
		b.WriteString("\n\nNew or changed pull request comments from " + target.scope + " to address:")
		for _, comment := range delta.MyIssueComments {
			b.WriteString(fmt.Sprintf("\n- %s", strings.TrimSpace(comment.Body)))
		}
	}

	b.WriteString(`
Instructions:
1. Fix every failing check listed above. Re-run the failing work locally when possible, and push the fix to the head branch.
2. Resolve the merge conflict with the base branch, if one is listed. Merge the base branch into the head branch and resolve the conflicts — do not close or reopen the pull request.
3. Address each comment listed above: make the requested change, reply to the comment on GitHub if your tools allow it, and resolve the conversation when it is fully addressed.
4. Checks still pending are not failures — do not react to them.
5. Do not merge the pull request. Push fixes to the head branch only.
6. When this round's work is done, reply with a short summary of what you fixed, pushed, and resolved.`)
	return b.String()
}

func autoFixMergeableWord(delta *autoFixDelta) string {
	switch {
	case delta.Conflict:
		return "dirty (conflicts with the base branch)"
	case delta.MergeableState != "":
		return delta.MergeableState
	default:
		return "clean"
	}
}

// readAutoFixDocument loads the persisted loop status (zero when unset).
func (a *Adapters) readAutoFixDocument(ctx context.Context, workspaceID, taskID string, identity recipe.ChangeRequestIdentity) (autoFixDocument, bool, error) {
	host := a.host()
	if host == nil {
		return autoFixDocument{}, false, fmt.Errorf("github-lite: Host not injected yet")
	}
	value, found, err := host.GetState(ctx, "workspace", workspaceID, autoFixStateKeyFor(taskID, identity.RepositoryID, identity.Number))
	if err != nil || !found || value == nil {
		return autoFixDocument{}, false, err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return autoFixDocument{}, false, err
	}
	var doc autoFixDocument
	if err := json.Unmarshal(encoded, &doc); err != nil {
		return autoFixDocument{}, false, err
	}
	return doc, true, nil
}

// writeAutoFixDocument persists the loop status (one host-state key).
func (a *Adapters) writeAutoFixDocument(ctx context.Context, workspaceID string, doc autoFixDocument) error {
	host := a.host()
	if host == nil {
		return fmt.Errorf("github-lite: Host not injected yet")
	}
	encoded, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	var value map[string]any
	if err := json.Unmarshal(encoded, &value); err != nil {
		return err
	}
	return host.SetState(ctx, "workspace", workspaceID, autoFixStateKeyFor(doc.TaskID, doc.RepositoryID, doc.Number), value)
}

func countPendingChecks(runs []apiCheckRun) int {
	pending := 0
	for _, run := range runs {
		if run.Status != "completed" {
			pending++
		}
	}
	return pending
}

func timestampString(value *time.Time) string {
	if value == nil {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

func nowMillis() int64 { return time.Now().UnixMilli() }
