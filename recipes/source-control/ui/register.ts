import type {
  PluginHostApi,
  PluginIcon,
  PluginRegistry,
  RepositoryInspection,
  RepositoryProviderRegistration,
  ReviewSummary,
  ReviewTaskAssociation,
  ReviewTaskStatus,
} from "@kandev/plugin-sdk";
import { createSnapshotStore } from "./review-store";
import { startFreshnessLoop } from "./refresh-loop";

/** ReviewSummary plus the optional provider-owned detail document the server
 * carries through the boundary (github-lite: the host ChangeRequestDetailModel
 * payload). Untouched by the recipe; forwarded to toChangeRequestDetail. */
export type RecipeReviewSummary = ReviewSummary & {
  detail?: Record<string, unknown>;
};

export type SourceControlRecipeOptions = {
  providerId: string;
  label: string;
  icon?: PluginIcon;
  changeRequestNoun: string;
  order?: number;
  supportsDraft?: boolean;
  matchesURL?(url: string): boolean;
  parseReference(reference: string): string | null;
  toChangeRequestDetail(review: RecipeReviewSummary): unknown;
  /** Optional per-change-request auto-fix loop (github-lite: a bounded
   * press-triggered @ci-auto-fix-style loop). Omit when the provider has no
   * auto-fix capability; the review panel then renders exactly as before. */
  autoFix?: SourceControlAutoFixOptions;
};

/** The auto-fix surface the review provider adds to the change request
 * detail panel: a header action (start/stop) plus a status notice that
 * polls the plugin's zero-provider-traffic status action while mounted. */
export type SourceControlAutoFixOptions = {
  /** Start/stop action key. Defaults to "change_requests.auto_fix". */
  actionKey?: string;
  /** Status action key. Defaults to "change_requests.auto_fix_status". */
  statusKey?: string;
  /** Start button label. Defaults to "Auto-fix CI". */
  label?: string;
  /** Stop button label. Defaults to "Stop auto-fix". */
  stopLabel?: string;
};

type AutoFixStatusResponse = {
  auto_fix?: unknown;
  found?: unknown;
};

/** Normalized auto-fix loop report (mirrors the server's AutoFixReport). */
type AutoFixReport = {
  running: boolean;
  status: string;
  rounds: number;
  maxRounds: number;
  message: string;
  updatedAt: number;
};

function normalizeAutoFixReport(value: unknown): AutoFixReport | undefined {
  const source = record(value);
  const status = text(source.status);
  if (!status) return undefined;
  return {
    running: source.running === true,
    status,
    rounds: nonNegativeInteger(source.rounds) ?? 0,
    maxRounds: nonNegativeInteger(source.max_rounds) ?? 0,
    message: text(source.message),
    updatedAt: finiteNumber(source.updated_at) ?? 0,
  };
}

function autoFixNoticeText(report: AutoFixReport): string {
  if (report.message) return report.message;
  if (report.running) {
    const rounds =
      report.maxRounds > 0
        ? ` — round ${Math.max(report.rounds, 1)}/${report.maxRounds}`
        : "";
    return `Auto-fix running${rounds}`;
  }
  return `Auto-fix ${report.status.replaceAll("_", " ")}`;
}

export type SourceControlRecipeLifecycle = {
  destroy(): void;
};

type RepositoryPageResponse = {
  repositories?: unknown[];
  next_cursor?: unknown;
};

type RepositoryInspectionResponse = { repository?: unknown };
type BranchListResponse = { branches?: unknown[] };
type ReviewListResponse = { reviews?: unknown[] };
type ReviewAssociationListResponse = { associations?: unknown[] };
type CreateChangeRequestResponse = {
  url?: unknown;
  output?: unknown;
  linked?: unknown;
  association_error?: unknown;
};

function record(value: unknown): Record<string, unknown> {
  return value !== null && typeof value === "object" ? (value as Record<string, unknown>) : {};
}

function text(value: unknown): string {
  return typeof value === "string" ? value.trim() : "";
}

function finiteNumber(value: unknown): number | undefined {
  return typeof value === "number" && Number.isFinite(value) ? value : undefined;
}

function nonNegativeInteger(value: unknown): number | undefined {
  const number = finiteNumber(value);
  return number !== undefined && Number.isInteger(number) && number >= 0 ? number : undefined;
}

