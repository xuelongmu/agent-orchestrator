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
 *  - `issueIdsByProject`: bare issue ids are only unique within a project (GitHub
 *    / GitLab expose `#20` as "20" in every repo), so a merged session's issue id
 *    may only satisfy a dependent **in the same project**. Matching globally
 *    would let repo B's issue 20 wrongly unblock a repo A dependent on its own
 *    issue 20.
 *  - `repoQualifiedIssueIds`: a repo-qualified issue ref ("owner/repo#N") IS
 *    globally unique, so — like a session id — it may satisfy a dependent in
 *    **any** project. `ao plan` emits cross-repo blockers in this form, so this
 *    is what lets an ordered multi-repo plan actually unblock across repos.
 *  - `globalIssueIds`: workspace-scoped tracker keys (e.g. Linear's "ENG-1") are
 *    globally unique across the workspace — unlike repo-scoped issue numbers,
 *    they carry a team prefix — so they may satisfy a dependent in **any**
 *    project. They are recognized by being non-numeric (GitHub/GitLab ids are
 *    bare numbers); `ao plan` emits cross-project Linear blockers as bare keys,
 *    so this is what lets a multi-project Linear plan unblock across projects.
 */
export interface SatisfiedDependencies {
  sessionIds: Set<string>;
  issueIdsByProject: Map<string, Set<string>>;
  repoQualifiedIssueIds: Set<string>;
  globalIssueIds: Set<string>;
}

/**
 * True for a workspace-scoped tracker key (e.g. Linear's "ENG-1"): globally
 * unique because it carries a non-numeric team prefix. Repo-scoped GitHub/GitLab
 * issue ids are bare numbers and are NOT globally unique. `id` is expected to be
 * already normalized (lowercased, leading "#"/repo prefix removed).
 */
function isWorkspaceScopedIssueId(id: string): boolean {
  return id.length > 0 && !/^\d+$/.test(id);
}

/**
 * Collect the satisfied dependency identifiers from merged sessions.
 *
 * `projectRepo` maps a session's project id to that project's `owner/repo` (the
 * repo the tracker issue lives in). It is used only to build the globally-unique
 * `owner/repo#N` form. We deliberately key on the issue's *project repo*, not on
 * every repo the session opened a PR in: an issue number is repo-local, so an
 * auxiliary PR a session opened in another repo must not register that repo's
 * `repo#N` as satisfied (it would wrongly unblock a dependent on a different
 * issue N in that other repo).
 */
export function collectSatisfiedDependencies(
  sessions: Session[],
  projectRepo?: (projectId: string) => string | undefined,
): SatisfiedDependencies {
  const sessionIds = new Set<string>();
  const issueIdsByProject = new Map<string, Set<string>>();
  const repoQualifiedIssueIds = new Set<string>();
  const globalIssueIds = new Set<string>();
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
        if (isWorkspaceScopedIssueId(issueId)) {
          globalIssueIds.add(issueId);
        } else {
          const repo = projectRepo?.(session.projectId);
          if (repo) {
            repoQualifiedIssueIds.add(normalizeDependencyId(`${repo}#${issueId}`));
          }
        }
      }
    }
  }
  return { sessionIds, issueIdsByProject, repoQualifiedIssueIds, globalIssueIds };
}

/**
 * True when a single `blockedBy` entry of a dependent in `projectId` is
 * satisfied — by a merged session id (any project), a merged repo-qualified
 * issue ref "owner/repo#N" (any project, globally unique), a merged
 * workspace-scoped key like "ENG-1" (any project, globally unique), or a merged
 * bare repo-scoped issue id within the same project.
 */
export function isDependencySatisfied(
  entry: string,
  projectId: string,
  satisfied: SatisfiedDependencies,
): boolean {
  const normalized = normalizeDependencyId(entry);
  if (satisfied.sessionIds.has(normalized)) return true;
  if (normalized.includes("/")) {
    return satisfied.repoQualifiedIssueIds.has(normalized);
  }
  if (isWorkspaceScopedIssueId(normalized)) {
    return satisfied.globalIssueIds.has(normalized);
  }
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
 * held in the blocked pre-state) workers whose agent process is still alive.
 * Orchestrator sessions are excluded — the cap governs concurrent worker agents.
 *
 * A `merged` session is terminal for status purposes, but its worker keeps
 * running through the post-merge auto-cleanup grace window (cleanup is deferred
 * while the agent is active / waiting for input). It must still count against
 * the cap — otherwise `maxConcurrent: 1` would launch a dependent while the
 * merged agent is still alive, breaking the bound on concurrent workers. Once
 * cleanup completes the status moves off `merged` (to killed/done/cleanup) and
 * it stops counting.
 */
export function countActiveSessions(sessions: Session[], projectId: string): number {
  return sessions.filter(
    (s) =>
      s.projectId === projectId &&
      s.metadata["role"] !== "orchestrator" &&
      !s.id.endsWith("-orchestrator") &&
      (!TERMINAL_STATUSES.has(s.status) || s.status === "merged") &&
      !isBlockedByDependency(s.lifecycle),
  ).length;
}
