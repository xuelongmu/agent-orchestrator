/**
 * Shared validation utilities.
 */

import type { SessionStatus } from "../types.js";

/** Valid session statuses for validation. */
const VALID_STATUSES: ReadonlySet<string> = new Set([
  "spawning",
  "working",
  "detecting",
  "pr_open",
  "ci_failed",
  "review_pending",
  "changes_requested",
  "approved",
  "mergeable",
  "merged",
  "cleanup",
  "needs_input",
  "stuck",
  "errored",
  "killed",
  "idle",
  "done",
  "terminated",
]);

/** Safely parse JSON, returning null on failure. */
export function safeJsonParse<T>(str: string): T | null {
  try {
    return JSON.parse(str) as T;
  } catch {
    return null;
  }
}

/**
 * Parse a comma-separated id list (e.g. dependsOn/blockedBy) into a trimmed,
 * de-duplicated array. Returns an empty array for missing/empty input.
 */
export function parseIdList(raw: string | undefined): string[] {
  if (!raw) return [];
  const seen = new Set<string>();
  for (const part of raw.split(",")) {
    const id = part.trim();
    if (id) seen.add(id);
  }
  return [...seen];
}

/**
 * Serialize an id list to a comma-separated string for metadata storage.
 * Empty/undefined input → "" so the metadata key is dropped on write.
 */
export function serializeIdList(ids: string[] | undefined): string {
  return ids && ids.length > 0 ? parseIdList(ids.join(",")).join(",") : "";
}

/** Validate and normalize a status string. */
export function validateStatus(raw: string | undefined): SessionStatus {
  // Bash scripts write "starting" — treat as "working"
  if (raw === "starting") return "working";
  if (raw && VALID_STATUSES.has(raw)) return raw as SessionStatus;
  return "spawning";
}
