import { describe, expect, it } from "vitest";
import {
  collectSatisfiedDependencyIds,
  computeRemainingBlockedBy,
  countActiveSessions,
  isPrerequisiteSatisfied,
  normalizeDependencyId,
  sessionSatisfiedIds,
} from "../dependency-scheduler.js";
import { createInitialCanonicalLifecycle } from "../lifecycle-state.js";
import { createActivitySignal } from "../activity-signal.js";
import type { Session, SessionId } from "../types.js";

const T0 = new Date("2025-01-01T00:00:00Z");

interface SessionOverrides {
  id?: string;
  projectId?: string;
  issueId?: string | null;
  prMerged?: boolean;
  blocked?: boolean;
  blockedBy?: string[];
  status?: Session["status"];
  role?: string;
}

function makeSession(overrides: SessionOverrides = {}): Session {
  const lifecycle = createInitialCanonicalLifecycle("worker", T0);
  if (overrides.blocked) {
    lifecycle.session.reason = "blocked_by_dependency";
  }
  if (overrides.prMerged) {
    lifecycle.pr.state = "merged";
    lifecycle.pr.reason = "merged";
  }
  return {
    id: (overrides.id ?? "app-1") as SessionId,
    projectId: overrides.projectId ?? "app",
    status: overrides.status ?? (overrides.blocked ? "spawning" : "working"),
    activity: null,
    activitySignal: createActivitySignal("unavailable"),
    lifecycle,
    branch: "feat/1",
    issueId: overrides.issueId === undefined ? null : overrides.issueId,
    pr: null,
    prs: [],
    workspacePath: null,
    runtimeHandle: null,
    agentInfo: null,
    createdAt: T0,
    lastActivityAt: T0,
    ...(overrides.blockedBy ? { blockedBy: overrides.blockedBy } : {}),
    metadata: overrides.role ? { role: overrides.role } : {},
  };
}

describe("normalizeDependencyId", () => {
  it("strips a leading #, trims, and lowercases", () => {
    expect(normalizeDependencyId("#9")).toBe("9");
    expect(normalizeDependencyId("  Front-2 ")).toBe("front-2");
    expect(normalizeDependencyId("9")).toBe("9");
  });
});

describe("sessionSatisfiedIds", () => {
  it("exposes the normalized session id and issue id", () => {
    const session = makeSession({ id: "back-1", issueId: "#9" });
    expect(sessionSatisfiedIds(session).sort()).toEqual(["9", "back-1"]);
  });

  it("omits a missing issue id", () => {
    const session = makeSession({ id: "back-1", issueId: null });
    expect(sessionSatisfiedIds(session)).toEqual(["back-1"]);
  });
});

describe("isPrerequisiteSatisfied", () => {
  it("is true only when the PR has merged", () => {
    expect(isPrerequisiteSatisfied(makeSession({ prMerged: true }))).toBe(true);
    expect(isPrerequisiteSatisfied(makeSession({ prMerged: false }))).toBe(false);
  });
});

describe("collectSatisfiedDependencyIds", () => {
  it("gathers ids only from merged sessions, across projects", () => {
    const sessions = [
      makeSession({ id: "back-1", projectId: "backend", issueId: "9", prMerged: true }),
      makeSession({ id: "back-2", projectId: "backend", issueId: "10", prMerged: false }),
    ];
    const satisfied = collectSatisfiedDependencyIds(sessions);
    expect(satisfied.has("back-1")).toBe(true);
    expect(satisfied.has("9")).toBe(true);
    expect(satisfied.has("10")).toBe(false);
    expect(satisfied.has("back-2")).toBe(false);
  });
});

describe("computeRemainingBlockedBy", () => {
  it("keeps a multi-prerequisite dependent blocked until all merge", () => {
    const onlyOneMerged = new Set(["9"]);
    expect(computeRemainingBlockedBy(["9", "10"], onlyOneMerged)).toEqual(["10"]);

    const bothMerged = new Set(["9", "10"]);
    expect(computeRemainingBlockedBy(["9", "10"], bothMerged)).toEqual([]);
  });

  it("matches regardless of # prefix or case", () => {
    expect(computeRemainingBlockedBy(["#9", "Back-1"], new Set(["9", "back-1"]))).toEqual([]);
  });
});

describe("countActiveSessions", () => {
  it("counts launched non-terminal workers, excluding held, terminal, and orchestrators", () => {
    const sessions = [
      makeSession({ id: "app-1", projectId: "app" }), // working → counts
      makeSession({ id: "app-2", projectId: "app", status: "pr_open" }), // counts
      makeSession({ id: "app-3", projectId: "app", blocked: true }), // held → excluded
      makeSession({ id: "app-4", projectId: "app", status: "done" }), // terminal → excluded
      makeSession({ id: "app-orchestrator", projectId: "app", role: "orchestrator" }), // excluded
      makeSession({ id: "other-1", projectId: "other" }), // different project → excluded
    ];
    expect(countActiveSessions(sessions, "app")).toBe(2);
  });
});
