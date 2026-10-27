// Auto-fix loop tests: the bounded per-PR loop driven end to end against the
// httptest fake GitHub and the fake Host's session/message surface. Timing
// knobs are shrunk so each loop resolves in milliseconds; the assertions cover
// the prompt shape, round flow, stop, resume-after-stall, and failure paths.
package main

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	recipe "kandev-plugin-github-lite/recipes/source-control/server"
	githublite "kandev-plugin-github-lite/server/githublite"

	"github.com/stretchr/testify/require"
)

// shrinkAutoFixTiming collapses every loop wait to milliseconds and restores
// the production values on cleanup.
func shrinkAutoFixTiming(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		githublite.AutoFixRoundWaitPoll = 10 * time.Second
		githublite.AutoFixRoundWaitCap = 30 * time.Minute
		githublite.AutoFixRoundStartGrace = 2 * time.Minute
		githublite.AutoFixSettlePoll = 60 * time.Second
		githublite.AutoFixSettleCap = 10 * time.Minute
		githublite.AutoFixPendingPoll = 60 * time.Second
		githublite.AutoFixPendingCap = 10 * time.Minute
		githublite.AutoFixSendTimeout = 15 * time.Second
	})
	githublite.AutoFixRoundWaitPoll = 10 * time.Millisecond
	githublite.AutoFixRoundWaitCap = 2 * time.Second
	githublite.AutoFixRoundStartGrace = 300 * time.Millisecond
	githublite.AutoFixSettlePoll = 10 * time.Millisecond
	githublite.AutoFixSettleCap = 400 * time.Millisecond
	githublite.AutoFixPendingPoll = 10 * time.Millisecond
	githublite.AutoFixPendingCap = 400 * time.Millisecond
	githublite.AutoFixSendTimeout = 2 * time.Second
}

