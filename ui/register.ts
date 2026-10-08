// GitHub Lite — the frontend half of kandev-plugin-github-lite.
//
// Registers the vendored source-control recipe (recipes/source-control/ui)
// as the "github-lite" repository provider: GitHub repositories in the
// task-create-from-URL and repository pickers, the Link dialog for pull
// requests, PR references in the composer, and the review provider that
// puts PR status on tasks (card indicator, top bar, CI chip, detail panel).
//
// This file is bundled to ui/bundle.js (`npm run bundle:ui` / `make
// package`); the host loads it as a native ES module and calls
// window.registerKandevPlugin with the manifest's id.

import {
  registerSourceControlRecipe,
  type RecipeReviewSummary,
  type SourceControlRecipeLifecycle,
} from "../recipes/source-control/ui/register";
import type { PluginHostApi, PluginRegistry } from "@kandev/plugin-sdk";

const providerId = "github-lite";

/** Cheap URL hint only; the server's inspectURL is the ownership authority. */
function matchesGitHubURL(url: string): boolean {
  return /^https?:\/\/(?:www\.)?github\.com\//i.test(url.trim());
}

const PULL_URL_PATTERN =
  /^(?:https?:\/\/)?(?:www\.)?github\.com\/([A-Za-z0-9_.-]+)\/([A-Za-z0-9_.-]+)\/pull\/([0-9]+)\/?(?:#.*)?$/;

/** Normalizes a pasted PR reference to "owner/repo#123" (or null). */
function parseGitHubPullReference(reference: string): string | null {
  const trimmed = reference.trim();
  const match = PULL_URL_PATTERN.exec(trimmed);
  if (match) {
    return `${match[1]}/${match[2]}#${match[3]}`;
  }
  const slugPattern = /^([A-Za-z0-9_.-]+)\/([A-Za-z0-9_.-]+)#([0-9]+)$/;
  const slug = slugPattern.exec(trimmed);
  if (slug) {
    return `${slug[1]}/${slug[2]}#${slug[3]}`;
  }
  return null;
}

/**
 * Folds a review (with its server-carried detail document) into the host
 * panel's detail model. The server already builds the full
 * ChangeRequestDetailModel-shaped payload; the lean fallback only exists
 * for reviews that arrived without one (never in practice).
 */
function toGitHubPullDetail(review: RecipeReviewSummary): unknown {
  if (review.detail) {
    return {
      ...review.detail,
      // Identity surfaces stay authoritative from the normalized review,
      // never from the passthrough document.
      providerId,
      reviewKey: review.reviewKey,
      number: review.changeRequestNumber,
      url: review.url,
      title: review.title,
      connectionScope: review.connectionScope,
      repositoryId: review.repositoryId,
      ...(review.taskStatus ? { state: review.taskStatus.state } : {}),
    };
  }
  return {
    providerId,
    reviewKey: review.reviewKey,
    number: review.changeRequestNumber,
    title: review.title,
    url: review.url,
    state: review.state,
    connectionScope: review.connectionScope,
    repositoryId: review.repositoryId,
    ...(review.taskStatus ? { state: review.taskStatus.state } : {}),
  };
}

declare global {
  interface Window {
    registerKandevPlugin(
      id: string,
      registration: { initialize(registry: PluginRegistry, host: PluginHostApi): void; destroy(): void },
    ): void;
  }
}

let lifecycle: SourceControlRecipeLifecycle | undefined;

window.registerKandevPlugin("kandev-plugin-github-lite", {
  initialize(registry, host) {
    lifecycle = registerSourceControlRecipe(registry, host, {
      providerId,
      label: "GitHub",
      icon: "globe",
      changeRequestNoun: "pull request",
      order: 90,
      supportsDraft: true,
      matchesURL: matchesGitHubURL,
      parseReference: parseGitHubPullReference,
      toChangeRequestDetail: toGitHubPullDetail,
    });
  },
  destroy() {
    lifecycle?.destroy();
    lifecycle = undefined;
  },
});
