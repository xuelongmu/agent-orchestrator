import { describe, it, expect } from "vitest";
import { resolveStackedChildBase } from "../stacked.js";
import { createInitialCanonicalLifecycle } from "../lifecycle-state.js";
import type { CanonicalSessionLifecycle } from "../types.js";

function lifecycleWithPrState(state: "open" | "merged" | "closed" | "none"): CanonicalSessionLifecycle {
  const lc = createInitialCanonicalLifecycle("worker", new Date("2026-01-01T00:00:00.000Z"));
  lc.pr.state = state;
  return lc;
}

describe("resolveStackedChildBase", () => {
  it("treats a missing parent record as merged → default base (undefined)", () => {
    expect(resolveStackedChildBase(null)).toEqual({ base: undefined, parentMerged: true });
  });

  it("stacks on the parent's branch while the parent PR is open", () => {
    const result = resolveStackedChildBase({
      lifecycle: lifecycleWithPrState("open"),
      branch: "feat/parent",
      ownBase: "main",
    });
    expect(result).toEqual({ base: "feat/parent", parentMerged: false });
  });

  it("branches off the parent's OWN base once the parent PR merged (middle stack)", () => {
    const result = resolveStackedChildBase({
      lifecycle: lifecycleWithPrState("merged"),
      branch: "feat/parent",
      ownBase: "feat/grandparent",
    });
    expect(result).toEqual({ base: "feat/grandparent", parentMerged: true });
  });

  it("branches off the default (undefined) when a merged top-level parent has no own base", () => {
    const result = resolveStackedChildBase({
      lifecycle: lifecycleWithPrState("merged"),
      branch: "feat/parent",
      ownBase: "",
    });
    expect(result).toEqual({ base: undefined, parentMerged: true });
  });

  it("derives merge-state from the lifecycle, not a status field (open when pr.state !== merged)", () => {
    // A parent with no merged PR record is treated as open even though callers
    // may not persist a status field.
    const result = resolveStackedChildBase({
      lifecycle: lifecycleWithPrState("open"),
      branch: "feat/parent",
      ownBase: "main",
    });
    expect(result.parentMerged).toBe(false);
    expect(result.base).toBe("feat/parent");
  });
});
