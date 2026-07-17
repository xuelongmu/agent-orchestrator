import { readAgentReport, AGENT_REPORT_METADATA_KEYS } from "./agent-report.js";
import { mutateMetadata } from "./metadata.js";
import { getProjectSessionsDir } from "./paths.js";
import { isDecisionReportActive, isDecisionReportState } from "./report-watcher.js";
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
  /**
   * When the session entered the needs_input episode it is currently parked in.
   * Edge-triggered: stamped on entry, cleared on exit, and NEVER refreshed while
   * parked — that stability is the whole point, and is why `lastTransitionAt`
   * cannot serve here (the poll re-commits `sessionState: "needs_input"` on every
   * waiting_input cycle, so it is rewritten every few seconds).
   */
  EPISODE_AT: "notifyDecisionEpisodeAt",
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

/** True while the session is parked on a human decision. */
export function isParkedOnDecision(session: Session): boolean {
  return session.lifecycle?.session.state === "needs_input";
}

/**
 * Edge-triggered patch maintaining {@link NOTIFY_DECISION_METADATA_KEYS.EPISODE_AT},
 * or `null` when nothing changes. Stamps on entry into needs_input and clears on
 * exit, so the marker stays fixed for the whole episode.
 */
export function decisionEpisodePatch(
  session: Session,
  nowIso: string,
): Record<string, string> | null {
  const parked = isParkedOnDecision(session);
  const stamped = session.metadata?.[NOTIFY_DECISION_METADATA_KEYS.EPISODE_AT] ?? "";
  if (parked && !stamped) return { [NOTIFY_DECISION_METADATA_KEYS.EPISODE_AT]: nowIso };
  if (!parked && stamped) return { [NOTIFY_DECISION_METADATA_KEYS.EPISODE_AT]: "" };
  return null;
}

/**
 * Identity of the session's currently active decision instance, or `null` when
 * no decision is live.
 *
 * The identity pairs the decision report's timestamp with the needs_input EPISODE
 * that report activated. Both halves are required, because neither alone is
 * sufficient:
 *
 * - The report timestamp alone survives its own decision. If decision A resolves
 *   without a new `ao report` and the agent stops at an unrelated bare prompt B
 *   within the report's freshness window, A's report is still present and still
 *   "active" (B re-parks the session in needs_input), so A's unused token would
 *   answer B.
 * - The episode alone cannot distinguish two successive reports within one
 *   episode.
 *
 * Pairing them means a token is honoured only inside the episode it was minted
 * for: B is a new episode, so A's token no longer matches.
 */
export function activeDecisionId(session: Session): string | null {
  const report = readAgentReport(session.metadata);
  if (!isDecisionReportActive(session, report) || !report) return null;
  const episodeAt = session.metadata?.[NOTIFY_DECISION_METADATA_KEYS.EPISODE_AT] ?? "";
  // No episode marker means the session is not parked on a decision a human can
  // answer, so there is nothing to bind a mutating action to.
  if (!episodeAt) return null;
  return `${report.timestamp}:${episodeAt}`;
}

/**
 * The decision identity as recorded in a raw metadata record — the same value
 * {@link activeDecisionId} derives, computed from stored fields alone.
 *
 * This is what makes the identity checkable INSIDE the metadata lock, where only
 * the stored record is in hand. It intentionally omits the liveness half of
 * {@link isDecisionReportActive} (which needs the enriched lifecycle): the episode
 * marker is itself cleared when the session leaves needs_input, so its presence
 * already implies the decision is live.
 */
export function storedDecisionId(raw: Record<string, string>): string | null {
  const state = raw[AGENT_REPORT_METADATA_KEYS.STATE];
  const at = raw[AGENT_REPORT_METADATA_KEYS.AT];
  const episodeAt = raw[NOTIFY_DECISION_METADATA_KEYS.EPISODE_AT];
  if (!state || !at || !episodeAt) return null;
  if (!isDecisionReportState(state)) return null;
  return `${at}:${episodeAt}`;
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
    // Re-check the identity INSIDE the lock. The caller validated the nonce
    // against a session it read earlier; if a new decision was reported in
    // between, that validation is already stale and this claim would otherwise
    // stamp the old id on top of the new decision and dispatch into it.
    if (storedDecisionId(existing) !== decisionId) {
      claimed = false;
      return existing;
    }
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
 * Retire a spent decision, but only if the stored record is still the one the
 * caller observed.
 *
 * The lifecycle poll decides a decision is spent from an in-memory session
 * snapshot taken at the top of the cycle. If the agent writes a fresh
 * `ao report` after that load, an unconditional clear would delete the NEW
 * decision — losing it and its callback identity. Comparing the stored state and
 * timestamp under the lock means a superseded observation simply no-ops.
 *
 * Returns `true` when the record was cleared.
 */
export function clearSpentDecision(
  projectId: string,
  sessionId: SessionId,
  observed: { state: string; at: string },
): boolean {
  let cleared = false;
  mutateMetadata(getProjectSessionsDir(projectId), sessionId, (existing) => {
    if (
      existing[AGENT_REPORT_METADATA_KEYS.STATE] !== observed.state ||
      existing[AGENT_REPORT_METADATA_KEYS.AT] !== observed.at
    ) {
      cleared = false;
      return existing;
    }
    cleared = true;
    return { ...existing, ...clearedDecisionMetadata() };
  });
  return cleared;
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
    [NOTIFY_DECISION_METADATA_KEYS.EPISODE_AT]: "",
  };
}
