import { readAgentReport, AGENT_REPORT_METADATA_KEYS } from "./agent-report.js";
import { mutateMetadata } from "./metadata.js";
import { getProjectSessionsDir } from "./paths.js";
import { isDecisionReportActive, isDecisionReportState } from "./report-watcher.js";
import { NOTIFY_CALLBACK_ACTIONS, type NotifyCallbackAction } from "./notify-callback.js";
import { ACTIVITY_STATE, type Session, type SessionId } from "./types.js";

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
 *
 * On EXIT the whole decision record is retired, not just the marker. A report
 * that outlives the episode it activated is spent by definition, and keeping it
 * would let the next episode inherit it: a bare prompt B would pair B's new
 * marker with A's retained timestamp, read as report-backed, and mint fresh
 * Approve/Deny/Kill buttons for a prompt no agent ever reported. Retiring on the
 * observed exit is what keeps a report bound to the episode it activated.
 *
 * A decision that has not been parked yet has no marker, so this never touches a
 * fresh report whose lifecycle state has not caught up (#12's pr_open ordering).
 */
export type DecisionEpisodeTransition =
  /** Entering needs_input: stamp the marker. A plain write is safe — it adds. */
  | { kind: "stamp"; patch: Record<string, string> }
  /**
   * Leaving needs_input: retire the whole record. Deliberately NOT a patch — the
   * clear must go through {@link clearSpentDecision}'s locked compare, because an
   * unconditional write here would erase a report the agent may have written
   * since the caller's snapshot.
   */
  | { kind: "retire" }
  | null;

export function decisionEpisodeTransition(
  session: Session,
  nowIso: string,
): DecisionEpisodeTransition {
  const parked = isParkedOnDecision(session);
  const stamped = session.metadata?.[NOTIFY_DECISION_METADATA_KEYS.EPISODE_AT] ?? "";
  if (parked && !stamped) {
    return { kind: "stamp", patch: { [NOTIFY_DECISION_METADATA_KEYS.EPISODE_AT]: nowIso } };
  }
  if (!parked && stamped) return { kind: "retire" };
  return null;
}

/**
 * Whether a non-consuming action (Nudge) must be REFUSED for `decisionId`, read
 * under the metadata lock so the answer reflects the stored record at dispatch
 * time rather than a snapshot taken earlier in the request.
 *
 * Refuse in either case:
 *  - The stored identity is missing or no longer equals `decisionId`. A new report
 *    written between the route's `get()` and this lock supersedes the decision the
 *    token names, and a stale Nudge ("continue if you can") must not be delivered
 *    into the successor. This mirrors the in-lock identity check
 *    {@link consumeDecision} uses for resolving actions, so both the consuming and
 *    non-consuming paths authorize against the same durable current identity at
 *    the same last stateful boundary.
 *  - A resolving action has already consumed this exact decision (Deny→Nudge).
 */
export function isNudgeBlocked(
  projectId: string,
  sessionId: SessionId,
  decisionId: string,
): boolean {
  let blocked = false;
  mutateMetadata(getProjectSessionsDir(projectId), sessionId, (existing) => {
    const supersededOrGone = storedDecisionId(existing) !== decisionId;
    const alreadyResolved = existing[NOTIFY_DECISION_METADATA_KEYS.CONSUMED_ID] === decisionId;
    blocked = supersededOrGone || alreadyResolved;
    return existing;
  });
  return blocked;
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
  // A decision is answerable only while it is live in BOTH durable identity and
  // current live activity. Live "active" activity means the agent has RESUMED
  // work, which is authoritative even when the persisted canonical state still
  // lags at needs_input — so a destructive Approve/Deny/Kill cannot land on
  // already-resumed work before the next lifecycle poll persists the transition.
  // Parked/ready/idle activities keep a legitimately-waiting decision answerable.
  // At mint the session is parked (waiting_input is why it is a needs_input
  // decision), so this never suppresses a genuine control. (#13 review)
  if (session.activity === ACTIVITY_STATE.ACTIVE) return null;
  const report = readAgentReport(session.metadata);
  if (!isDecisionReportActive(session, report) || !report) return null;
  const episodeAt = session.metadata?.[NOTIFY_DECISION_METADATA_KEYS.EPISODE_AT] ?? "";
  // No episode marker means the session is not parked on a decision a human can
  // answer, so there is nothing to bind a mutating action to.
  if (!episodeAt) return null;
  return `${report.timestamp}:${episodeAt}`;
}

/**
 * Canonicalize a stored report timestamp with the SAME rule `readAgentReport`
 * applies (`Date.parse` → `toISOString`), or `null` when unparseable. Every
 * identity comparison must go through this so a valid but non-canonical spelling
 * (e.g. `2026-07-17T12:34:56Z`) matches the value the live report derives.
 */
export function normalizeReportTimestamp(at: string | undefined): string | null {
  if (!at) return null;
  const parsed = Date.parse(at);
  if (Number.isNaN(parsed)) return null;
  return new Date(parsed).toISOString();
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
 *
 * The timestamp is normalized via {@link normalizeReportTimestamp}, so a stored
 * `at` that is valid but not already in `toISOString` form still equals the signed
 * nonce (which `activeDecisionId` derives from the report `readAgentReport`
 * returns) instead of rejecting every action with 409.
 */
export function storedDecisionId(raw: Record<string, string>): string | null {
  const state = raw[AGENT_REPORT_METADATA_KEYS.STATE];
  const at = normalizeReportTimestamp(raw[AGENT_REPORT_METADATA_KEYS.AT]);
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
 * Reopen a consumed decision so a legitimate retry can re-claim and re-dispatch.
 *
 * Only used for a PROVABLY pre-delivery dispatch failure (see
 * {@link import("./types.js").SessionSendNotDeliveredError}), never after the
 * runtime delivery boundary is crossed — reopening an already-delivered decision
 * would break at-most-once. Clears the marker only if it still names `decisionId`,
 * so it can never reopen a different decision claimed in the meantime.
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
 * Retire a spent decision, removing only the record the caller actually observed.
 *
 * The lifecycle poll decides a decision is spent from an in-memory session
 * snapshot taken at the top of the cycle. If the agent writes a fresh report
 * after that load, an unconditional clear would delete the NEW record — losing it
 * and (for a decision) its callback identity. Everything below runs under the
 * metadata lock and compares the stored record before touching it.
 *
 * Two observations, two shapes:
 *  - `observed` is a decision the poll saw: clear the WHOLE record (report fields
 *    + identity markers) only while the stored state/timestamp still identify that
 *    decision. A superseded observation no-ops.
 *  - `observed` is `null` (episode marker lingered but the poll saw no decision
 *    report): the report fields now belong to a SUCCESSOR — either a non-decision
 *    report like `ao report working`, which must be preserved so the report
 *    watcher does not treat the agent as never-acknowledged, or a fresh decision
 *    with its own identity. Clear ONLY the orphan identity markers (episode +
 *    consumed), never the report fields; and if the successor is itself a decision
 *    report, leave even those markers to its own lifecycle.
 *
 * Returns the exact patch applied (so callers can mirror it into an in-memory
 * snapshot key-for-key), or `null` when nothing was cleared. Mirroring a fixed
 * key set instead would hide a preserved successor report for the rest of the
 * cycle.
 */
export function clearSpentDecision(
  projectId: string,
  sessionId: SessionId,
  observed: { state: string; at: string } | null,
): Record<string, string> | null {
  let applied: Record<string, string> | null = null;
  mutateMetadata(getProjectSessionsDir(projectId), sessionId, (existing) => {
    const storedState = existing[AGENT_REPORT_METADATA_KEYS.STATE] ?? "";
    if (observed === null) {
      // A decision report present now was written after our observation — it is a
      // successor with its own (possibly just-set) episode identity. Leave it.
      if (isDecisionReportState(storedState)) {
        applied = null;
        return existing;
      }
      // Otherwise drop only the orphaned identity markers, preserving the
      // successor non-decision report's state/timestamp.
      const hasOrphanMarker =
        !!existing[NOTIFY_DECISION_METADATA_KEYS.EPISODE_AT] ||
        !!existing[NOTIFY_DECISION_METADATA_KEYS.CONSUMED_ID];
      if (!hasOrphanMarker) {
        applied = null;
        return existing;
      }
      applied = {
        [NOTIFY_DECISION_METADATA_KEYS.EPISODE_AT]: "",
        [NOTIFY_DECISION_METADATA_KEYS.CONSUMED_ID]: "",
      };
      return { ...existing, ...applied };
    }
    // Compare NORMALIZED timestamps: `observed.at` came from `readAgentReport`
    // (already canonical), but the stored value may be a valid non-canonical
    // spelling. A raw comparison would never match those, so retirement would
    // no-op and a later bare prompt could revalidate the stale token. (#13 review)
    if (
      storedState !== observed.state ||
      normalizeReportTimestamp(existing[AGENT_REPORT_METADATA_KEYS.AT]) !== observed.at
    ) {
      applied = null;
      return existing;
    }
    applied = clearedDecisionMetadata();
    return { ...existing, ...applied };
  });
  return applied;
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
