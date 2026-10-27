import type {
  PluginHostApi,
  PluginRegistry,
  RepositoryProviderRegistration,
} from "@kandev/plugin-sdk";
import { describe, expect, it, vi } from "vitest";
import { registerSourceControlRecipe } from "./register";

type TaskAction = Parameters<PluginRegistry["registerTaskAction"]>[0];
type TaskLinkDialog = Parameters<PluginHostApi["openTaskLinkDialog"]>[0];
type ReviewProvider = Parameters<PluginRegistry["registerReviewProvider"]>[0];
type ActionResponder = (key: string, input?: unknown, options?: unknown) => unknown | Promise<unknown>;

function harness(actionResult: unknown | ActionResponder, options?: Record<string, unknown>) {
  let repositoryProvider: RepositoryProviderRegistration | undefined;
  let taskAction: TaskAction | undefined;
  const taskActions: TaskAction[] = [];
  let taskLinkDialog: TaskLinkDialog | undefined;
  const taskLinkClose = vi.fn();
  let reviewProvider: ReviewProvider | undefined;
  const registry = {
    registerRepositoryProvider: vi.fn((provider: RepositoryProviderRegistration) => {
      repositoryProvider = provider;
    }),
    registerTaskAction: vi.fn((action: TaskAction) => {
      taskAction = action;
      taskActions.push(action);
    }),
    registerReviewProvider: vi.fn((provider: ReviewProvider) => {
      reviewProvider = provider;
    }),
  } as unknown as PluginRegistry;
  const invokeAction = vi.fn(async (key: string, input?: unknown, options?: unknown) =>
    typeof actionResult === "function"
      ? (actionResult as ActionResponder)(key, input, options)
      : actionResult,
  );
  const miniReact = createMiniReact();
  const host = {
    api: { invokeAction },
    openTaskLinkDialog: vi.fn((dialog: TaskLinkDialog) => {
      taskLinkDialog = dialog;
      return { close: taskLinkClose };
    }),
    jsx: vi.fn((component: unknown, props: unknown) => ({ component, props })),
    React: miniReact.react,
    ui: {
      ChangeRequestDetail: Symbol("ChangeRequestDetail"),
      Alert: Symbol("Alert"),
      AlertDescription: Symbol("AlertDescription"),
    },
  } as unknown as PluginHostApi;

  const lifecycle = registerSourceControlRecipe(registry, host, {
    providerId: "acme",
    label: "Acme",
    changeRequestNoun: "change request",
    parseReference: (value) => value.trim() || null,
    toChangeRequestDetail: (review) => review,
    ...(options ?? {}),
  });

  return {
    registry,
    host,
    invokeAction,
    miniReact,
    lifecycle,
    get repositoryProvider() { return repositoryProvider; },
    get taskAction() { return taskAction; },
    get taskActions() { return taskActions; },
    get taskLinkDialog() { return taskLinkDialog; },
    taskLinkClose,
    get reviewProvider() { return reviewProvider; },
  };
}

/** A minimal call-order React hooks shim: enough to drive the hook-based
 * auto-fix panel (fixed useState/useEffect/useCallback order) in tests.
 * Render = call the component again; effects are returned for manual runs. */
function createMiniReact() {
  const states: unknown[] = [];
  const setters: Array<(value: unknown) => void> = []
  let cursor = 0;
  let onRender: () => void = () => {};
  const react = {
    useState: (initial: unknown) => {
      const index = cursor++;
      if (states.length <= index) {
        states[index] = typeof initial === "function" ? (initial as () => unknown)() : initial;
        setters[index] = (value: unknown) => {
          states[index] = typeof value === "function" ? (value as (current: unknown) => unknown)(states[index]) : value;
          onRender();
        };
      }
      return [states[index], setters[index]] as const;
    },
    useEffect: (effect: () => void | (() => void)) => {
      effects.push(effect as () => void | (() => void));
    },
    useCallback: (fn: unknown) => fn,
    useMemo: (fn: () => unknown) => fn(),
    useRef: (initial: unknown) => ({ current: initial }),
    createElement: () => null,
  };
  const effects: Array<() => void | (() => void)> = [];
  const reset = () => {
    cursor = 0;
    effects.length = 0;
  };
  return {
    react,
    reset,
    setRenderHook: (hook: () => void) => { onRender = hook; },
    getEffects: () => effects,
  };
}

