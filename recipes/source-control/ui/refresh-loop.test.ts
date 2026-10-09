import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { startFreshnessLoop, type FreshnessLoopDeps } from "./refresh-loop";

type Call =
  | { kind: "task"; taskId: string; workspaceId?: string; signal: AbortSignal }
  | { kind: "workspace"; workspaceId: string; signal: AbortSignal };

type DocumentStub = {
  visibilityState: string;
  listeners: Map<string, () => void>;
  addEventListener(type: string, listener: () => void): void;
  removeEventListener(type: string, listener: () => void): void;
  emit(type: string): void;
};

function installDocumentStub(initial = "visible"): DocumentStub {
  const listeners = new Map<string, () => void>();
  const stub: DocumentStub = {
    visibilityState: initial,
    listeners,
    addEventListener(type, listener) {
      listeners.set(type, listener);
    },
    removeEventListener(type, listener) {
      if (listeners.get(type) === listener) listeners.delete(type);
    },
    emit(type) {
      listeners.get(type)?.();
    },
  };
  (globalThis as Record<string, unknown>).document = stub;
  return stub;
}

function harness(overrides: Partial<FreshnessLoopDeps> = {}) {
  const calls: Call[] = [];
  const deps: FreshnessLoopDeps = {
    refreshTask: (taskId, workspaceId, signal) => {
      calls.push({ kind: "task", taskId, workspaceId, signal });
    },
    refreshWorkspace: (workspaceId, signal) => {
      calls.push({ kind: "workspace", workspaceId, signal });
    },
    intervalMs: 60_000,
    minSweepGapMs: 15_000,
    taskWindowMs: 10 * 60_000,
    workspaceWindowMs: 30 * 60_000,
    ...overrides,
  };
  const loop = startFreshnessLoop(deps);
  return { calls, loop };
}

describe("freshness loop", () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
    delete (globalThis as Record<string, unknown>).document;
  });

  it("does nothing until the host refreshes a surface", () => {
    installDocumentStub();
    const { calls } = harness();
    vi.advanceTimersByTime(600_000);
    expect(calls).toEqual([]);
  });

  it("re-refreshes noted tasks and workspaces on each interval sweep", () => {
    installDocumentStub();
    const { calls, loop } = harness();
    loop.noteTask("task-1", "workspace-1");
    loop.noteWorkspace("workspace-1");
    vi.advanceTimersByTime(60_000);
    expect(calls.length).toBe(2);
    expect(calls[0]).toMatchObject({ kind: "task", taskId: "task-1", workspaceId: "workspace-1" });
    expect(calls[1]).toMatchObject({ kind: "workspace", workspaceId: "workspace-1" });
    vi.advanceTimersByTime(60_000);
    expect(calls.length).toBe(4);
    loop.stop();
  });

  it("skips interval sweeps while the document is hidden, sweeps on visibilitychange", () => {
    const document = installDocumentStub();
    const { calls, loop } = harness();
    loop.noteTask("task-1", undefined);
    document.visibilityState = "hidden";
    vi.advanceTimersByTime(240_000);
    expect(calls.length).toBe(0);
    document.visibilityState = "visible";
    document.emit("visibilitychange");
    expect(calls.length).toBe(1);
    loop.stop();
  });

  it("prunes tasks and workspaces once past their windows", () => {
    installDocumentStub();
    const { calls, loop } = harness();
    loop.noteTask("task-1", "workspace-1");
    loop.noteWorkspace("workspace-1");
    // 10 task-window sweeps + 30 workspace-window sweeps, then everything is
    // older than its window and every further sweep is a no-op.
    vi.advanceTimersByTime(35 * 60_000);
    expect(calls.length).toBe(40);
    vi.advanceTimersByTime(60_000);
    expect(calls.length).toBe(40);
    loop.stop();
  });

  it("stops sweeping, detaches listeners, and ignores post-stop notes after stop()", () => {
    const document = installDocumentStub();
    const { calls, loop } = harness();
    loop.noteTask("task-1", "workspace-1");
    loop.stop();
    expect(document.listeners.size).toBe(0);
    vi.advanceTimersByTime(600_000);
    expect(calls.length).toBe(0);
    loop.noteTask("task-2", "workspace-2");
    document.visibilityState = "visible";
    document.emit("visibilitychange");
    vi.advanceTimersByTime(600_000);
    expect(calls.length).toBe(0);
  });

  it("honors the min sweep gap when visibility flips rapidly", () => {
    const document = installDocumentStub();
    const { calls, loop } = harness();
    loop.noteTask("task-1", undefined);
    document.visibilityState = "hidden";
    document.visibilityState = "visible";
    document.emit("visibilitychange");
    expect(calls.length).toBe(1);
    // Rapid hide/show within the gap: the forced sweep is suppressed.
    document.visibilityState = "hidden";
    document.visibilityState = "visible";
    document.emit("visibilitychange");
    expect(calls.length).toBe(1);
    // After the gap passes, the next visibility flip sweeps again.
    vi.advanceTimersByTime(16_000);
    document.visibilityState = "hidden";
    document.visibilityState = "visible";
    document.emit("visibilitychange");
    expect(calls.length).toBe(2);
    loop.stop();
  });

  it("a throwing refresh callback does not take down later sweeps", () => {
    installDocumentStub();
    const calls: Call[] = [];
    const loop = startFreshnessLoop({
      refreshTask: (taskId, workspaceId, signal) => {
        calls.push({ kind: "task", taskId, workspaceId, signal });
        throw new Error("boom");
      },
      refreshWorkspace: () => undefined,
      intervalMs: 60_000,
      minSweepGapMs: 0,
    });
    loop.noteTask("task-1", undefined);
    vi.advanceTimersByTime(60_000);
    vi.advanceTimersByTime(60_000);
    expect(calls.length).toBe(2);
    loop.stop();
  });
});