// waitAutoFixTerminal polls AutoFixStatus until the loop leaves "running" or
// the timeout hits (the status read costs zero GitHub calls).
func waitAutoFixTerminal(t *testing.T, adapters *githublite.Adapters, taskID string, identity recipe.ChangeRequestIdentity) recipe.AutoFixReport {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		report, _, err := adapters.AutoFixStatus(context.Background(), "workspace-1", taskID, identity)
		require.NoError(t, err)
		if report.Status != "running" && report.Status != "idle" {
			return report
		}
		if time.Now().After(deadline) {
			t.Fatalf("auto-fix loop did not reach a terminal state (last: %s %s)", report.Status, report.Message)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// failingGitHub is the canonical red PR: one failing check, one conflicting
// mergeable_state, and the token owner's review + issue comments.
func failingGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	github := newFakeGitHub(t)
	github.addPull("acme", "widgets", 42, "feature/thing", "main")
	github.setMergeableState("acme", "widgets", 42, "dirty")
	github.checks["acme/widgets#42"] = []map[string]any{
		{"id": 10, "name": "ci", "status": "completed", "conclusion": "failure", "html_url": "https://github.com/acme/widgets/actions/1"},
		{"id": 11, "name": "lint", "status": "completed", "conclusion": "success", "html_url": "https://github.com/acme/widgets/actions/2"},
	}
	github.reviewComments["acme/widgets#42"] = []map[string]any{
		// The token owner's thread comment: actionable.
		{"id": 900, "user": map[string]any{"login": "testuser"}, "body": "please use the helper here", "path": "src/app.ts", "line": 12, "html_url": "https://github.com/acme/widgets/pull/42#discussion_r900", "created_at": "2024-01-01T00:00:00Z", "updated_at": "2024-01-01T00:00:00Z"},
		// Someone else's comment: not this loop's brief.
		{"id": 901, "user": map[string]any{"login": "bob"}, "body": "looks good to me", "path": "src/app.ts", "line": 12, "created_at": "2024-01-01T00:00:00Z", "updated_at": "2024-01-01T00:00:00Z"},
	}
	github.comments["acme/widgets#42"] = []map[string]any{
		{"id": 800, "user": map[string]any{"login": "testuser"}, "body": "also fix the docs link", "created_at": "2024-01-01T00:00:00Z"},
	}
	return github
}

// greenGitHub flips the canonical PR green: head advanced, checks pass,
// conflict resolved, comments addressed.
func greenGitHub(github *fakeGitHub) {
	github.setHeadSHA("acme", "widgets", 42, "def456")
	github.setMergeableState("acme", "widgets", 42, "clean")
	github.checks["acme/widgets#42"] = []map[string]any{
		{"id": 10, "name": "ci", "status": "completed", "conclusion": "success", "html_url": "https://github.com/acme/widgets/actions/1"},
	}
}

func autoFixIdentity() recipe.ChangeRequestIdentity {
	return recipe.ChangeRequestIdentity{ConnectionScope: "acme:widgets", RepositoryID: "5001", Number: 42}
}

// TestAutoFixHappyPath: a red PR (failing check + conflict + the token
// owner's comments) drives one dispatched round whose prompt carries the
// complete self-contained instructions and snapshot; the session finishes,
// the PR goes green, and the loop reports done without a second round.
func TestAutoFixHappyPath(t *testing.T) {
	shrinkAutoFixTiming(t)
	github := failingGitHub(t)
	host := newFakeHost(nil)
	adapters := newTestAdapters(t, github, host)
	identity := autoFixIdentity()

	var pressed atomic.Bool
	host.onSend = func(taskID, sessionID, text string) {
		if pressed.CompareAndSwap(false, true) {
			// The agent's turn: finish shortly after the prompt lands, and
			// the push + fixes land with it.
			time.AfterFunc(30*time.Millisecond, func() {
				greenGitHub(github)
				host.setSessionState(sessionID, "IDLE")
			})
		}
	}

	report, err := adapters.StartAutoFix(context.Background(), "workspace-1", "task-1", identity)
	require.NoError(t, err)
	require.True(t, report.Running)

	final := waitAutoFixTerminal(t, adapters, "task-1", identity)
	require.Equal(t, "done", final.Status, final.Message)
	require.Equal(t, 1, final.Rounds)

	texts := host.sentTexts()
	require.Len(t, texts, 1, "one round = one dispatched prompt")
	prompt := texts[0]
	require.Contains(t, prompt, "@ci-auto-fix — round 1 of 3 on acme/widgets#42")
	// Self-contained instructions (plugin-sent prompts are not host-side
	// mention-expanded, so the guidance must live in the message itself).
	require.Contains(t, prompt, "Instructions:")
	require.Contains(t, prompt, "Do not merge the pull request")
	require.Contains(t, prompt, "Checks still pending are not failures")
	// Snapshot: failing check with its URL, the conflict, the owner's
	// comments — and none of someone else's.
	require.Contains(t, prompt, "ci: failure (https://github.com/acme/widgets/actions/1)")
	require.Contains(t, prompt, "mergeable: dirty (conflicts with the base branch)")
	require.Contains(t, prompt, "src/app.ts:12 please use the helper here")
	require.Contains(t, prompt, "also fix the docs link")
	require.NotContains(t, prompt, "looks good to me")
}

// TestAutoFixNoProgressStopsEarly: if the session works the round but pushes
// nothing and no new feedback arrives, the loop stops instead of re-sending
// the same ask.
func TestAutoFixNoProgressStopsEarly(t *testing.T) {
	shrinkAutoFixTiming(t)
	github := failingGitHub(t)
	host := newFakeHost(nil)
	adapters := newTestAdapters(t, github, host)
	identity := autoFixIdentity()

	host.onSend = func(taskID, sessionID, text string) {
		// The session finishes without fixing anything.
		time.AfterFunc(30*time.Millisecond, func() { host.setSessionState(sessionID, "IDLE") })
	}

	_, err := adapters.StartAutoFix(context.Background(), "workspace-1", "task-1", identity)
	require.NoError(t, err)

	final := waitAutoFixTerminal(t, adapters, "task-1", identity)
	require.Equal(t, "done", final.Status)
	require.Equal(t, 1, final.Rounds, "no-progress stops the loop after the first round")
	require.Contains(t, final.Message, "no progress")
}

// TestAutoFixStopPress: stop cancels a running loop; the terminal state is
// "stopped" and no further prompts go out.
func TestAutoFixStopPress(t *testing.T) {
	shrinkAutoFixTiming(t)
	github := failingGitHub(t)
	host := newFakeHost(nil)
	adapters := newTestAdapters(t, github, host)
	identity := autoFixIdentity()

	stop := make(chan struct{})
	host.onSend = func(taskID, sessionID, text string) {
		// Keep the session running until the stop press lands.
		host.setSessionState(sessionID, "RUNNING")
		go func() {
			<-stop
			host.setSessionState(sessionID, "IDLE")
		}()
	}

	report, err := adapters.StartAutoFix(context.Background(), "workspace-1", "task-1", identity)
	require.NoError(t, err)
	require.True(t, report.Running)

	// Wait for round one to be dispatched before stopping — the stop press
	// must interrupt a loop that is genuinely mid-round.
	deadline := time.Now().Add(2 * time.Second)
	for len(host.sentTexts()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("round one never dispatched")
		}
		time.Sleep(5 * time.Millisecond)
	}

	report, err = adapters.StopAutoFix(context.Background(), "workspace-1", "task-1", identity)
	require.NoError(t, err)
	require.Equal(t, "stopped", report.Status)
	close(stop)

	final := waitAutoFixTerminal(t, adapters, "task-1", identity)
	require.Equal(t, "stopped", final.Status)
	require.Equal(t, 1, final.Rounds)
	require.Contains(t, final.Message, "stopped")

	// Start is idempotent: pressing start on a stopped loop resumes it, and
	// the checkpoint means the previously-seen comments are not re-listed.
	greenGitHub(github)
	host.onSend = func(taskID, sessionID, text string) {
		time.AfterFunc(30*time.Millisecond, func() { host.setSessionState(sessionID, "IDLE") })
	}
	_, err = adapters.StartAutoFix(context.Background(), "workspace-1", "task-1", identity)
	require.NoError(t, err)
	final = waitAutoFixTerminal(t, adapters, "task-1", identity)
	require.Equal(t, "done", final.Status)
	require.Empty(t, final.Rounds, "the resumed loop finds a green PR and dispatches nothing")
}