describe("registerSourceControlRecipe", () => {
  it("forwards server-side repository search and pagination with the host signal", async () => {
    const test = harness({
      repositories: [
        {
          provider_id: "untrusted-result-provider",
          provider_host: "code.example",
          provider_scope: "connection-7",
          owner_or_project: "tools",
          provider_repository_id: "immutable-repository-9",
          name: "widgets",
          clone_url: "https://code.example/tools/widgets.git",
          default_branch: "main",
        },
      ],
      next_cursor: "opaque-page-2",
    });
    const signal = new AbortController().signal;

    const result = await test.repositoryProvider!.listRepositories({
      workspaceId: "workspace-1",
      query: "widgets",
      cursor: "opaque-page-1",
      limit: 25,
      signal,
    });

    expect(test.invokeAction).toHaveBeenCalledWith(
      "repositories.list",
      {
        workspaceId: "workspace-1",
        body: { query: "widgets", cursor: "opaque-page-1", limit: 25 },
      },
      { signal },
    );
    expect(result).toEqual({
      repositories: [
        {
          providerId: "acme",
          providerHost: "code.example",
          providerScope: "connection-7",
          ownerOrProject: "tools",
          repositoryId: "immutable-repository-9",
          repositoryName: "widgets",
          cloneUrl: "https://code.example/tools/widgets.git",
          defaultBranch: "main",
        },
      ],
      nextCursor: "opaque-page-2",
    });
  });

  it("routes URL inspection through the verified workspace action", async () => {
    const test = harness({
      repository: {
        provider_host: "code.example",
        provider_scope: "connection-7",
        owner_or_project: "tools",
        provider_repository_id: "immutable-repository-9",
        name: "widgets",
        clone_url: "https://code.example/tools/widgets.git",
      },
    });
    const signal = new AbortController().signal;

    const inspected = await test.repositoryProvider!.inspectURL({
      workspaceId: "workspace-1",
      url: "https://code.example/tools/widgets",
      signal,
    });

    expect(test.invokeAction).toHaveBeenCalledWith(
      "repositories.inspect",
      {
        workspaceId: "workspace-1",
        body: { url: "https://code.example/tools/widgets" },
      },
      { signal },
    );
    expect(inspected).toMatchObject({
      providerId: "acme",
      providerScope: "connection-7",
      repositoryId: "immutable-repository-9",
    });
  });

  it("normalizes PR detail from a PR-URL inspection for the task-create flow", async () => {
    const test = harness({
      repository: {
        provider_host: "github.com",
        provider_scope: "connection-7",
        owner_or_project: "acme",
        provider_repository_id: "immutable-repository-9",
        name: "widgets",
        clone_url: "https://github.com/acme/widgets.git",
        default_branch: "main",
        head_branch: "feat/assets-prefetch",
        base_branch: "main",
        pull_request: { number: 42, title: "PR 42" },
      },
    });
    const signal = new AbortController().signal;

    const inspected = await test.repositoryProvider!.inspectURL({
      workspaceId: "workspace-1",
      url: "https://github.com/acme/widgets/pull/42",
      signal,
    });

    expect(inspected).toMatchObject({
      headBranch: "feat/assets-prefetch",
      baseBranch: "main",
      pullRequest: { number: 42, title: "PR 42" },
    });
  });

  it("lists branches through a credential-free immutable repository descriptor", async () => {
    const test = harness({
      branches: [
        { name: "main", commit: "abc123", is_default: true },
        { name: "feature/provider-hooks", commit: "def456" },
      ],
    });
    const signal = new AbortController().signal;

    const branches = await test.repositoryProvider!.listBranches({
      workspaceId: "workspace-1",
      repository: {
        providerId: "acme",
        providerHost: "code.example",
        providerScope: "connection-7",
        ownerOrProject: "tools",
        repositoryId: "immutable-repository-9",
        repositoryName: "widgets",
        cloneUrl: "https://code.example/tools/widgets.git",
        defaultBranch: "main",
      },
      signal,
    });

    expect(test.invokeAction).toHaveBeenCalledWith(
      "repositories.branches",
      {
        workspaceId: "workspace-1",
        body: {
          repository: {
            provider_id: "acme",
            provider_host: "code.example",
            provider_scope: "connection-7",
            provider_repository_id: "immutable-repository-9",
            owner_or_project: "tools",
            name: "widgets",
            clone_url: "https://code.example/tools/widgets.git",
            default_branch: "main",
          },
        },
      },
      { signal },
    );
    expect(branches).toEqual([{ name: "main" }, { name: "feature/provider-hooks" }]);
  });

  it("creates a change request from verified host selectors and returns partial-link outcomes", async () => {
    const test = harness({
      url: "https://code.example/tools/widgets/changes/42",
      provider: "untrusted-result-provider",
      linked: false,
      association_error: "Created remotely; link it again after reconnecting.",
    });
    const signal = new AbortController().signal;

    const result = await test.repositoryProvider!.createChangeRequest!({
      workspaceId: "workspace-1",
      taskId: "task-2",
      sessionId: "session-3",
      repositoryId: "host-repository-4",
      repository: {
        id: "host-repository-4",
        workspace_id: "workspace-1",
        name: "widgets",
        provider: "acme",
      },
      title: "Demonstrate provider hooks",
      body: "Provider-neutral description",
      baseBranch: "main",
      draft: true,
      signal,
    });

    expect(test.invokeAction).toHaveBeenCalledWith(
      "change_requests.create",
      {
        workspaceId: "workspace-1",
        taskId: "task-2",
        sessionId: "session-3",
        repositoryId: "host-repository-4",
        body: {
          title: "Demonstrate provider hooks",
          description: "Provider-neutral description",
          destination: "main",
          draft: true,
        },
      },
      { signal },
    );
    expect(result).toEqual({
      url: "https://code.example/tools/widgets/changes/42",
      provider: "acme",
      linked: false,
      associationError: "Created remotely; link it again after reconnecting.",
    });
  });

  it("registers a host-owned Sync action that re-reads the task on demand", async () => {
    const test = harness(undefined);
    const sync = test.taskActions.find((action) => action.id === "acme-sync-change-requests");
    expect(sync).toBeDefined();
    expect(sync!.label).toBe("Sync Acme change requests");
    expect(sync!.placement).toBe("link");
    expect(sync!.singleTaskOnly).toBe(true);

    await sync!.run({
      workspaceId: "workspace-1",
      taskId: "task-2",
      repositories: [],
      pathname: "/tasks/task-2",
      presentation: "desktop",
    });

    // One sync press: the server drops the task's cached reads, then the
    // panel + workspace refreshes follow on the fresh cache.
    const syncCalls = test.invokeAction.mock.calls.filter((call) => call[0] === "change_requests.sync");
    expect(syncCalls).toEqual([
      ["change_requests.sync", { workspaceId: "workspace-1", taskId: "task-2" }],
    ]);
    expect(test.invokeAction).toHaveBeenCalledWith(
      "change_requests.get",
      expect.objectContaining({ workspaceId: "workspace-1", taskId: "task-2" }),
      expect.anything(),
    );
    expect(test.invokeAction).toHaveBeenCalledWith(
      "change_requests.associations",
      expect.objectContaining({ workspaceId: "workspace-1" }),
      expect.anything(),
    );
  });

  it("registers a host-owned Link action and forwards cancellation to the link action", async () => {
    const test = harness({ linked: true });
    await test.taskAction!.run({
      workspaceId: "workspace-1",
      taskId: "task-2",
      repositories: [],
      pathname: "/tasks/task-2",
      presentation: "mobile",
    });
    const signal = new AbortController().signal;

    await test.taskLinkDialog!.onSubmit("  tools/widgets#42  ", signal);

    expect(test.taskAction).toMatchObject({
      id: "acme-link-change-request",
      label: "Acme change request",
      placement: "link",
      singleTaskOnly: true,
    });
    expect(test.invokeAction).toHaveBeenCalledWith(
      "change_requests.link",
      {
        workspaceId: "workspace-1",
        taskId: "task-2",
        body: { reference: "tools/widgets#42" },
      },
      { signal },
    );
  });

  it("publishes semantic review status and CI checks for host-owned surfaces", async () => {
    const test = harness((key: string) => {
      if (key !== "change_requests.get") throw new Error(`unexpected action: ${key}`);
      return {
        reviews: [
          {
            provider_id: "untrusted-result-provider",
            review_key: "connection-7/repository-9#42",
            title: "Provider-neutral hooks",
            url: "https://code.example/tools/widgets/changes/42",
            connection_scope: "connection-7",
            repository_id: "repository-9",
            change_request_number: 42,
            state: "open",
            task_status: {
              number: 42,
              state: "open",
              pipeline_state: "failure",
              checks: [
                {
                  id: "build",
                  label: "Build",
                  state: "failure",
                  detail: "One job failed",
                  url: "https://ci.example/build/7",
                },
              ],
              review: {
                state: "changes_requested",
                approved: 1,
                required: 2,
                requested: 1,
              },
              unresolved_comments: 3,
              updated_at: 1_765_000_000_000,
            },
          },
          {
            review_key: "missing-immutable-identity",
            title: "Ignored",
            url: "https://code.example/ignored",
            change_request_number: 9,
          },
        ],
      };
    });
    const signal = new AbortController().signal;

    await test.reviewProvider!.refresh("task-2", signal);

    expect(test.invokeAction).toHaveBeenCalledWith(
      "change_requests.get",
      { taskId: "task-2" },
      { signal },
    );
    expect(test.reviewProvider!.getSnapshot("task-2")).toEqual([
      {
        providerId: "acme",
        reviewKey: "connection-7/repository-9#42",
        title: "Provider-neutral hooks",
        url: "https://code.example/tools/widgets/changes/42",
        connectionScope: "connection-7",
        repositoryId: "repository-9",
        changeRequestNumber: 42,
        state: "open",
        taskStatus: {
          number: 42,
          state: "open",
          pipelineState: "failure",
          checks: [
            {
              id: "build",
              label: "Build",
              state: "failure",
              detail: "One job failed",
              url: "https://ci.example/build/7",
            },
          ],
          review: {
            state: "changes_requested",
            approved: 1,
            required: 2,
            requested: 1,
          },
          unresolvedComments: 3,
          updatedAt: 1_765_000_000_000,
        },
      },
    ]);
  });

  it("keeps complete immutable association identity through refresh and unlink", async () => {
    const test = harness((key: string) => {
      if (key === "change_requests.associations") {
        return {
          associations: [
            {
              provider_id: "untrusted-result-provider",
              task_id: "task-2",
              review_key: "display-key-can-change",
              connection_scope: "connection-7",
              repository_id: "repository-9",
              change_request_number: 42,
            },
            {
              task_id: "task-ignored",
              review_key: "incomplete",
              repository_id: "repository-9",
              change_request_number: 9,
            },
          ],
        };
      }
      if (key === "change_requests.unlink") return { unlinked: true };
      throw new Error(`unexpected action: ${key}`);
    });
    const signal = new AbortController().signal;

    await test.reviewProvider!.refreshAssociations!("workspace-1", signal);

    expect(test.reviewProvider!.getAssociationSnapshot!("workspace-1")).toEqual([
      {
        providerId: "acme",
        taskId: "task-2",
        reviewKey: "display-key-can-change",
        connectionScope: "connection-7",
        repositoryId: "repository-9",
        changeRequestNumber: 42,
      },
    ]);

    await test.reviewProvider!.unlink!({
      workspaceId: "workspace-1",
      taskId: "task-2",
      reviewKey: "new-display-key",
      connectionScope: "connection-7",
      repositoryId: "repository-9",
      changeRequestNumber: 42,
      signal,
    });

    expect(test.invokeAction).toHaveBeenLastCalledWith(
      "change_requests.unlink",
      {
        workspaceId: "workspace-1",
        taskId: "task-2",
        body: {
          connection_scope: "connection-7",
          repository_id: "repository-9",
          number: 42,
        },
      },
      { signal },
    );
  });

  it("uses the same host review detail surface on desktop and mobile", async () => {
    const test = harness({
      reviews: [
        {
          review_key: "connection-7/repository-9#42",
          title: "Provider-neutral hooks",
          url: "https://code.example/tools/widgets/changes/42",
          connection_scope: "connection-7",
          repository_id: "repository-9",
          change_request_number: 42,
          state: "open",
        },
      ],
    });
    await test.reviewProvider!.refresh("task-2", new AbortController().signal);

    const common = {
      panelId: "review-panel",
      workspaceId: "workspace-1",
      taskId: "task-2",
      reviewKey: "connection-7/repository-9#42",
      connectionScope: "connection-7",
      repositoryId: "repository-9",
      changeRequestNumber: 42,
    };
    test.reviewProvider!.ReviewPanel({ ...common, presentation: "desktop" });
    test.reviewProvider!.ReviewPanel({ ...common, presentation: "mobile", sessionId: "session-3" });

    expect(test.host.jsx).toHaveBeenNthCalledWith(
      1,
      test.host.ui.ChangeRequestDetail,
      expect.objectContaining({
        presentation: "desktop",
        detail: expect.objectContaining({
          providerId: "acme",
          connectionScope: "connection-7",
          repositoryId: "repository-9",
          changeRequestNumber: 42,
        }),
      }),
    );
    expect(test.host.jsx).toHaveBeenNthCalledWith(
      2,
      test.host.ui.ChangeRequestDetail,
      expect.objectContaining({
        presentation: "mobile",
        detail: expect.objectContaining({
          providerId: "acme",
          connectionScope: "connection-7",
          repositoryId: "repository-9",
          changeRequestNumber: 42,
        }),
      }),
    );
  });

  it("fences late refresh results after plugin unload", async () => {
    let resolveResponse!: (value: unknown) => void;
    const response = new Promise<unknown>((resolve) => {
      resolveResponse = resolve;
    });
    const test = harness(() => response);
    const refresh = test.reviewProvider!.refresh("task-2", new AbortController().signal);

    test.lifecycle.destroy();
    resolveResponse({
      reviews: [
        {
          review_key: "connection-7/repository-9#42",
          title: "Late result",
          url: "https://code.example/tools/widgets/changes/42",
          connection_scope: "connection-7",
          repository_id: "repository-9",
          change_request_number: 42,
          state: "open",
        },
      ],
    });
    await refresh;

    expect(test.reviewProvider!.getSnapshot("task-2")).toEqual([]);
  });

  it("preserves a remote create outcome when best-effort snapshot refresh fails", async () => {
    const test = harness((key: string) => {
      if (key === "change_requests.create") {
        return {
          url: "https://code.example/tools/widgets/changes/42",
          linked: false,
          association_error: "task association could not be saved",
        };
      }
      if (key === "change_requests.get" || key === "change_requests.associations") {
        throw new Error("refresh unavailable");
      }
      throw new Error(`unexpected action: ${key}`);
    });
    const signal = new AbortController().signal;

    const result = await test.repositoryProvider!.createChangeRequest!({
      workspaceId: "workspace-1",
      taskId: "task-2",
      sessionId: "session-3",
      repositoryId: "host-repository-4",
      repository: {
        id: "host-repository-4",
        workspace_id: "workspace-1",
        name: "widgets",
        provider: "acme",
      },
      title: "Provider hooks",
      body: "Description",
      baseBranch: "main",
      draft: false,
      signal,
    });

    expect(result).toMatchObject({
      url: "https://code.example/tools/widgets/changes/42",
      linked: false,
      associationError: "task association could not be saved",
    });
    expect(test.invokeAction.mock.calls.map(([key]) => key)).toEqual([
      "change_requests.create",
      "change_requests.get",
      "change_requests.associations",
    ]);
  });

  it("keeps a completed mutation outcome when cancellation stops only its refresh", async () => {
    const controller = new AbortController();
    const test = harness((key: string) => {
      if (key === "change_requests.create") {
        return { url: "https://code.example/tools/widgets/changes/42", linked: true };
      }
      if (key === "change_requests.get") {
        controller.abort();
        throw controller.signal.reason;
      }
      throw new Error(`unexpected action: ${key}`);
    });

    const result = await test.repositoryProvider!.createChangeRequest!({
      workspaceId: "workspace-1",
      taskId: "task-2",
      sessionId: "session-3",
      repositoryId: "host-repository-4",
      repository: {
        id: "host-repository-4",
        workspace_id: "workspace-1",
        name: "widgets",
        provider: "acme",
      },
      title: "Provider hooks",
      body: "Description",
      draft: false,
      signal: controller.signal,
    });

    expect(result).toMatchObject({
      url: "https://code.example/tools/widgets/changes/42",
      linked: true,
    });
    expect(controller.signal.aborted).toBe(true);
  });

  it("does not start provider I/O for an already-canceled callback", async () => {
    const test = harness({ repositories: [] });
    const controller = new AbortController();
    controller.abort();

    await expect(test.repositoryProvider!.listRepositories({
      workspaceId: "workspace-1",
      signal: controller.signal,
    })).rejects.toMatchObject({ name: "AbortError" });

    expect(test.invokeAction).not.toHaveBeenCalled();
  });

  it("closes plugin-owned Link dialogs during teardown", async () => {
    const test = harness({ linked: true });
    await test.taskAction!.run({
      workspaceId: "workspace-1",
      taskId: "task-2",
      repositories: [],
      pathname: "/tasks/task-2",
      presentation: "desktop",
    });

    test.lifecycle.destroy();

    expect(test.taskLinkClose).toHaveBeenCalledOnce();
  });
});

