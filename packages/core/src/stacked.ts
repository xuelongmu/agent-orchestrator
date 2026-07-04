/**
 * Stacked PRs (#11): the single source of truth for "what base should a stacked
 * child use, given its parent's CURRENT lifecycle state?"
 *
 * Every path that resolves a stacked child's base — held-child spawn/unblock in
 * the session manager, and the retarget fallback in the lifecycle manager —
 * routes through {@link resolveStackedChildBase} so the rules stay consistent:
 *
 *   - No parent record  → the parent merged and was auto-cleaned up; its work is
 *                          in the base, so branch off the caller's default.
 *   - Parent MERGED      → branch off the base the parent merged INTO (its own
 *                          `ownBase`): the default for a top-level parent, the
 *                          grandparent branch for a middle stack. NOT the
 *                          now-merged/deleted parent branch.
 *   - Parent OPEN        → stack directly on the parent's branch.
 *
 * Merge state is derived from the parent's LIFECYCLE (its PR record), never a
 * persisted `status` field — lifecycle-backed metadata does not persist status.
 */
import type { CanonicalSessionLifecycle } from "./types.js";

export interface StackedParentState {
  /** The parent's canonical lifecycle (its `pr.state` is the merge-state truth). */
  lifecycle?: CanonicalSessionLifecycle;
  /** The parent's current branch. */
  branch?: string | null;
  /** The base the parent itself targets / merged into (the parent's own baseRef). */
  ownBase?: string | null;
}

export interface StackedChildBase {
  /** Branch the child should be cut from / target. `undefined` ⇒ project default. */
  base: string | undefined;
  /** Whether the parent's PR has merged (or the parent is gone → treated merged). */
  parentMerged: boolean;
}

export function resolveStackedChildBase(parent: StackedParentState | null): StackedChildBase {
  // No parent record: it merged and was auto-cleaned up. Its work is in the base.
  if (!parent) return { base: undefined, parentMerged: true };

  const parentMerged = parent.lifecycle?.pr.state === "merged";
  if (parentMerged) {
    return { base: parent.ownBase || undefined, parentMerged: true };
  }
  return { base: parent.branch || undefined, parentMerged: false };
}
