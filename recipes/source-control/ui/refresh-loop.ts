/**
 * Visibility-gated freshness loop for the source-control recipe.
 *
 * The host refreshes review/association snapshots only when one of its
 * components mounts (task open, navigation, panel mount). A surface that
 * stays open never re-fetches, so a PR merged on the forge keeps showing its
 * pre-merge state on the ticket indefinitely. This loop closes that gap
 * without reintroducing a poller's cost: every sweep re-invokes the same
 * pull-based actions the host already uses, and the plugin server's TTL cache
 * dedupes the resulting GitHub calls — refresh cadence here never multiplies
 * upstream quota, it only decides how quickly a TTL-expired change becomes
 * visible.
 *
 * Scheduling rules:
 * - A `setInterval` sweep every `intervalMs`, skipped while the document is
 *   hidden; visibility flips to visible trigger an immediate sweep, guarded
 *   by a small floor so focus thrash cannot burst requests.
 * - Only ids the host has actually refreshed recently are swept again: tasks
 *   within `taskWindowMs`, workspaces within `workspaceWindowMs`. Nothing the
 *   user has not looked at is ever touched.
 * - One sweep owns one AbortController; a new sweep aborts the previous one,
 *   and stop() aborts whatever is in flight. Errors are swallowed — the next
 *   sweep retries, and the host's own mount-driven refreshes still surface
 *   failures through the UI.
 */

export type FreshnessLoopDeps = {
  refreshTask: (taskId: string, workspaceId: string | undefined, signal: AbortSignal) => void;
  refreshWorkspace: (workspaceId: string, signal: AbortSignal) => void;
  isDocumentVisible?: () => boolean;
  intervalMs?: number;
  minSweepGapMs?: number;
  taskWindowMs?: number;
  workspaceWindowMs?: number;
};

export type FreshnessRecorder = {
  noteTask(taskId: string, workspaceId: string | undefined, now?: number): void;
  noteWorkspace(workspaceId: string, now?: number): void;
};

export type FreshnessLoop = FreshnessRecorder & {
  stop(): void;
};

const DEFAULT_INTERVAL_MS = 60_000;
const DEFAULT_MIN_SWEEP_GAP_MS = 15_000;
const DEFAULT_TASK_WINDOW_MS = 10 * 60_000;
const DEFAULT_WORKSPACE_WINDOW_MS = 30 * 60_000;
const MAX_TRACKED_IDS = 64;

export function startFreshnessLoop(deps: FreshnessLoopDeps): FreshnessLoop {
  const intervalMs = deps.intervalMs ?? DEFAULT_INTERVAL_MS;
  const minSweepGapMs = deps.minSweepGapMs ?? DEFAULT_MIN_SWEEP_GAP_MS;
  const taskWindowMs = deps.taskWindowMs ?? DEFAULT_TASK_WINDOW_MS;
  const workspaceWindowMs = deps.workspaceWindowMs ?? DEFAULT_WORKSPACE_WINDOW_MS;
  const isDocumentVisible = deps.isDocumentVisible ?? (() =>
    typeof document === "undefined" ? true : document.visibilityState === "visible");

  const tasks = new Map<string, { workspaceId: string | undefined; at: number }>();
  const workspaces = new Map<string, number>();
  let inFlight: AbortController | undefined;
  let lastSweepAt: number | undefined;
  let stopped = false;

  const prune = (now: number) => {
    for (const [taskId, entry] of tasks) {
      if (now - entry.at > taskWindowMs) tasks.delete(taskId);
    }
    for (const [workspaceId, at] of workspaces) {
      if (now - at > workspaceWindowMs) workspaces.delete(workspaceId);
    }
  };

  const sweep = () => {
    if (stopped) return;
    const now = Date.now();
    if (lastSweepAt !== undefined && now - lastSweepAt < minSweepGapMs) return;
    if (!isDocumentVisible()) return;
    prune(now);
    lastSweepAt = now;
    inFlight?.abort();
    const controller = new AbortController();
    inFlight = controller;
    try {
      for (const [taskId, entry] of tasks) {
        deps.refreshTask(taskId, entry.workspaceId, controller.signal);
      }
      for (const [workspaceId] of workspaces) {
        deps.refreshWorkspace(workspaceId, controller.signal);
      }
    } catch {
      // A throwing refresh call must not take the loop down with it.
    }
  };

  const timer = setInterval(() => sweep(), intervalMs);
  const onVisibility = () => {
    if (isDocumentVisible()) sweep();
  };
  if (typeof document !== "undefined") {
    document.addEventListener("visibilitychange", onVisibility);
  }

  return {
    noteTask(taskId, workspaceId, now = Date.now()) {
      if (stopped || !taskId) return;
      tasks.set(taskId, { workspaceId, at: now });
      if (tasks.size > MAX_TRACKED_IDS) {
        const oldest = [...tasks.entries()].sort((a, b) => a[1].at - b[1].at)[0];
        if (oldest) tasks.delete(oldest[0]);
      }
    },
    noteWorkspace(workspaceId, now = Date.now()) {
      if (stopped || !workspaceId) return;
      workspaces.set(workspaceId, now);
      if (workspaces.size > MAX_TRACKED_IDS) {
        const oldest = [...workspaces.entries()].sort((a, b) => a[1] - b[1])[0];
        if (oldest) workspaces.delete(oldest[0]);
      }
    },
    stop() {
      stopped = true;
      clearInterval(timer);
      if (typeof document !== "undefined") {
        document.removeEventListener("visibilitychange", onVisibility);
      }
      inFlight?.abort();
      tasks.clear();
      workspaces.clear();
    },
  };
}
