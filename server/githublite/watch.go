package githublite

// Watch mode: opt-in, per-PR babysitting on top of the auto-fix loop.
//
// The press-driven loop stops at green, so a PR that goes red later (or gets
// a review) needs another press. A watch removes the press without becoming
// a general poller: it is a per-PR opt-in with a hard expiry, one shared
// sweep ticker for every watched PR (host-local ListState — no GitHub calls
// when nothing is watched), and each sweep reads a watched PR through the
// same TTL cache every other read uses, so the interval is never shorter
// than the cache TTL (each sweep reads the PR fresh, scoped to its repo). When the delta is actionable the sweep just calls
// StartAutoFix — the same bounded, checkpointed, never-merges loop a press
// would start. Guardrails on top: a daily run budget per PR, a no-progress
// guard (same head SHA and no new feedback → wait), a pause while the
// session needs the user's input, and expiry on merge/close/unlink.
//
// "Watch all" (config) enrols every linked open PR the first time a reviews
// refresh sees it; an explicit unwatch tombstones the PR against re-enrolment.

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	recipe "kandev-plugin-github-lite/recipes/source-control/server"
)

// watchStateKey prefixes one Host state key (workspace scope) per watched PR.
const watchStateKey = "github_lite_watch"

const (
	watchSourceManual = "manual" // user toggled watch on
	watchSourceAll    = "all"    // enrolled by the watch-all setting
	watchSourceOff    = "off"    // user toggled watch off: blocks watch-all re-enrolment
)

// WatchMinInterval floors the sweep interval (a var so tests can shrink it).
var WatchMinInterval = 2 * time.Minute

// watchDocument is the persisted watch state for one (task, PR).
type watchDocument struct {
	TaskID       string `json:"task_id"`
	RepositoryID string `json:"repository_id"`
	Number       int64  `json:"number"`
	Enabled      bool   `json:"enabled"`
	Source       string `json:"source"`
	StartedAt    int64  `json:"started_at"`
	ExpiresAt    int64  `json:"expires_at"`
	LastFixAt    int64  `json:"last_fix_at,omitempty"`
	Day          string `json:"day,omitempty"` // local date the FixesToday counter belongs to
	FixesToday   int    `json:"fixes_today,omitempty"`
	Message      string `json:"message,omitempty"`
}

func watchStateKeyFor(taskID, repositoryID string, number int64) string {
	return watchStateKey + ":" + autoFixRegistryKey(taskID, repositoryID, number)
}

func (d watchDocument) identity() recipe.ChangeRequestIdentity {
	return recipe.ChangeRequestIdentity{RepositoryID: d.RepositoryID, Number: d.Number}
}

func (a *Adapters) readWatch(ctx context.Context, workspaceID, taskID string, identity recipe.ChangeRequestIdentity) (watchDocument, bool, error) {
	host := a.host()
	if host == nil {
		return watchDocument{}, false, fmt.Errorf("github-lite: Host not injected yet")
	}
	value, found, err := host.GetState(ctx, "workspace", workspaceID, watchStateKeyFor(taskID, identity.RepositoryID, identity.Number))
	if err != nil || !found || value == nil {
		return watchDocument{}, false, err
	}
	return decodeWatch(value)
}

func decodeWatch(value any) (watchDocument, bool, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return watchDocument{}, false, err
	}
	var doc watchDocument
	if err := json.Unmarshal(encoded, &doc); err != nil {
		return watchDocument{}, false, err
	}
	return doc, true, nil
}

func (a *Adapters) writeWatch(ctx context.Context, workspaceID string, doc watchDocument) error {
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
	return host.SetState(ctx, "workspace", workspaceID, watchStateKeyFor(doc.TaskID, doc.RepositoryID, doc.Number), value)
}

func (a *Adapters) watchExpiry(from time.Time) int64 {
	hours := a.config.AutoFixWatchHours
	if hours <= 0 {
		hours = 8
	}
	return from.Add(time.Duration(hours) * time.Hour).UnixMilli()
}

