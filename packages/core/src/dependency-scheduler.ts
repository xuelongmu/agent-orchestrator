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
 * True when a session's work is complete enough to unblock its dependents —
 * its PR has merged. `lifecycle.pr.state` stays "merged" through the post-merge
 * cleanup window (and even after the session is torn down but before it is
 * archived), so this remains a reliable signal across several poll cycles.
 */
export function isPrerequisiteSatisfied(session: Session): boolean {
  return session.lifecycle.pr.state === "merged";
}

/**
 * The set of dependency identifiers satisfied by merged sessions, split by how
 * uniquely each identifier resolves:
 *  - `sessionIds`: session ids are globally unique (`{prefix}-{num}`), so a
 *    merged session satisfies a matching `blockedBy` entry in **any** project —
 *    this is the unambiguous cross-project handle.
 *  - `issueIdsByProject`: issue ids are only unique within a project (GitHub /
 *    GitLab expose `#20` as "20" in every repo), so a merged session's issue id
 *    may only satisfy a dependent **in the same project**. Matching globally
 *    would let repo B's issue 20 wrongly unblock a repo A dependent on its own
 *    issue 20.
 */
export interface SatisfiedDependencies {
  sessionIds: Set<string>;
  issueIdsByProject: Map<string, Set<string>>;
}

/** Collect the satisfied dependency identifiers from merged sessions. */
export function collectSatisfiedDependencies(sessions: Session[]): SatisfiedDependencies {
  const sessionIds = new Set<string>();
  const issueIdsByProject = new Map<string, Set<string>>();
  for (const session of sessions) {
    if (!isPrerequisiteSatisfied(session)) continue;
    const sessionId = normalizeDependencyId(session.id);
    if (sessionId) sessionIds.add(sessionId);
    if (session.issueId) {
      const issueId = normalizeDependencyId(session.issueId);
      if (issueId) {
        let perProject = issueIdsByProject.get(session.projectId);
        if (!perProject) {
          perProject = new Set();
          issueIdsByProject.set(session.projectId, perProject);
        }
        perProject.add(issueId);
      }
    }
  }
  return { sessionIds, issueIdsByProject };
}

/**
 * True when a single `blockedBy` entry of a dependent in `projectId` is
 * satisfied — either by a merged session id (any project) or by a merged issue
 * id within the same project.
 */
export function isDependencySatisfied(
  entry: string,
  projectId: string,
  satisfied: SatisfiedDependencies,
): boolean {
  const normalized = normalizeDependencyId(entry);
  if (satisfied.sessionIds.has(normalized)) return true;
  return satisfied.issueIdsByProject.get(projectId)?.has(normalized) ?? false;
}

/**
 * Narrow a held session's `blockedBy` to the entries still unsatisfied. The
 * dependent stays blocked until every prerequisite is satisfied (the returned
 * array is empty).
 */
export function computeRemainingBlockedBy(
  blockedBy: string[],
  projectId: string,
  satisfied: SatisfiedDependencies,
): string[] {
  return blockedBy.filter((id) => !isDependencySatisfied(id, projectId, satisfied));
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
