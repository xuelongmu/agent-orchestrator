/**
 * Process-local guard against double-resolving a single notification decision —
 * a double-tap, or Approve then Deny/Kill before the agent leaves the pending
 * state (#13, review). Decisions are keyed by identity (`project:session:nonce`)
 * and each entry self-expires at its token's expiry.
 *
 * AO's web dashboard is a single long-running process, so a process-local guard
 * suffices; a restart clears it, by which point the agent has moved on and the
 * report nonce would no longer match anyway.
 */

const resolvedDecisions = new Map<string, number>();

export function decisionKeyFor(payload: {
  projectId: string;
  sessionId: string;
  nonce?: string;
}): string {
  return `${payload.projectId}:${payload.sessionId}:${payload.nonce ?? ""}`;
}

/**
 * Claim a decision before dispatching its action. Returns `false` when the
 * decision was already claimed (a duplicate tap). Expired claims are purged.
 */
export function claimDecision(key: string, expiresAt: number, now: number): boolean {
  for (const [k, exp] of resolvedDecisions) {
    if (exp <= now) resolvedDecisions.delete(k);
  }
  if (resolvedDecisions.has(key)) return false;
  resolvedDecisions.set(key, expiresAt);
  return true;
}

/** Release a claim so a genuinely failed dispatch can be retried. */
export function releaseDecision(key: string): void {
  resolvedDecisions.delete(key);
}

/** Test-only: clear all claims. */
export function __resetResolvedDecisions(): void {
  resolvedDecisions.clear();
}