// SetWatch turns watch mode on or off for one linked PR and returns the
// refreshed report. Enabling schedules one immediate check of that PR.
func (a *Adapters) SetWatch(ctx context.Context, workspaceID, taskID string, identity recipe.ChangeRequestIdentity, enabled bool) (recipe.AutoFixReport, error) {
	if err := a.init(ctx); err != nil {
		return recipe.AutoFixReport{}, err
	}
	if !a.config.AutoFixEnabled {
		return recipe.AutoFixReport{}, fmt.Errorf("github-lite: the auto-fix loop is disabled in this plugin's settings")
	}
	if err := validateIdentityForAutoFix(workspaceID, taskID, identity); err != nil {
		return recipe.AutoFixReport{}, err
	}
	now := time.Now()
	doc, _, err := a.readWatch(ctx, workspaceID, taskID, identity)
	if err != nil {
		return recipe.AutoFixReport{}, err
	}
	doc.TaskID, doc.RepositoryID, doc.Number = taskID, identity.RepositoryID, identity.Number
	if enabled {
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
			return recipe.AutoFixReport{}, fmt.Errorf("github-lite: pull request %s/%s#%d is already %s — nothing to watch", owner, name, identity.Number, prLifecycleState(pull))
		}
		doc.Enabled, doc.Source = true, watchSourceManual
		doc.StartedAt, doc.ExpiresAt = now.UnixMilli(), a.watchExpiry(now)
		doc.Message = "watching — checking soon"
	} else {
		doc.Enabled, doc.Source = false, watchSourceOff
		doc.Message = "watch stopped"
	}
	if err := a.writeWatch(ctx, workspaceID, doc); err != nil {
		return recipe.AutoFixReport{}, fmt.Errorf("github-lite: persisting watch state: %w", err)
	}
	if enabled {
		a.ensureWatcher(workspaceID)
		go a.watchProcessOne(context.WithoutCancel(ctx), workspaceID, doc)
	}
	report, _, err := a.AutoFixStatus(ctx, workspaceID, taskID, identity)
	return report, err
}

// attachWatch folds the watch document into a report (host state only).
func (a *Adapters) attachWatch(ctx context.Context, workspaceID, taskID string, identity recipe.ChangeRequestIdentity, report recipe.AutoFixReport) recipe.AutoFixReport {
	doc, found, err := a.readWatch(ctx, workspaceID, taskID, identity)
	if err != nil || !found || !doc.Enabled {
		return report
	}
	report.Watching = true
	report.WatchExpiresAt = doc.ExpiresAt
	report.WatchMessage = doc.Message
	return report
}

// enrollWatchAll is the watch-all hook: the reviews refresh calls it for each
// linked open PR. It creates an enabled watch unless one exists (an explicit
// off stays off) and renews its expiry so a PR you keep looking at keeps
// being watched. Host state only — no GitHub calls.
func (a *Adapters) enrollWatchAll(ctx context.Context, workspaceID, taskID string, identity recipe.ChangeRequestIdentity) {
	if !a.config.AutoFixWatchAll || !a.config.AutoFixEnabled {
		return
	}
	doc, found, err := a.readWatch(ctx, workspaceID, taskID, identity)
	if err != nil || (found && doc.Source != watchSourceAll && doc.Source != "") {
		return // manual on/off choices are never overridden
	}
	now := time.Now()
	if found && doc.Enabled && doc.ExpiresAt-now.UnixMilli() > a.watchExpiry(now)-now.UnixMilli()-int64(time.Hour/time.Millisecond) {
		return // renewed within the last hour already
	}
	if !found || !doc.Enabled {
		doc = watchDocument{TaskID: taskID, RepositoryID: identity.RepositoryID, Number: identity.Number, StartedAt: now.UnixMilli(), Message: "watching (watch all)"}
	}
	doc.Enabled, doc.Source, doc.ExpiresAt = true, watchSourceAll, a.watchExpiry(now)
	if err := a.writeWatch(ctx, workspaceID, doc); err != nil {
		logger.Warn("watch-all enrol failed", "task_id", taskID, "error", err.Error())
		return
	}
	a.ensureWatcher(workspaceID)
}

// ensureWatcher records the workspace and starts the single sweep goroutine
// (once per process). After a plugin restart the first action touching a
// workspace re-arms its persisted watches.
func (a *Adapters) ensureWatcher(workspaceID string) {
	a.watchMu.Lock()
	defer a.watchMu.Unlock()
	if a.watchWorkspaces == nil {
		a.watchWorkspaces = map[string]bool{}
	}
	a.watchWorkspaces[workspaceID] = true
	if a.watchStarted {
		return
	}
	a.watchStarted = true
	go a.watchLoop()
}

func (a *Adapters) watchInterval() time.Duration {
	interval := time.Duration(a.config.AutoFixWatchMinutes) * time.Minute
	if ttl := secondsDuration(a.config.CacheTTL); interval < ttl {
		interval = ttl // never re-read faster than the cache serves
	}
	if interval < WatchMinInterval {
		interval = WatchMinInterval
	}
	return interval
}

func (a *Adapters) watchLoop() {
	for {
		time.Sleep(a.watchInterval())
		a.WatchSweep(context.Background())
	}
}

// WatchSweep checks every enabled watch once. Exported for tests; the ticker
// calls it.
func (a *Adapters) WatchSweep(ctx context.Context) {
	if err := a.init(ctx); err != nil {
		return
	}
	host := a.host()
	if host == nil {
		return
	}
	a.watchMu.Lock()
	workspaces := make([]string, 0, len(a.watchWorkspaces))
	for workspaceID := range a.watchWorkspaces {
		workspaces = append(workspaces, workspaceID)
	}
	a.watchMu.Unlock()
	for _, workspaceID := range workspaces {
		entries, err := host.ListState(ctx, "workspace", workspaceID)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Key, watchStateKey+":") {
				continue
			}
			doc, ok, err := decodeWatch(entry.Value)
			if err != nil || !ok || !doc.Enabled {
				continue
			}
			a.watchProcessOne(ctx, workspaceID, doc)
		}
	}
}

