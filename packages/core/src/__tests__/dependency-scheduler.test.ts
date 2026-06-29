import { describe, expect, it } from "vitest";
import {
  collectSatisfiedDependencies,
  computeRemainingBlockedBy,
  countActiveSessions,
  isDependencySatisfied,
  isPrerequisiteSatisfied,
  normalizeDependencyId,
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

describe("isPrerequisiteSatisfied", () => {
  it("is true only when the PR has merged", () => {
    expect(isPrerequisiteSatisfied(makeSession({ prMerged: true }))).toBe(true);
    expect(isPrerequisiteSatisfied(makeSession({ prMerged: false }))).toBe(false);
  });
});

describe("collectSatisfiedDependencies", () => {
  it("gathers session ids globally and issue ids per project, from merged sessions only", () => {
    const sessions = [
      makeSession({ id: "back-1", projectId: "backend", issueId: "9", prMerged: true }),
      makeSession({ id: "back-2", projectId: "backend", issueId: "10", prMerged: false }),
    ];
    const satisfied = collectSatisfiedDependencies(sessions);
    expect(satisfied.sessionIds.has("back-1")).toBe(true);
    expect(satisfied.sessionIds.has("back-2")).toBe(false);
    expect(satisfied.issueIdsByProject.get("backend")?.has("9")).toBe(true);
    expect(satisfied.issueIdsByProject.get("backend")?.has("10")).toBe(false);
  });
});

describe("isDependencySatisfied", () => {
  const satisfied = collectSatisfiedDependencies([
    makeSession({ id: "back-1", projectId: "backend", issueId: "20", prMerged: true }),
  ]);

  it("matches a merged session id from any project (globally unique handle)", () => {
    expect(isDependencySatisfied("back-1", "frontend", satisfied)).toBe(true);
    expect(isDependencySatisfied("#back-1", "frontend", satisfied)).toBe(true);
  });

  it("matches a merged issue id only within the same project", () => {
    expect(isDependencySatisfied("20", "backend", satisfied)).toBe(true);
    expect(isDependencySatisfied("#20", "backend", satisfied)).toBe(true);
  });

  it("does NOT cross-satisfy a same-numbered issue in another project", () => {
    // backend#20 merged must not unblock a frontend dependent on its own #20.
    expect(isDependencySatisfied("20", "frontend", satisfied)).toBe(false);
  });

  it("matches a repo-qualified issue ref from any project (globally unique)", () => {
    // A cross-repo blocker emitted by `ao plan` as "acme/api#5" must resolve once
    // the merged session that owns acme/api#5 lands, even in another project. The
    // qualified repo comes from the issue's project repo, not from PR repos.
    const crossRepo = collectSatisfiedDependencies(
      [makeSession({ id: "api-1", projectId: "api", issueId: "5", prMerged: true })],
      (projectId) => (projectId === "api" ? "acme/api" : undefined),
    );
    expect(isDependencySatisfied("acme/api#5", "web", crossRepo)).toBe(true);
    expect(isDependencySatisfied("acme/api#5", "anything", crossRepo)).toBe(true);
    // A different repo's #5 must not satisfy it.
    expect(isDependencySatisfied("acme/other#5", "web", crossRepo)).toBe(false);
  });

  it("does not register repo-qualified ids without a project-repo resolver", () => {
    // Without the resolver (the issue's repo is unknown), a cross-repo ref can't
    // be claimed satisfied — better unresolved than wrongly unblocking.
    const noRepo = collectSatisfiedDependencies([
      makeSession({ id: "api-1", projectId: "api", issueId: "5", prMerged: true }),
    ]);
    expect(isDependencySatisfied("acme/api#5", "web", noRepo)).toBe(false);
  });
});

describe("computeRemainingBlockedBy", () => {
  it("keeps a multi-prerequisite dependent blocked until all merge", () => {
    const onlyOne = collectSatisfiedDependencies([
      makeSession({ id: "a-1", projectId: "app", issueId: "9", prMerged: true }),
    ]);
    expect(computeRemainingBlockedBy(["9", "10"], "app", onlyOne)).toEqual(["10"]);

    const both = collectSatisfiedDependencies([
      makeSession({ id: "a-1", projectId: "app", issueId: "9", prMerged: true }),
      makeSession({ id: "a-2", projectId: "app", issueId: "10", prMerged: true }),
    ]);
    expect(computeRemainingBlockedBy(["9", "10"], "app", both)).toEqual([]);
  });

  it("unblocks a cross-project dependent via the prerequisite's session id", () => {
    const satisfied = collectSatisfiedDependencies([
      makeSession({ id: "back-1", projectId: "backend", issueId: "20", prMerged: true }),
    ]);
    expect(computeRemainingBlockedBy(["back-1"], "frontend", satisfied)).toEqual([]);
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

  it("counts a merged session still in its post-merge cleanup grace window", () => {
    // `merged` is terminal for status purposes, but the worker is still alive
    // until auto-cleanup completes, so it must occupy a concurrency slot.
    const sessions = [
      makeSession({ id: "app-1", projectId: "app", status: "merged" }), // grace → counts
      makeSession({ id: "app-2", projectId: "app", status: "done" }), // cleaned → excluded
    ];
    expect(countActiveSessions(sessions, "app")).toBe(1);
  });
});
