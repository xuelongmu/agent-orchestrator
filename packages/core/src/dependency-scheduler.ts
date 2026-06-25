/**
 * Dependency-aware scheduler helpers (issue #10).
 *
 * Pure logic backing the lifecycle manager's per-poll scheduler pass: deciding
 * which prerequisite work has been satisfied (its PR merged), narrowing the
 * `blockedBy` set of held dependents, and counting active sessions for the
 * per-project concurrency cap before launching newly-unblocked sessions.
 *
 * Kept side-effect-free so it can be unit-tested without the lifecycle harness.
 * The orchestration (persisting, launching) lives in lifecycle-manager.ts.
 */

import { TERMINAL_STATUSES, type Session } from "./types.js";
import { isBlockedByDependency } from "./lifecycle-state.js";

/**
 * Normalize a dependency identifier for comparison: trim, drop a leading "#"
 * (issue-number sugar), and lowercase. Lets "#9" match "9" and session ids
 * compare case-insensitively. `blockedBy` entries and the ids a satisfied
 * session exposes are both normalized before matching.
 */
export function normalizeDependencyId(id: string): string {
  return id.trim().replace(/^#/, "").toLowerCase();
}

/**
 * The identifiers a session satisfies for its dependents once its PR has
 * merged: its session id and its issue id (if any), both normalized. A
 * dependent's `blockedBy` may reference either form.
 */
export function sessionSatisfiedIds(session: Session): string[] {
  const ids = [session.id, ...(session.issueId ? [session.issueId] : [])];
  return ids.map(normalizeDependencyId).filter((id) => id.length > 0);
}

/**
 * True when a session's work is complete enough to unblock its dependents —
 * its PR has merged. `lifecycle.pr.state` stays "merged" through the post-merge
 * cleanup window (and even after the session is torn down but before it is
 * archived), so this remains a reliable signal across several poll cycles.
 */
export function isPrerequisiteSatisfied(session: Session): boolean {
  return session.lifecycle.pr.state === "merged";
}

/**
 * Collect the normalized set of dependency ids satisfied by the given sessions
 * (those whose PR has merged). Cross-project by construction — the supervisor
 * passes sessions from every project.
 */
export function collectSatisfiedDependencyIds(sessions: Session[]): Set<string> {
  const satisfied = new Set<string>();
  for (const session of sessions) {
    if (!isPrerequisiteSatisfied(session)) continue;
    for (const id of sessionSatisfiedIds(session)) satisfied.add(id);
  }
  return satisfied;
}

/**
 * Narrow a held session's `blockedBy` to the entries still unsatisfied. The
 * dependent stays blocked until every prerequisite is satisfied (the returned
 * array is empty).
 */
export function computeRemainingBlockedBy(
  blockedBy: string[],
  satisfied: Set<string>,
): string[] {
  return blockedBy.filter((id) => !satisfied.has(normalizeDependencyId(id)));
}

/**
 * Count the sessions occupying a concurrency slot in a project: launched (not
 * held in the blocked pre-state) and not yet terminal. Orchestrator sessions
 * are excluded — the cap governs concurrent worker agents.
 */
export function countActiveSessions(sessions: Session[], projectId: string): number {
  return sessions.filter(
    (s) =>
      s.projectId === projectId &&
      s.metadata["role"] !== "orchestrator" &&
      !s.id.endsWith("-orchestrator") &&
      !TERMINAL_STATUSES.has(s.status) &&
      !isBlockedByDependency(s.lifecycle),
  ).length;
}