// watchProcessOne evaluates one watched PR; serialised per PR.
func (a *Adapters) watchProcessOne(ctx context.Context, workspaceID string, doc watchDocument) {
	key := workspaceID + "|" + autoFixRegistryKey(doc.TaskID, doc.RepositoryID, doc.Number)
	a.watchMu.Lock()
	if a.watchBusy == nil {
		a.watchBusy = map[string]bool{}
	}
	if a.watchBusy[key] {
		a.watchMu.Unlock()
		return
	}
	a.watchBusy[key] = true
	a.watchMu.Unlock()
	defer func() {
		a.watchMu.Lock()
		delete(a.watchBusy, key)
		a.watchMu.Unlock()
	}()

	slug := fmt.Sprintf("%s#%d", doc.RepositoryID, doc.Number)
	update := func(message string, mutate func(*watchDocument)) {
		if mutate == nil && doc.Message == message {
			return
		}
		doc.Message = message
		if mutate != nil {
			mutate(&doc)
		}
		if err := a.writeWatch(ctx, workspaceID, doc); err != nil {
			logger.Warn("watch state persist failed", "task_id", doc.TaskID, "pr", slug, "error", err.Error())
		}
	}
	stop := func(message string) {
		update(message, func(d *watchDocument) { d.Enabled = false })
	}

	if time.Now().UnixMilli() > doc.ExpiresAt {
		stop("watch expired — toggle it on again to keep babysitting")
		return
	}
	// Unlinked PRs fall out of the watch.
	associations, err := a.readAssociationDocument(ctx, workspaceID, doc.TaskID)
	if err == nil {
		linked := false
		for _, record := range associations.Associations {
			if record.RepositoryID == doc.RepositoryID && record.Number == doc.Number {
				linked = true
			}
		}
		if !linked {
			stop("pull request is no longer linked to the task")
			return
		}
	}
	identity := doc.identity()
	if a.activeAutoFixLoop(autoFixRegistryKey(doc.TaskID, doc.RepositoryID, doc.Number)) != nil {
		return // a round is in flight; nothing to decide
	}
	fixDoc, fixFound, err := a.readAutoFixDocument(ctx, workspaceID, doc.TaskID, identity)
	if err != nil {
		return
	}
	if fixFound && fixDoc.Status == autoFixStatusNeedsInput {
		update("paused — the session is waiting for your input", nil)
		return
	}

	owner, name, err := a.repoSlugByID(ctx, doc.RepositoryID)
	if err != nil {
		return
	}
	scope, err := a.ConnectionScope(ctx, workspaceID)
	if err != nil {
		return
	}
	target := autoFixTarget{workspaceID: workspaceID, taskID: doc.TaskID, owner: owner, name: name, number: doc.Number, scope: scope}
	checkpoint := autoFixCheckpoint{}
	if fixFound {
		checkpoint = fixDoc.Checkpoint
	}
	// A watch exists to see changes, so read past the TTL cache — scoped to
	// this repository, once per sweep interval (never shorter than the TTL).
	a.client.forgetPathPrefix("/repos/" + owner + "/" + name + "/")
	delta, err := a.autoFixReadDelta(ctx, target, checkpoint)
	if err != nil {
		update("check failed: "+err.Error()+" — will retry", nil)
		return
	}
	if delta.PullMerged || delta.PullClosed {
		stop("pull request is " + delta.PullLifecycle + " — watch ended")
		return
	}
	if !delta.actionable() {
		if delta.PendingChecks > 0 {
			update("CI running — nothing to fix yet", nil)
		} else {
			update("green and up to date", nil)
		}
		return
	}
	if checkpoint.HeadSHA != "" && delta.HeadSHA == checkpoint.HeadSHA && !delta.feedbackDiffersFromCheckpoint(&checkpoint) {
		update("waiting — the last fix round pushed nothing new and there is no new feedback", nil)
		return
	}
	today := time.Now().Format("2006-01-02")
	if doc.Day != today {
		doc.Day, doc.FixesToday = today, 0
	}
	budget := int(a.config.AutoFixWatchDailyRuns)
	if budget <= 0 {
		budget = 6
	}
	if doc.FixesToday >= budget {
		update("daily auto-fix budget used ("+strconv.Itoa(budget)+") — resumes tomorrow", nil)
		return
	}
	if _, err := a.StartAutoFix(ctx, workspaceID, doc.TaskID, identity); err != nil {
		update("could not start a fix round: "+err.Error(), nil)
		return
	}
	update("fix round started", func(d *watchDocument) {
		d.FixesToday++
		d.LastFixAt = nowMillis()
	})
	logger.Info("watch started auto-fix", "task_id", doc.TaskID, "pr", slug)
}
