// UI bundle tests: exercises ui/register.ts the way the host loads it —
// window.registerKandevPlugin(id, { initialize, destroy }) — with minimal
// registry/host doubles capturing what the plugin registers.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { PluginHostApi, PluginRegistry } from "@kandev/plugin-sdk";

type Registration = {
  id: string;
  definition: { initialize(registry: PluginRegistry, host: PluginHostApi): void; destroy(): void };
};

async function loadPlugin(): Promise<Registration> {
  let registration!: Registration;
  (globalThis as Record<string, unknown>).window = {
    registerKandevPlugin(id: string, definition: Registration["definition"]) {
      registration = { id, definition };
    },
  };
  await import("../ui/register");
  return registration;
}

describe("ui/register bundle entry", () => {
  let registration: Registration;

  beforeEach(async () => {
    vi.resetModules();
    registration = await loadPlugin();
  });

  afterEach(() => {
    delete (globalThis as Record<string, unknown>).window;
  });

  it("registers under the manifest id", () => {
    expect(registration.id).toBe("kandev-plugin-github-lite");
    expect(typeof registration.definition.initialize).toBe("function");
    expect(typeof registration.definition.destroy).toBe("function");
  });

  it("registers the github-lite review provider and link action on initialize", () => {
    const taskActions: Array<Record<string, unknown>> = [];
    const reviewProviders: Array<Record<string, unknown>> = [];
    const repositoryProviders: Array<Record<string, unknown>> = [];
    const registry = {
      registerRepositoryProvider: (provider: Record<string, unknown>) => repositoryProviders.push(provider),
      registerTaskAction: (action: Record<string, unknown>) => taskActions.push(action),
      registerReviewProvider: (provider: Record<string, unknown>) => reviewProviders.push(provider),
    } as unknown as PluginRegistry;
    const host = {} as PluginHostApi;

    registration.definition.initialize(registry, host);

    expect(repositoryProviders).toHaveLength(1);
    expect(repositoryProviders[0]!.id).toBe("github-lite");
    expect(taskActions).toHaveLength(2);
    expect(taskActions.map((a) => a.id)).toEqual([
      "github-lite-sync-change-requests",
      "github-lite-link-change-request",
    ]);
    expect(taskActions[1]!.placement).toBe("link");
    expect(taskActions[1]!.singleTaskOnly).toBe(true);
    expect(reviewProviders).toHaveLength(1);
    expect(reviewProviders[0]!.id).toBe("github-lite");
    expect(reviewProviders[0]!.changeRequestNoun).toBe("pull request");
    expect(reviewProviders[0]!.ReviewPanel).toBeTypeOf("function");
    expect(reviewProviders[0]!.refresh).toBeTypeOf("function");
    expect(reviewProviders[0]!.getSnapshot).toBeTypeOf("function");

    // destroy tears the lifecycle down without throwing.
    expect(() => registration.definition.destroy()).not.toThrow();
  });
});

describe("auto-fix surface wiring", () => {
  let registration: Registration;

  beforeEach(async () => {
    vi.resetModules();
    registration = await loadPlugin();
  });

  afterEach(() => {
    delete (globalThis as Record<string, unknown>).window;
  });

  it("routes the review panel through the hook-driven auto-fix surface", () => {
    const reviewProviders: Array<Record<string, unknown>> = [];
    const registry = {
      registerRepositoryProvider: () => {},
      registerTaskAction: () => {},
      registerReviewProvider: (provider: Record<string, unknown>) => reviewProviders.push(provider),
    } as unknown as PluginRegistry;
    const jsx = vi.fn((component: unknown, props: unknown) => ({ component, props }));
    const host = {
      // The recipe only needs React at component creation; ReviewPanel itself
      // just renders the wrapper element through jsx.
      React: {},
      jsx,
      ui: { ChangeRequestDetail: Symbol("ChangeRequestDetail") },
    } as unknown as PluginHostApi;

    registration.definition.initialize(registry, host);
    const panel = reviewProviders[0]!.ReviewPanel as (props: Record<string, unknown>) => unknown;
    const element = panel({
      panelId: "review-panel",
      workspaceId: "workspace-1",
      taskId: "task-1",
      reviewKey: "acme/widgets#42",
      connectionScope: "acme:widgets",
      repositoryId: "5001",
      changeRequestNumber: 42,
      presentation: "desktop",
    }) as { component: unknown; props: Record<string, unknown> };

    // The plugin passes autoFix options, so the panel is the hook-driven
    // wrapper component — not a bare ChangeRequestDetail render.
    expect(typeof element.component).toBe("function");
    expect(element.component).not.toBe((host as unknown as { ui: Record<string, symbol> }).ui.ChangeRequestDetail);
    expect(element.props).toMatchObject({
      workspaceId: "workspace-1",
      taskId: "task-1",
      connectionScope: "acme:widgets",
      repositoryId: "5001",
      changeRequestNumber: 42,
    });
  });
});