// --- auto-fix loop surface --------------------------------------------------

type JsxCall = { component: unknown; props: Record<string, unknown> };

/** Renders the hook-driven auto-fix panel (the component ReviewPanel
 * returned) and replays it on every state change, like React would. */
function mountAutoFixPanel(test: ReturnType<typeof harness>, props: Record<string, unknown>) {
  const jsx = test.host.jsx as ReturnType<typeof vi.fn>;
  const detailCalls = (): JsxCall[] =>
    jsx.mock.calls
      .filter(([component]) => component === (test.host.ui as unknown as Record<string, symbol>).ChangeRequestDetail)
      .map((call) => ({ component: call[0], props: call[1] as Record<string, unknown> }));
  const lastDetail = () => detailCalls().at(-1);
  let renderCount = 0;
  const render = () => {
    test.miniReact.reset();
    renderCount += 1;
    // ReviewPanel rendered the wrapper component through jsx; find and
    // invoke it with the same props.
    const wrapper = (jsx.mock.calls.find(([component]) => typeof component === "function") ?? []) as [unknown, Record<string, unknown>];
    if (!wrapper.length) throw new Error("ReviewPanel did not render the auto-fix panel component");
    (wrapper[0] as (p: Record<string, unknown>) => unknown)(wrapper[1]);
  };
  test.miniReact.setRenderHook(render);
  render();
  return { render, renderCount: () => renderCount, lastDetail };
}

