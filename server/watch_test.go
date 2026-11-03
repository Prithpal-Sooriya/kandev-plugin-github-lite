// Watch-mode tests: the opt-in per-PR babysitter that starts the auto-fix
// loop when a watched PR turns actionable. The sweep is driven directly
// (WatchSweep) so no test waits on a ticker.
package main

import (
	"context"
	"testing"
	"time"

	recipe "kandev-plugin-github-lite/recipes/source-control/server"
	githublite "kandev-plugin-github-lite/server/githublite"

	"github.com/stretchr/testify/require"
)

// linkedWatchSetup links the canonical PR to task-1 (watch drops unlinked PRs).
func linkedWatchSetup(t *testing.T, github *fakeGitHub, config map[string]any) (*githublite.Adapters, *fakeHost, recipe.ChangeRequestIdentity) {
	t.Helper()
	shrinkAutoFixTiming(t)
	host := newFakeHost(config)
	adapters := newTestAdapters(t, github, host)
	identity := autoFixIdentity()
	ctx := githublite.WithWorkspaceID(context.Background(), "workspace-1")
	require.NoError(t, adapters.Link(ctx, "task-1", identity))
	return adapters, host, identity
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func watchReport(t *testing.T, adapters *githublite.Adapters, identity recipe.ChangeRequestIdentity) recipe.AutoFixReport {
	t.Helper()
	report, _, err := adapters.AutoFixStatus(context.Background(), "workspace-1", "task-1", identity)
	require.NoError(t, err)
	return report
}

func idleOnSend(host *fakeHost) {
	host.onSend = func(taskID, sessionID, text string) {
		time.AfterFunc(20*time.Millisecond, func() { host.setSessionState(sessionID, "IDLE") })
	}
}

// A red PR with watch on gets a fix round without anyone pressing the button.
func TestWatchStartsFixOnRedPR(t *testing.T) {
	adapters, host, identity := linkedWatchSetup(t, failingGitHub(t), nil)
	idleOnSend(host)

	report, err := adapters.SetWatch(context.Background(), "workspace-1", "task-1", identity, true)
	require.NoError(t, err)
	require.True(t, report.Watching)
	require.NotZero(t, report.WatchExpiresAt)

	waitFor(t, "watch-started prompt", func() bool { return len(host.sentTexts()) == 1 })
	require.Contains(t, host.sentTexts()[0], "@ci-auto-fix — round 1")
	waitAutoFixTerminal(t, adapters, "task-1", identity)
	require.True(t, watchReport(t, adapters, identity).Watching, "watch outlives the loop")
}

// Green PR: the watch observes, spends no agent turn.
func TestWatchLeavesGreenPRAlone(t *testing.T) {
	github := failingGitHub(t)
	greenGitHub(github)
	github.reviewComments["acme/widgets#42"] = nil
	github.comments["acme/widgets#42"] = nil
	adapters, host, identity := linkedWatchSetup(t, github, nil)

	_, err := adapters.SetWatch(context.Background(), "workspace-1", "task-1", identity, true)
	require.NoError(t, err)
	waitFor(t, "green observation", func() bool {
		return watchReport(t, adapters, identity).WatchMessage == "green and up to date"
	})
	require.Empty(t, host.sentTexts())
}

// After a round that pushed nothing, the same red state does not re-trigger;
// a new commit that is still red does — until the daily budget runs out.
func TestWatchNoProgressGuardAndBudget(t *testing.T) {
	github := failingGitHub(t)
	adapters, host, identity := linkedWatchSetup(t, github, map[string]any{"github_token": "test-token", "auto_fix_watch_daily_runs": 2})
	idleOnSend(host)
	ctx := context.Background()

	_, err := adapters.SetWatch(ctx, "workspace-1", "task-1", identity, true)
	require.NoError(t, err)
	waitFor(t, "first round", func() bool { return len(host.sentTexts()) == 1 })
	waitAutoFixTerminal(t, adapters, "task-1", identity)

	// Nothing pushed, nothing new: guard holds.
	adapters.WatchSweep(ctx)
	require.Len(t, host.sentTexts(), 1)
	require.Contains(t, watchReport(t, adapters, identity).WatchMessage, "waiting")

	// A new (still red) head: second run, spends the last of the budget.
	github.setHeadSHA("acme", "widgets", 42, "def456")
	adapters.WatchSweep(ctx)
	waitFor(t, "second round", func() bool { return len(host.sentTexts()) == 2 })
	waitAutoFixTerminal(t, adapters, "task-1", identity)

	github.setHeadSHA("acme", "widgets", 42, "fed789")
	adapters.WatchSweep(ctx)
	require.Len(t, host.sentTexts(), 2, "budget of 2 reached")
	require.Contains(t, watchReport(t, adapters, identity).WatchMessage, "budget")
}

// Unwatch stops sweeps from acting, and a user's off survives watch-all.
func TestUnwatchStopsAndBeatsWatchAll(t *testing.T) {
	github := failingGitHub(t)
	adapters, host, identity := linkedWatchSetup(t, github, map[string]any{"github_token": "test-token", "auto_fix_watch_all": true})
	ctx := githublite.WithWorkspaceID(context.Background(), "workspace-1")

	_, err := adapters.SetWatch(ctx, "workspace-1", "task-1", identity, false)
	require.NoError(t, err)
	adapters.WatchSweep(ctx)
	_, err = adapters.ForTask(ctx, "workspace-1", "task-1")
	require.NoError(t, err)
	require.False(t, watchReport(t, adapters, identity).Watching, "explicit off is not re-enrolled")
	require.Empty(t, host.sentTexts())
}

// Watch-all enrols a linked open PR the first time a reviews refresh sees it.
func TestWatchAllEnrolsOnRefresh(t *testing.T) {
	github := failingGitHub(t)
	adapters, host, identity := linkedWatchSetup(t, github, map[string]any{"github_token": "test-token", "auto_fix_watch_all": true})
	ctx := githublite.WithWorkspaceID(context.Background(), "workspace-1")
	require.False(t, watchReport(t, adapters, identity).Watching)

	_, err := adapters.ForTask(ctx, "workspace-1", "task-1")
	require.NoError(t, err)
	require.True(t, watchReport(t, adapters, identity).Watching)

	idleOnSend(host)
	adapters.WatchSweep(ctx)
	waitFor(t, "watch-all round", func() bool { return len(host.sentTexts()) == 1 })
	waitAutoFixTerminal(t, adapters, "task-1", identity)
}

// Merged PRs refuse to start a watch; a watch on a PR that later closes ends.
func TestWatchEndsWhenClosed(t *testing.T) {
	github := failingGitHub(t)
	adapters, host, identity := linkedWatchSetup(t, github, nil)
	ctx := context.Background()
	idleOnSend(host)

	_, err := adapters.SetWatch(ctx, "workspace-1", "task-1", identity, true)
	require.NoError(t, err)
	waitFor(t, "first round", func() bool { return len(host.sentTexts()) == 1 })
	waitAutoFixTerminal(t, adapters, "task-1", identity)

	github.setClosed("acme", "widgets", 42, false)
	adapters.WatchSweep(ctx)
	require.False(t, watchReport(t, adapters, identity).Watching)
}