// TestAutoFixNeedsInputStops: a session that parks waiting for the user
// halts the loop in needs_input instead of burning rounds.
func TestAutoFixNeedsInputStops(t *testing.T) {
	shrinkAutoFixTiming(t)
	github := failingGitHub(t)
	host := newFakeHost(nil)
	adapters := newTestAdapters(t, github, host)
	identity := autoFixIdentity()

	host.onSend = func(taskID, sessionID, text string) {
		time.AfterFunc(30*time.Millisecond, func() { host.setSessionState(sessionID, "WAITING_FOR_INPUT") })
	}

	_, err := adapters.StartAutoFix(context.Background(), "workspace-1", "task-1", identity)
	require.NoError(t, err)

	final := waitAutoFixTerminal(t, adapters, "task-1", identity)
	require.Equal(t, "needs_input", final.Status)
	require.Equal(t, 1, final.Rounds)
	require.Contains(t, final.Message, "waiting for your input")
}

// TestAutoFixFailedSessionErrors: a session that fails while working a round
// surfaces the failure instead of claiming progress.
func TestAutoFixFailedSessionErrors(t *testing.T) {
	shrinkAutoFixTiming(t)
	github := failingGitHub(t)
	host := newFakeHost(nil)
	adapters := newTestAdapters(t, github, host)
	identity := autoFixIdentity()

	host.onSend = func(taskID, sessionID, text string) {
		time.AfterFunc(30*time.Millisecond, func() { host.setSessionState(sessionID, "FAILED") })
	}

	_, err := adapters.StartAutoFix(context.Background(), "workspace-1", "task-1", identity)
	require.NoError(t, err)

	final := waitAutoFixTerminal(t, adapters, "task-1", identity)
	require.Equal(t, "error", final.Status)
	require.Contains(t, final.Message, "FAILED")
}