const autoFixPanelProps = {
  panelId: "review-panel",
  workspaceId: "workspace-1",
  taskId: "task-2",
  reviewKey: "connection-7/repository-9#42",
  connectionScope: "connection-7",
  repositoryId: "repository-9",
  changeRequestNumber: 42,
  presentation: "desktop" as const,
};

async function seedReview(test: ReturnType<typeof harness>) {
  await test.reviewProvider!.refresh("task-2", new AbortController().signal);
}


const autoFixSeedReview = {
  review_key: "connection-7/repository-9#42",
  title: "Provider-neutral hooks",
  url: "https://code.example/tools/widgets/changes/42",
  connection_scope: "connection-7",
  repository_id: "repository-9",
  change_request_number: 42,
  state: "open",
};

/** One responder for the whole auto-fix surface: review seeding keys plus
 * the status/start/stop actions. Unexpected keys throw loudly. */
function autoFixResponder(args: {
  status: () => Record<string, unknown>;
  command?: (command: string) => Record<string, unknown>;
}) {
  return (key: string, input?: unknown) => {
    switch (key) {
      case "change_requests.get":
        return { reviews: [autoFixSeedReview] };
      case "change_requests.associations":
        return { associations: [] };
      case "change_requests.auto_fix_status":
        return args.status();
      case "change_requests.auto_fix":
        if (!args.command) throw new Error(`unexpected action ${key}`);
        return args.command((((input ?? {}) as { body?: { command?: string } }).body ?? {}).command ?? "start");
      default:
        throw new Error(`unexpected action ${key}`);
    }
  };
}

