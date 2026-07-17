import { readAgentReport, AGENT_REPORT_METADATA_KEYS } from "./agent-report.js";
import { mutateMetadata } from "./metadata.js";
import { getProjectSessionsDir } from "./paths.js";
import { isDecisionReportActive } from "./report-watcher.js";
import { NOTIFY_CALLBACK_ACTIONS, type NotifyCallbackAction } from "./notify-callback.js";
import type { Session, SessionId } from "./types.js";

/**
 * The decision instance behind an actionable notification (#13).
 *
 * ONE invariant, enforced here and nowhere else:
 *
 *   A mutating callback is valid only for the project-scoped, currently active
 *   decision instance. Approve/Deny/Kill atomically move that durable instance
 *   from pending to consumed before dispatch; Nudge does not consume it. A
 *   report identity belongs only to the needs_input transition that activated
 *   it.
 *
 * Both the signer (core, minting buttons) and the verifier (the web callback
 * route) derive the identity through {@link activeDecisionId}, so a token can
 * only ever be honoured against the very decision it was minted for.
 *
 * Consumption lives in the session's own metadata, which is project-scoped
 * (`{dataDir}/{hash}-{projectId}/sessions/{id}`) and written under a file lock.
 * That makes it durable across dashboard restarts and atomic against concurrent
 * taps — a process-local map was neither.
 */

export const NOTIFY_DECISION_METADATA_KEYS = {
  /** Identity of the decision instance already resolved by a callback action. */
  CONSUMED_ID: "notifyDecisionConsumedId",
} as const;

/**
 * Callback actions that RESOLVE a decision, and so may fire at most once for it.
 *
 * Nudge is deliberately absent: it only asks the agent for a status update and
 * leaves the underlying choice outstanding, so consuming the instance on a nudge
 * would strand a still-pending decision with no way to answer it.
 */
export const RESOLVING_CALLBACK_ACTIONS = NOTIFY_CALLBACK_ACTIONS.filter(
  (action) => action !== "nudge",
);

export function isResolvingCallbackAction(action: NotifyCallbackAction): boolean {
  return (RESOLVING_CALLBACK_ACTIONS as readonly string[]).includes(action);
}

/**
 * Identity of the session's currently active decision instance, or `null` when
 * no decision is live.
 *
 * The identity is the decision report's timestamp, but ONLY while that report is
 * still an active block ({@link isDecisionReportActive}). Once the agent resolves
 * a decision and resumes, the lifecycle poll clears the spent report, so a later
 * unrelated prompt yields no identity (or a different one) and a token minted for
 * the earlier decision can never answer it.
 */
export function activeDecisionId(session: Session): string | null {
  const report = readAgentReport(session.metadata);
  if (!isDecisionReportActive(session, report)) return null;
  return report?.timestamp ?? null;
}

/**
 * Atomically move a decision instance from pending to consumed.
 *
 * Returns `true` for the caller that claimed it and `false` for every later or
 * concurrent attempt on the same instance. The read-modify-write runs under the
 * metadata file lock, so two simultaneous taps cannot both claim.
 */
export function consumeDecision(
  projectId: string,
  sessionId: SessionId,
  decisionId: string,
): boolean {
  let claimed = false;
  mutateMetadata(getProjectSessionsDir(projectId), sessionId, (existing) => {
    if (existing[NOTIFY_DECISION_METADATA_KEYS.CONSUMED_ID] === decisionId) {
      claimed = false;
      return existing;
    }
    claimed = true;
    return { ...existing, [NOTIFY_DECISION_METADATA_KEYS.CONSUMED_ID]: decisionId };
  });
  return claimed;
}

/**
 * Hand a consumed instance back, so a genuinely failed dispatch stays retryable.
 * Only releases the instance named by `decisionId`, so it can never reopen a
 * different decision that was claimed in the meantime.
 */
export function releaseDecision(
  projectId: string,
  sessionId: SessionId,
  decisionId: string,
): void {
  mutateMetadata(getProjectSessionsDir(projectId), sessionId, (existing) =>
    existing[NOTIFY_DECISION_METADATA_KEYS.CONSUMED_ID] === decisionId
      ? { ...existing, [NOTIFY_DECISION_METADATA_KEYS.CONSUMED_ID]: "" }
      : existing,
  );
}

/**
 * Metadata patch that retires a spent decision instance: the report identity
 * itself plus its consumption marker. Applied by the lifecycle poll when a
 * decision stops being active, so the next decision starts from a clean slate.
 */
export function clearedDecisionMetadata(): Record<string, string> {
  return {
    [AGENT_REPORT_METADATA_KEYS.STATE]: "",
    [AGENT_REPORT_METADATA_KEYS.AT]: "",
    [AGENT_REPORT_METADATA_KEYS.QUESTION]: "",
    [AGENT_REPORT_METADATA_KEYS.CONFIDENCE]: "",
    [NOTIFY_DECISION_METADATA_KEYS.CONSUMED_ID]: "",
  };
}