// TestAutoFixPendingOnlyWaitsThenDone: pending checks are not failures — the
// loop waits a bounded window for CI to settle and finishes clean when it
// does, dispatching nothing.
func TestAutoFixPendingOnlyWaitsThenDone(t *testing.T) {
	shrinkAutoFixTiming(t)
	github := failingGitHub(t)
	github.setMergeableState("acme", "widgets", 42, "clean")
	github.checks["acme/widgets#42"] = []map[string]any{
		{"id": 10, "name": "ci", "status": "in_progress", "conclusion": "", "html_url": "https://github.com/acme/widgets/actions/1"},
	}
	github.reviewComments = nil
	github.comments = nil
	host := newFakeHost(nil)
	adapters := newTestAdapters(t, github, host)
	identity := autoFixIdentity()

	// CI finishes successfully while the pending window runs.
	time.AfterFunc(40*time.Millisecond, func() {
		github.setChecks("acme", "widgets", 42, []map[string]any{
			{"id": 10, "name": "ci", "status": "completed", "conclusion": "success", "html_url": "https://github.com/acme/widgets/actions/1"},
		})
	})

	_, err := adapters.StartAutoFix(context.Background(), "workspace-1", "task-1", identity)
	require.NoError(t, err)

	final := waitAutoFixTerminal(t, adapters, "task-1", identity)
	require.Equal(t, "done", final.Status)
	require.Empty(t, final.Rounds, "nothing actionable was ever dispatched")
	require.Empty(t, host.sentTexts())
}

// TestAutoFixGreenPRRefusesWork: pressing auto-fix on a green PR costs the
// initial reads and dispatches nothing — the loop returns done immediately.
func TestAutoFixGreenPRRefusesWork(t *testing.T) {
	shrinkAutoFixTiming(t)
	github := newFakeGitHub(t)
	github.addPull("acme", "widgets", 42, "feature/thing", "main")
	github.checks["acme/widgets#42"] = []map[string]any{
		{"id": 10, "name": "ci", "status": "completed", "conclusion": "success"},
	}
	host := newFakeHost(nil)
	adapters := newTestAdapters(t, github, host)
	identity := autoFixIdentity()

	_, err := adapters.StartAutoFix(context.Background(), "workspace-1", "task-1", identity)
	require.NoError(t, err)
	final := waitAutoFixTerminal(t, adapters, "task-1", identity)
	require.Equal(t, "done", final.Status)
	require.Empty(t, final.Rounds)
	require.Empty(t, host.sentTexts())
}

// TestAutoFixClosedPRRefusesWork: merged/closed PRs cannot start a loop.
func TestAutoFixClosedPRRefusesWork(t *testing.T) {
	shrinkAutoFixTiming(t)
	github := newFakeGitHub(t)
	github.addPull("acme", "widgets", 42, "feature/thing", "main")
	github.setClosed("acme", "widgets", 42, true)
	host := newFakeHost(nil)
	adapters := newTestAdapters(t, github, host)

	_, err := adapters.StartAutoFix(context.Background(), "workspace-1", "task-1", autoFixIdentity())
	require.Error(t, err)
	require.Contains(t, err.Error(), "merged")
	require.Empty(t, host.sentTexts())
}

// TestAutoFixDisabledByConfig: auto_fix_enabled=false hides the surface —
// start refuses and status reports unsupported.
func TestAutoFixDisabledByConfig(t *testing.T) {
	shrinkAutoFixTiming(t)
	github := failingGitHub(t)
	host := newFakeHost(map[string]any{"github_token": "test-token", "auto_fix_enabled": false})
	adapters := newTestAdapters(t, github, host)
	identity := autoFixIdentity()

	report, found, err := adapters.AutoFixStatus(context.Background(), "workspace-1", "task-1", identity)
	require.NoError(t, err)
	require.False(t, found)
	require.Equal(t, "unsupported", report.Status)

	_, err = adapters.StartAutoFix(context.Background(), "workspace-1", "task-1", identity)
	require.Error(t, err)
	require.Contains(t, err.Error(), "disabled")
	require.Empty(t, host.sentTexts())
}

// TestAutoFixStatusInterruptedAfterPluginRestart: a persisted "running" doc
// with no live loop in this process is reported as interrupted (the loop
// died with the plugin process) — never as a phantom running loop.
func TestAutoFixStatusInterruptedAfterPluginRestart(t *testing.T) {
	shrinkAutoFixTiming(t)
	github := failingGitHub(t)
	host := newFakeHost(nil)
	adapters := newTestAdapters(t, github, host)
	identity := autoFixIdentity()

	stop := make(chan struct{})
	host.onSend = func(taskID, sessionID, text string) {
		host.setSessionState(sessionID, "RUNNING")
		go func() { <-stop }()
	}
	_, err := adapters.StartAutoFix(context.Background(), "workspace-1", "task-1", identity)
	require.NoError(t, err)

	// Simulate the process restart: the loop registry is empty while the
	// persisted state still says running.
	adapters2 := newTestAdapters(t, github, host)
	report, found, err := adapters2.AutoFixStatus(context.Background(), "workspace-1", "task-1", identity)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "interrupted", report.Status)
	require.Contains(t, report.Message, "plugin restarted")
	close(stop)
}