function positiveInteger(value: unknown): number | undefined {
  const number = nonNegativeInteger(value);
  return number !== undefined && number > 0 ? number : undefined;
}

function normalizeTaskStatus(value: unknown): ReviewTaskStatus | undefined {
  const source = record(value);
  const number = positiveInteger(source.number);
  const state = text(source.state);
  const pipelineState = text(source.pipeline_state);
  if (
    number === undefined ||
    !(["open", "merged", "closed", "draft"] as const).includes(
      state as "open" | "merged" | "closed" | "draft",
    ) ||
    !(["success", "failure", "pending", "neutral"] as const).includes(
      pipelineState as "success" | "failure" | "pending" | "neutral",
    )
  ) {
    return undefined;
  }

  const checks = Array.isArray(source.checks)
    ? source.checks.flatMap((value) => {
        const check = record(value);
        const id = text(check.id);
        const label = text(check.label);
        const checkState = text(check.state);
        if (
          !id ||
          !label ||
          !(["success", "failure", "pending", "neutral"] as const).includes(
            checkState as "success" | "failure" | "pending" | "neutral",
          )
        ) {
          return [];
        }
        const detail = text(check.detail);
        const url = text(check.url);
        return [{
          id,
          label,
          state: checkState as "success" | "failure" | "pending" | "neutral",
          ...(detail ? { detail } : {}),
          ...(url ? { url } : {}),
        }];
      })
    : [];

  const reviewSource = record(source.review);
  const reviewState = text(reviewSource.state);
  const approved = nonNegativeInteger(reviewSource.approved);
  const required = nonNegativeInteger(reviewSource.required);
  const requested = nonNegativeInteger(reviewSource.requested);
  const review =
    approved !== undefined &&
    (["approved", "changes_requested", "pending"] as const).includes(
      reviewState as "approved" | "changes_requested" | "pending",
    )
      ? {
          state: reviewState as "approved" | "changes_requested" | "pending",
          approved,
          ...(required === undefined ? {} : { required }),
          ...(requested === undefined ? {} : { requested }),
        }
      : undefined;
  const unresolvedComments = nonNegativeInteger(source.unresolved_comments);
  const updatedAt = finiteNumber(source.updated_at);

  return {
    number,
    state: state as "open" | "merged" | "closed" | "draft",
    pipelineState: pipelineState as "success" | "failure" | "pending" | "neutral",
    checks,
    ...(review ? { review } : {}),
    ...(unresolvedComments === undefined ? {} : { unresolvedComments }),
    ...(updatedAt === undefined ? {} : { updatedAt }),
  };
}

function normalizeReview(providerId: string, value: unknown): RecipeReviewSummary | null {
  const source = record(value);
  const reviewKey = text(source.review_key);
  const title = text(source.title);
  const url = text(source.url);
  const connectionScope = text(source.connection_scope);
  const repositoryId = text(source.repository_id);
  const changeRequestNumber = positiveInteger(source.change_request_number);
  if (
    !reviewKey ||
    !title ||
    !url ||
    !connectionScope ||
    !repositoryId ||
    changeRequestNumber === undefined
  ) {
    return null;
  }
  const state = text(source.state);
  const taskStatus = normalizeTaskStatus(source.task_status);
  const detail =
    source.detail !== null &&
    typeof source.detail === "object" &&
    !Array.isArray(source.detail)
      ? (source.detail as Record<string, unknown>)
      : undefined;
  return {
    providerId,
    reviewKey,
    title,
    url,
    connectionScope,
    repositoryId,
    changeRequestNumber,
    state,
    ...(taskStatus ? { taskStatus } : {}),
    ...(detail !== undefined ? { detail } : {}),
  };
}

function normalizeAssociation(providerId: string, value: unknown): ReviewTaskAssociation | null {
  const source = record(value);
  const taskId = text(source.task_id);
  const reviewKey = text(source.review_key);
  const connectionScope = text(source.connection_scope);
  const repositoryId = text(source.repository_id);
  const changeRequestNumber = positiveInteger(source.change_request_number);
  if (
    !taskId ||
    !reviewKey ||
    !connectionScope ||
    !repositoryId ||
    changeRequestNumber === undefined
  ) {
    return null;
  }
  return {
    providerId,
    taskId,
    reviewKey,
    connectionScope,
    repositoryId,
    changeRequestNumber,
  };
}

