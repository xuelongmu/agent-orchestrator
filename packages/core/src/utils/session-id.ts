import type { SessionId } from "../types.js";

export const SESSION_ID_COMPONENT_PATTERN = /^[a-zA-Z0-9_-]+$/;

/**
 * The single source of truth for a valid session-ID component. Session IDs are
 * built from `${sessionPrefix}-${n}` (and `-rev-${n}` reviewer variants), and
 * `sessionPrefix` is length-unbounded in config — so validators that reject a
 * long-but-valid ID (e.g. a generic identifier cap) would refuse an ID core can
 * legitimately create. Any boundary that guards a session ID (the notify
 * callback route, metadata paths) must use this contract, not an ad-hoc cap.
 */
export function isValidSessionIdComponent(sessionId: string): boolean {
  return SESSION_ID_COMPONENT_PATTERN.test(sessionId);
}

export function assertValidSessionIdComponent(
  sessionId: SessionId,
  context = "session ID",
): void {
  if (!isValidSessionIdComponent(sessionId)) {
    throw new Error(`Invalid ${context}: ${sessionId}`);
  }
}