// TestAutoFixSecondRoundListedDeltaOnly: after round one works (but leaves a
// failing check) and pushes, round two re-dispatches and lists only the new
// feedback — the previously-seen comment is not repeated.
func TestAutoFixSecondRoundListedDeltaOnly(t *testing.T) {
	shrinkAutoFixTiming(t)
	github := failingGitHub(t)
	host := newFakeHost(nil)
	adapters := newTestAdapters(t, github, host)
	identity := autoFixIdentity()

	var rounds atomic.Int32
	host.onSend = func(taskID, sessionID, text string) {
		switch rounds.Add(1) {
		case 1:
			// Round one: fixes the conflict, pushes, but the check still fails.
			time.AfterFunc(30*time.Millisecond, func() {
				github.setHeadSHA("acme", "widgets", 42, "def456")
				github.setMergeableState("acme", "widgets", 42, "clean")
				host.setSessionState(sessionID, "IDLE")
			})
		default:
			// Round two: the re-run goes green.
			time.AfterFunc(30*time.Millisecond, func() {
				github.setChecks("acme", "widgets", 42, []map[string]any{
					{"id": 10, "name": "ci", "status": "completed", "conclusion": "success", "html_url": "https://github.com/acme/widgets/actions/1"},
				})
				host.setSessionState(sessionID, "IDLE")
			})
		}
	}

	_, err := adapters.StartAutoFix(context.Background(), "workspace-1", "task-1", identity)
	require.NoError(t, err)

	final := waitAutoFixTerminal(t, adapters, "task-1", identity)
	require.Equal(t, "done", final.Status, final.Message)
	require.Equal(t, 2, final.Rounds)

	texts := host.sentTexts()
	require.Len(t, texts, 2)
	// Round one: the full snapshot — conflict, both comments.
	require.Contains(t, texts[0], "round 1 of 3")
	require.Contains(t, texts[0], "mergeable: dirty (conflicts with the base branch)")
	require.Contains(t, texts[0], "please use the helper here")
	// Round two: only the still-failing check — the seen comment is not
	// re-listed, and the resolved conflict is gone.
	require.Contains(t, texts[1], "round 2 of 3")
	require.Contains(t, texts[1], "ci: failure")
	require.NotContains(t, texts[1], "please use the helper here")
	require.NotContains(t, texts[1], "dirty (conflicts")
}

// TestAutoFixStatusAfterDone: the zero-GitHub status read reports the last
// terminal state (found=true), and a fresh identity reports idle (found=false).
func TestAutoFixStatusAfterDone(t *testing.T) {
	shrinkAutoFixTiming(t)
	github := failingGitHub(t)
	host := newFakeHost(nil)
	adapters := newTestAdapters(t, github, host)
	identity := autoFixIdentity()

	host.onSend = func(taskID, sessionID, text string) {
		time.AfterFunc(30*time.Millisecond, func() { host.setSessionState(sessionID, "IDLE") })
	}
	_, err := adapters.StartAutoFix(context.Background(), "workspace-1", "task-1", identity)
	require.NoError(t, err)
	final := waitAutoFixTerminal(t, adapters, "task-1", identity)
	require.Equal(t, "done", final.Status)

	report, found, err := adapters.AutoFixStatus(context.Background(), "workspace-1", "task-1", identity)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "done", report.Status)
	require.Equal(t, 1, report.Rounds)
	require.True(t, strings.Contains(report.Message, "no progress") || strings.Contains(report.Message, "green"), report.Message)

	// A different PR: no loop ever ran.
	other := recipe.ChangeRequestIdentity{ConnectionScope: "acme:widgets", RepositoryID: "5001", Number: 43}
	report, found, err = adapters.AutoFixStatus(context.Background(), "workspace-1", "task-1", other)
	require.NoError(t, err)
	require.False(t, found)
	require.Equal(t, "idle", report.Status)
}