function normalizeRepository(providerId: string, value: unknown): RepositoryInspection | null {
  const source = record(value);
  const providerHost = text(source.provider_host);
  const ownerOrProject = text(source.owner_or_project);
  // kandev's backend validator requires the `provider_repository_id`
  // spelling; the legacy `repository_id` spelling is accepted for symmetry
  // with the server recipe's dual-key branch-list input.
  const repositoryId = text(source.provider_repository_id) || text(source.repository_id);
  const repositoryName = text(source.name);
  const cloneUrl = text(source.clone_url);
  if (!providerHost || !ownerOrProject || !repositoryId || !repositoryName || !cloneUrl) return null;
  const providerScope = text(source.provider_scope);
  const defaultBranch = text(source.default_branch);
  // Pull-request detail, present only when the inspected URL points at a PR
  // (.../pull/N): the task-create URL flow preselects the PR's head branch
  // and prefills the task title from these.
  const headBranch = text(source.head_branch);
  const baseBranch = text(source.base_branch);
  const pull = record(source.pull_request);
  let pullRequest: RepositoryInspection["pullRequest"] = undefined;
  if (pull) {
    const candidate = Number(pull.number);
    if (Number.isFinite(candidate) && candidate > 0) {
      pullRequest = { number: candidate, title: text(pull.title) };
    }
  }
  return {
    providerId,
    providerHost,
    ...(providerScope ? { providerScope } : {}),
    ownerOrProject,
    repositoryId,
    repositoryName,
    cloneUrl,
    ...(defaultBranch ? { defaultBranch } : {}),
    ...(headBranch ? { headBranch } : {}),
    ...(baseBranch ? { baseBranch } : {}),
    ...(pullRequest ? { pullRequest } : {}),
  };
}

function credentialFreeRepository(repository: RepositoryInspection) {
  return {
    provider_id: repository.providerId,
    provider_host: repository.providerHost,
    provider_scope: repository.providerScope ?? "",
    provider_repository_id: repository.repositoryId,
    owner_or_project: repository.ownerOrProject,
    name: repository.repositoryName,
    clone_url: repository.cloneUrl,
    default_branch: repository.defaultBranch ?? "",
  };
}

