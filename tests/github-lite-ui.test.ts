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
    expect(taskActions).toHaveLength(1);
    expect(taskActions[0]!.id).toBe("github-lite-link-change-request");
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