describe("auto-fix loop surface", () => {
  it("leaves the detail surface untouched when the provider has no auto-fix options", async () => {
    const test = harness({ auto_fix: { status: "running" }, found: true });
    await seedReview(test);
    test.reviewProvider!.ReviewPanel(autoFixPanelProps);
    const call = (test.host.jsx as ReturnType<typeof vi.fn>).mock.calls.at(-1)!;
    expect(call[0]).toBe((test.host.ui as unknown as Record<string, symbol>).ChangeRequestDetail);
    expect(call[1]).not.toHaveProperty("actions");
    expect(call[1]).not.toHaveProperty("onAction");
    expect(call[1]).not.toHaveProperty("notice");
    // The only host action traffic is the panel's own review seeding — no
    // auto-fix status poll ever fires.
    expect(test.invokeAction.mock.calls.some(([key]) => String(key).startsWith("change_requests.auto_fix"))).toBe(false);
  });

  it("polls the zero-provider status action on mount and renders the stop surface while running", async () => {
    vi.useFakeTimers();
    try {
      const test = harness(
        autoFixResponder({
          status: () => ({ auto_fix: { running: true, status: "running", rounds: 1, max_rounds: 3, message: "round 1/3 dispatched — waiting for the session" }, found: true }),
        }),
        { autoFix: {} },
      );
      await seedReview(test);

      test.reviewProvider!.ReviewPanel(autoFixPanelProps);
      const panel = mountAutoFixPanel(test, autoFixPanelProps);
      const effect = test.miniReact.getEffects()[0]!;
      const cleanup = effect() as () => void;
      await vi.advanceTimersByTimeAsync(0);

      // The mount poll used the status action with the verified context and
      // the immutable change-request identity — a host-only read.
      expect(test.invokeAction).toHaveBeenCalledWith(
        "change_requests.auto_fix_status",
        expect.objectContaining({
          workspaceId: "workspace-1",
          taskId: "task-2",
          body: { connection_scope: "connection-7", repository_id: "repository-9", number: 42 },
        }),
      );

      const detail = panel.lastDetail()!;
      expect(detail.props.actions).toEqual([
        expect.objectContaining({ id: "auto_fix", label: "Stop auto-fix", placement: "header" }),
      ]);
      expect((detail.props.notice as { props: { children: { props: { children: string } } } }).props.children.props.children).toContain("round 1/3 dispatched");
      expect(detail.props.busyActionId).toBeNull();

      // A running loop is re-polled at the fast cadence; the panel does not
      // refresh the provider snapshot to learn loop status.
      await vi.advanceTimersByTimeAsync(2000);
      expect(test.invokeAction.mock.calls.filter(([key]) => key === "change_requests.auto_fix_status")).toHaveLength(2);

      cleanup();
      await vi.advanceTimersByTimeAsync(15000);
      expect(test.invokeAction.mock.calls.filter(([key]) => key === "change_requests.auto_fix_status")).toHaveLength(2);
    } finally {
      vi.useRealTimers();
    }
  });

  it("starts and stops the loop through the header action with explicit commands", async () => {
    vi.useFakeTimers();
    try {
      const commands: string[] = [];
      const test = harness(
        autoFixResponder({
          status: () => ({ auto_fix: { running: commands.at(-1) === "stop", status: commands.at(-1) === "stop" ? "stopped" : "running", rounds: commands.length, max_rounds: 3 }, found: true }),
          command: (command) => {
            commands.push(command);
            return { auto_fix: { running: command !== "stop", status: command === "stop" ? "stopped" : "running", rounds: 1, max_rounds: 3 } };
          },
        }),
        { autoFix: { label: "Fix CI", stopLabel: "Halt auto-fix" } },
      );
      await seedReview(test);

      test.reviewProvider!.ReviewPanel(autoFixPanelProps);
      const panel = mountAutoFixPanel(test, autoFixPanelProps);
      const cleanup = test.miniReact.getEffects()[0]!() as () => void;
      await vi.advanceTimersByTimeAsync(0);

      // Idle report: the start surface with the provider's labels.
      let detail = panel.lastDetail()!;
      const startAction = (detail.props.actions as Array<Record<string, unknown>>)[0]!;
      expect(startAction.label).toBe("Fix CI");
      expect(startAction.disabled).toBe(false);

      // Press start: explicit command through the start/stop action.
      await (detail.props.onAction as (request: { actionId: string }) => Promise<void>)({ actionId: "auto_fix" });
      expect(commands).toEqual(["start"]);

      detail = panel.lastDetail()!;
      expect((detail.props.actions as Array<Record<string, unknown>>)[0]!.label).toBe("Halt auto-fix");

      // Press stop: the same action, the opposite command.
      await (detail.props.onAction as (request: { actionId: string }) => Promise<void>)({ actionId: "auto_fix" });
      expect(commands).toEqual(["start", "stop"]);

      cleanup();
    } finally {
      vi.useRealTimers();
    }
  });

  it("surfaces provider errors as the notice instead of failing the panel", async () => {
    vi.useFakeTimers();
    try {
      const test = harness(
        autoFixResponder({
          status: () => ({ auto_fix: { running: false, status: "idle" }, found: false }),
          command: () => {
            throw new Error("boom: session is FAILED");
          },
        }),
        { autoFix: {} },
      );
      await seedReview(test);

      test.reviewProvider!.ReviewPanel(autoFixPanelProps);
      const panel = mountAutoFixPanel(test, autoFixPanelProps);
      const cleanup = test.miniReact.getEffects()[0]!() as () => void;
      await vi.advanceTimersByTimeAsync(0);

      const detail = panel.lastDetail()!;
      await (detail.props.onAction as (request: { actionId: string }) => Promise<void>)({ actionId: "auto_fix" });
      const after = panel.lastDetail()!;
      expect((after.props.notice as { props: { children: { props: { children: string } } } }).props.children.props.children).toContain("session is FAILED");
      // The panel itself is intact: the detail still renders.
      expect(after.component).toBe((test.host.ui as unknown as Record<string, symbol>).ChangeRequestDetail);
      cleanup();
    } finally {
      vi.useRealTimers();
    }
  });

  it("hides the surface entirely when the provider reports auto-fix unsupported", async () => {
    vi.useFakeTimers();
    try {
      const test = harness(
        autoFixResponder({
          status: () => ({ auto_fix: { running: false, status: "unsupported", message: "the auto-fix loop is disabled in this plugin's settings" }, found: false }),
        }),
        { autoFix: {} },
      );
      await seedReview(test);

      test.reviewProvider!.ReviewPanel(autoFixPanelProps);
      const panel = mountAutoFixPanel(test, autoFixPanelProps);
      const cleanup = test.miniReact.getEffects()[0]!() as () => void;
      await vi.advanceTimersByTimeAsync(0);

      const detail = panel.lastDetail()!;
      expect(detail.props.actions).toBeUndefined();
      expect(detail.props.notice).toBeUndefined();
      // No presses are possible: nothing routes to the start/stop action.
      expect(test.invokeAction.mock.calls.some(([key]) => key === "change_requests.auto_fix")).toBe(false);
      cleanup();
    } finally {
      vi.useRealTimers();
    }
  });
});