export function registerSourceControlRecipe(
  registry: PluginRegistry,
  host: PluginHostApi,
  options: SourceControlRecipeOptions,
): SourceControlRecipeLifecycle {
  const reviewStore = createSnapshotStore<string, RecipeReviewSummary>();
  const associationStore = createSnapshotStore<string, ReviewTaskAssociation>();
  const overlays = new Set<{ close(): void }>();

  // Visibility-gated freshness: re-refreshes the surfaces the host has
  // already refreshed while the window is visible, so a PR merged on the
  // forge shows up on the ticket instead of staying stale until the user
  // navigates. The plugin server's TTL cache dedupes the GitHub traffic.
  const freshness = startFreshnessLoop({
    refreshTask: (taskId, workspaceId, signal) => {
      void refreshReviews(taskId, signal, workspaceId).catch(() => undefined);
    },
    refreshWorkspace: (workspaceId, signal) => {
      void refreshAssociations(workspaceId, signal).catch(() => undefined);
    },
  });

  async function refreshReviews(taskId: string, signal: AbortSignal, workspaceId?: string) {
    freshness.noteTask(taskId, workspaceId);
    const token = reviewStore.beginRefresh(taskId);
    signal.throwIfAborted();
    const response = await host.api.invokeAction<ReviewListResponse>(
      "change_requests.get",
      { ...(workspaceId ? { workspaceId } : {}), taskId },
      { signal },
    );
    signal.throwIfAborted();
    reviewStore.commit(
      taskId,
      token,
      (response.reviews ?? []).flatMap((review) => {
        const normalized = normalizeReview(options.providerId, review);
        return normalized ? [normalized] : [];
      }),
    );
  }

  async function refreshAssociations(workspaceId: string, signal: AbortSignal) {
    freshness.noteWorkspace(workspaceId);
    const token = associationStore.beginRefresh(workspaceId);
    signal.throwIfAborted();
    const response = await host.api.invokeAction<ReviewAssociationListResponse>(
      "change_requests.associations",
      { workspaceId },
      { signal },
    );
    signal.throwIfAborted();
    associationStore.commit(
      workspaceId,
      token,
      (response.associations ?? []).flatMap((association) => {
        const normalized = normalizeAssociation(options.providerId, association);
        return normalized ? [normalized] : [];
      }),
    );
  }

  async function refreshAfterMutation(workspaceId: string, taskId: string, signal: AbortSignal) {
    try {
      await Promise.all([
        refreshReviews(taskId, signal, workspaceId),
        refreshAssociations(workspaceId, signal),
      ]);
    } catch {
      // The mutation already succeeded. Its known outcome wins even when the
      // supplied signal cancels this optional refresh; the host can retry it.
    }
  }

  const repositoryProvider: RepositoryProviderRegistration = {
    id: options.providerId,
    label: options.label,
    ...(options.icon ? { icon: options.icon } : {}),
    ...(options.matchesURL ? { matchesURL: options.matchesURL } : {}),
    ...(options.supportsDraft === undefined ? {} : { supportsDraft: options.supportsDraft }),

    async listRepositories({ workspaceId, query = "", cursor = "", limit = 100, signal }) {
      signal.throwIfAborted();
      const response = await host.api.invokeAction<RepositoryPageResponse>(
        "repositories.list",
        { workspaceId, body: { query, cursor, limit } },
        { signal },
      );
      signal.throwIfAborted();
      return {
        repositories: (response.repositories ?? []).flatMap((repository) => {
          const normalized = normalizeRepository(options.providerId, repository);
          return normalized ? [normalized] : [];
        }),
        nextCursor: text(response.next_cursor) || undefined,
      };
    },

    async listBranches({ workspaceId, repository, signal }) {
      signal.throwIfAborted();
      const response = await host.api.invokeAction<BranchListResponse>(
        "repositories.branches",
        { workspaceId, body: { repository: credentialFreeRepository(repository) } },
        { signal },
      );
      signal.throwIfAborted();
      return (response.branches ?? []).flatMap((branch) => {
        const name = text(record(branch).name);
        return name ? [{ name }] : [];
      });
    },

    async inspectURL({ workspaceId, url, signal }) {
      signal.throwIfAborted();
      const response = await host.api.invokeAction<RepositoryInspectionResponse>(
        "repositories.inspect",
        { workspaceId, body: { url } },
        { signal },
      );
      signal.throwIfAborted();
      return normalizeRepository(options.providerId, response.repository);
    },

    async createChangeRequest({
      workspaceId,
      taskId,
      sessionId,
      repositoryId,
      title,
      body,
      baseBranch,
      draft,
      signal,
    }) {
      signal.throwIfAborted();
      const response = await host.api.invokeAction<CreateChangeRequestResponse>(
        "change_requests.create",
        {
          workspaceId,
          taskId,
          sessionId,
          repositoryId,
          body: {
            title,
            description: body,
            destination: baseBranch ?? "",
            draft,
          },
        },
        { signal },
      );
      const url = text(response.url);
      if (!url) throw new Error("source-control recipe: create response did not include a URL");
      await refreshAfterMutation(workspaceId, taskId, signal);
      const output = text(response.output);
      const associationError = text(response.association_error);
      return {
        url,
        provider: options.providerId,
        ...(output ? { output } : {}),
        ...(typeof response.linked === "boolean" ? { linked: response.linked } : {}),
        ...(associationError ? { associationError } : {}),
      };
    },
  };

  registry.registerRepositoryProvider(repositoryProvider);
  registry.registerTaskAction({
    id: `${options.providerId}-sync-change-requests`,
    label: `Sync ${options.label} ${options.changeRequestNoun}s`,
    ...(options.icon ? { icon: options.icon } : {}),
    placement: "link",
    singleTaskOnly: true,
    async run(context) {
      // The server drops this task's cached reads (branch lookup + linked
      // PR details) and re-runs the refresh, so a PR opened seconds ago is
      // discovered and auto-linked now instead of at the next TTL expiry.
      // Background refreshes stay fully cache-absorbed: freshness here is
      // user intent, paid once per press. TaskContext carries no abort
      // signal, so the trailing optional refresh runs on a fresh one.
      await host.api.invokeAction("change_requests.sync", {
        workspaceId: context.workspaceId,
        taskId: context.taskId,
      });
      await refreshAfterMutation(
        context.workspaceId,
        context.taskId,
        new AbortController().signal,
      );
    },
  });
  registry.registerTaskAction({
    id: `${options.providerId}-link-change-request`,
    label: `${options.label} ${options.changeRequestNoun}`,
    ...(options.icon ? { icon: options.icon } : {}),
    placement: "link",
    singleTaskOnly: true,
    async run(context) {
      const dialog = host.openTaskLinkDialog({
        title: `Link ${options.label} ${options.changeRequestNoun}`,
        description: `Enter a ${options.label} ${options.changeRequestNoun} URL or canonical reference.`,
        inputLabel: options.changeRequestNoun,
        emptyError: `Enter a valid ${options.label} ${options.changeRequestNoun} reference.`,
        failureMessage: `Failed to link ${options.label} ${options.changeRequestNoun}.`,
        successMessage: `${options.label} ${options.changeRequestNoun} linked`,
        inputTestId: `${options.providerId}-review-reference`,
        errorTestId: `${options.providerId}-review-reference-error`,
        submitTestId: `${options.providerId}-review-reference-submit`,
        async onSubmit(reference, signal) {
          signal.throwIfAborted();
          const parsed = options.parseReference(reference);
          if (!parsed) {
            throw new Error(`Enter a valid ${options.label} ${options.changeRequestNoun} reference.`);
          }
          await host.api.invokeAction(
            "change_requests.link",
            {
              workspaceId: context.workspaceId,
              taskId: context.taskId,
              body: { reference: parsed },
            },
            { signal },
          );
          await refreshAfterMutation(context.workspaceId, context.taskId, signal);
        },
      });
      overlays.add(dialog);
    },
  });
  registry.registerReviewProvider({
    id: options.providerId,
    label: options.label,
    ...(options.icon ? { icon: options.icon } : {}),
    changeRequestNoun: options.changeRequestNoun,
    order: options.order ?? 100,
    getSnapshot: (taskId) => reviewStore.get(taskId),
    subscribe: (taskId, listener) => reviewStore.subscribe(taskId, listener),
    refresh: (taskId, signal) => refreshReviews(taskId, signal),
    getAssociationSnapshot: (workspaceId) => associationStore.get(workspaceId),
    subscribeAssociations: (workspaceId, listener) =>
      associationStore.subscribe(workspaceId, listener),
    refreshAssociations,
    async unlink({
      workspaceId,
      taskId,
      connectionScope,
      repositoryId,
      changeRequestNumber,
      signal,
    }) {
      const number =
        typeof changeRequestNumber === "string" && /^\d+$/.test(changeRequestNumber)
          ? positiveInteger(Number(changeRequestNumber))
          : positiveInteger(changeRequestNumber);
      if (!connectionScope.trim() || !repositoryId.trim() || number === undefined) {
        throw new Error("source-control recipe: cannot unlink an incomplete review identity");
      }
      signal.throwIfAborted();
      await host.api.invokeAction(
        "change_requests.unlink",
        {
          workspaceId,
          taskId,
          body: {
            connection_scope: connectionScope,
            repository_id: repositoryId,
            number,
          },
        },
        { signal },
      );
    },
    ReviewPanel: (props) => {
      const review = reviewStore.get(props.taskId).find(
        (candidate) =>
          candidate.reviewKey === props.reviewKey &&
          candidate.connectionScope === props.connectionScope &&
          candidate.repositoryId === props.repositoryId &&
          String(candidate.changeRequestNumber) === String(props.changeRequestNumber),
      );
      const detail = review ? options.toChangeRequestDetail(review) : null;
      if (!options.autoFix) {
        return host.jsx(host.ui.ChangeRequestDetail, {
          detail,
          presentation: props.presentation,
          loading: false,
          error: null,
        });
      }
      // The auto-fix surface is a hook-driven panel: a header action
      // (start/stop) plus a status notice that polls the plugin's
      // zero-provider-traffic status action while mounted. Freshness for
      // the rest of the panel still flows through the host's refreshes.
      return host.jsx(createAutoFixPanel(host, options.autoFix), {
        detail,
        presentation: props.presentation,
        workspaceId: props.workspaceId,
        taskId: props.taskId,
        connectionScope: props.connectionScope,
        repositoryId: props.repositoryId,
        changeRequestNumber: props.changeRequestNumber,
      });
    },
  });
  return {
    destroy() {
      freshness.stop();
      overlays.forEach((overlay) => overlay.close());
      overlays.clear();
      reviewStore.clear();
      associationStore.clear();
    },
  };
}

type AutoFixPanelProps = {
  detail: unknown;
  presentation: "desktop" | "mobile";
  workspaceId: string;
  taskId: string;
  connectionScope: string;
  repositoryId: string;
  changeRequestNumber: string | number;
};

/** Milliseconds between status polls. While a loop runs this is a host-only
 * read (the plugin serves it from Host state — no provider traffic); while
 * idle it is a slow heartbeat. */
const AUTO_FIX_POLL_RUNNING_MS = 2000;
const AUTO_FIX_POLL_IDLE_MS = 15000;

function createAutoFixPanel(host: PluginHostApi, autoFix: SourceControlAutoFixOptions) {
  const actionKey = autoFix.actionKey ?? "change_requests.auto_fix";
  const statusKey = autoFix.statusKey ?? "change_requests.auto_fix_status";
  const startLabel = autoFix.label ?? "Auto-fix CI";
  const stopLabel = autoFix.stopLabel ?? "Stop auto-fix";
  const { useState, useEffect, useCallback } = host.React;

  return function AutoFixPanel(props: AutoFixPanelProps) {
    const [report, setReport] = useState<AutoFixReport | undefined>(undefined);
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState<string | null>(null);

    const body = {
      connection_scope: props.connectionScope,
      repository_id: props.repositoryId,
      number:
        typeof props.changeRequestNumber === "string" && /^\d+$/.test(props.changeRequestNumber)
          ? Number(props.changeRequestNumber)
          : props.changeRequestNumber,
    };

    useEffect(() => {
      let cancelled = false;
      let timer: ReturnType<typeof setTimeout> | undefined;
      const poll = async () => {
        try {
          const response = await host.api.invokeAction<AutoFixStatusResponse>(
            statusKey,
            { workspaceId: props.workspaceId, taskId: props.taskId, body },
          );
          if (cancelled) return;
          const next = normalizeAutoFixReport(response.auto_fix);
          setReport(next);
          timer = setTimeout(poll, next?.running ? AUTO_FIX_POLL_RUNNING_MS : AUTO_FIX_POLL_IDLE_MS);
        } catch {
          if (cancelled) return;
          timer = setTimeout(poll, AUTO_FIX_POLL_IDLE_MS);
        }
      };
      void poll();
      return () => {
        cancelled = true;
        if (timer !== undefined) clearTimeout(timer);
      };
      // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [statusKey, props.workspaceId, props.taskId, props.connectionScope, props.repositoryId, props.changeRequestNumber]);

    const onAction = useCallback(async (request: { actionId?: string }) => {
      if (request?.actionId !== "auto_fix") return;
      setBusy(true);
      setError(null);
      try {
        const response = await host.api.invokeAction<AutoFixStatusResponse>(
          actionKey,
          {
            workspaceId: props.workspaceId,
            taskId: props.taskId,
            body: { ...body, command: report?.running ? "stop" : "start" },
          },
        );
        const next = normalizeAutoFixReport(response.auto_fix);
        if (next) setReport(next);
      } catch (cause) {
        setError(cause instanceof Error ? cause.message : String(cause));
      } finally {
        setBusy(false);
      }
      // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [actionKey, props.workspaceId, props.taskId, props.connectionScope, props.repositoryId, props.changeRequestNumber, report?.running]);

    const running = report?.running === true;
    const unsupported = report?.status === "unsupported";
    const actions = unsupported
      ? undefined
      : [
          {
            id: "auto_fix",
            label: running ? stopLabel : startLabel,
            pendingLabel: running ? "Stopping…" : "Starting…",
            placement: "header",
            tone: running ? "secondary" : "default",
            disabled: report === undefined || busy,
            busy,
          },
        ];

    const noticeText = error ?? (report ? autoFixNoticeText(report) : null);
    const notice =
      unsupported || noticeText === null
        ? undefined
        : host.jsx(host.ui.Alert, {
            children: host.jsx(host.ui.AlertDescription, { children: noticeText }),
          });

    return host.jsx(host.ui.ChangeRequestDetail, {
      detail: props.detail,
      presentation: props.presentation,
      loading: false,
      error: null,
      actions,
      busyActionId: busy ? "auto_fix" : null,
      onAction,
      notice,
    });
  };
}
