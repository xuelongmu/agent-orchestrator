import { describe, expect, it, vi } from "vitest";
import {
  buildClaudeDecomposerArgs,
  buildCodexDecomposerArgs,
  buildDecomposerPrompt,
  createPlanTickets,
  decomposeGoal,
  parsePlan,
  PlanValidationError,
  resolveDecomposerAgent,
  resolvePlanRunner,
  topoSortPlan,
  validatePlanGraph,
  type Plan,
} from "../planner.js";
import type { CreateIssueInput, Issue, ProjectConfig, Tracker } from "../types.js";

const baseProject: ProjectConfig = {
  name: "Demo",
  repo: "acme/app",
  path: "/tmp/demo",
  defaultBranch: "main",
  sessionPrefix: "demo",
  tracker: { plugin: "github" },
};

function plan(tickets: Plan["tickets"]): Plan {
  return { tickets };
}

/** Mock tracker that records createIssue calls and returns sequential issues. */
function makeMockTracker(): { tracker: Tracker; calls: Array<{ input: CreateIssueInput; repo?: string }> } {
  const calls: Array<{ input: CreateIssueInput; repo?: string }> = [];
  let counter = 0;
  const tracker: Tracker = {
    name: "mock",
    getIssue: async () => {
      throw new Error("not implemented");
    },
    isCompleted: async () => false,
    issueUrl: (id) => `https://example.com/issues/${id}`,
    branchName: (id) => `feat/${id}`,
    generatePrompt: async () => "",
    createIssue: async (input: CreateIssueInput, project: ProjectConfig): Promise<Issue> => {
      counter += 1;
      const id = String(counter);
      calls.push({ input, repo: project.repo });
      return {
        id,
        title: input.title,
        description: input.description,
        url: `https://example.com/issues/${id}`,
        state: "open",
        labels: input.labels ?? [],
      };
    },
  };
  return { tracker, calls };
}

describe("parsePlan", () => {
  it("parses a bare JSON object", () => {
    const result = parsePlan('{"tickets":[{"ref":"t1","title":"A"}]}');
    expect(result.tickets).toHaveLength(1);
    expect(result.tickets[0].body).toBe(""); // default applied
  });

  it("parses JSON wrapped in a markdown fence and surrounding prose", () => {
    const raw = [
      "Here is the plan:",
      "```json",
      '{"tickets":[{"ref":"t1","title":"A","body":"do it"}]}',
      "```",
      "Hope that helps!",
    ].join("\n");
    const result = parsePlan(raw);
    expect(result.tickets[0].title).toBe("A");
  });

  it("throws on non-JSON output", () => {
    expect(() => parsePlan("I could not produce a plan.")).toThrow(PlanValidationError);
  });

  it("throws when tickets is empty", () => {
    expect(() => parsePlan('{"tickets":[]}')).toThrow(PlanValidationError);
  });
});

describe("validatePlanGraph", () => {
  it("rejects duplicate refs", () => {
    expect(() =>
      validatePlanGraph(plan([
        { ref: "t1", title: "A", body: "" },
        { ref: "t1", title: "B", body: "" },
      ])),
    ).toThrow(/Duplicate ticket ref/);
  });

  it("rejects dangling references", () => {
    expect(() =>
      validatePlanGraph(plan([{ ref: "t1", title: "A", body: "", blockedByRefs: ["nope"] }])),
    ).toThrow(/unknown blocker/);
  });

  it("rejects self references", () => {
    expect(() =>
      validatePlanGraph(plan([{ ref: "t1", title: "A", body: "", parentRef: "t1" }])),
    ).toThrow(/its own parent/);
  });

  it("detects dependency cycles", () => {
    expect(() =>
      validatePlanGraph(plan([
        { ref: "t1", title: "A", body: "", blockedByRefs: ["t2"] },
        { ref: "t2", title: "B", body: "", blockedByRefs: ["t1"] },
      ])),
    ).toThrow(/cycle/);
  });

  it("accepts a valid DAG", () => {
    expect(() =>
      validatePlanGraph(plan([
        { ref: "t1", title: "A", body: "" },
        { ref: "t2", title: "B", body: "", blockedByRefs: ["t1"], parentRef: "t1" },
      ])),
    ).not.toThrow();
  });
});

describe("topoSortPlan", () => {
  it("orders dependencies before dependents", () => {
    const p = plan([
      { ref: "t3", title: "C", body: "", blockedByRefs: ["t2"] },
      { ref: "t2", title: "B", body: "", blockedByRefs: ["t1"] },
      { ref: "t1", title: "A", body: "" },
    ]);
    const order = topoSortPlan(p).map((t) => t.ref);
    expect(order.indexOf("t1")).toBeLessThan(order.indexOf("t2"));
    expect(order.indexOf("t2")).toBeLessThan(order.indexOf("t3"));
  });
});

describe("createPlanTickets", () => {
  it("creates tickets in topo order and resolves relations to real issue ids", async () => {
    const { tracker, calls } = makeMockTracker();
    const p = plan([
      { ref: "child", title: "Child", body: "", blockedByRefs: ["parent"], parentRef: "parent" },
      { ref: "parent", title: "Parent", body: "" },
    ]);

    const created = await createPlanTickets({ plan: p, tracker, project: baseProject });

    // parent created first (issue 1), child second (issue 2)
    expect(created.map((c) => c.ref)).toEqual(["parent", "child"]);
    expect(created[0].issue.id).toBe("1");
    expect(created[1].issue.id).toBe("2");

    // child's relations resolved to the parent's real issue number
    const childCall = calls[1];
    expect(childCall.input.parentId).toBe("1");
    expect(childCall.input.blockedBy).toEqual(["1"]);
  });

  it("routes per-ticket repo overrides to the tracker", async () => {
    const { tracker, calls } = makeMockTracker();
    const p = plan([
      { ref: "t1", title: "API", body: "", repo: "acme/api" },
      { ref: "t2", title: "Web", body: "" },
    ]);

    await createPlanTickets({ plan: p, tracker, project: baseProject });

    expect(calls[0].repo).toBe("acme/api"); // override
    expect(calls[1].repo).toBe("acme/app"); // project default
  });

  it("carries a forward related ref onto the later-created ticket", async () => {
    const { tracker, calls } = makeMockTracker();
    // t1 relates to t2 with no dependency edge, so t1 is created first (issue 1)
    // before t2 exists. The link must still be recorded — it lands on t2.
    const p = plan([
      { ref: "t1", title: "A", body: "", relatedRefs: ["t2"] },
      { ref: "t2", title: "B", body: "" },
    ]);

    await createPlanTickets({ plan: p, tracker, project: baseProject });

    expect(calls[0].input.relatedTo).toBeUndefined(); // t1 created first
    expect(calls[1].input.relatedTo).toEqual(["1"]); // t2 links back to t1
  });

  it("rejects cross-repo dependency edges before creating anything", async () => {
    const { tracker, calls } = makeMockTracker();
    const p = plan([
      { ref: "api", title: "API", body: "", repo: "acme/api" },
      { ref: "web", title: "Web", body: "", blockedByRefs: ["api"] }, // web in acme/app
    ]);

    await expect(
      createPlanTickets({ plan: p, tracker, project: baseProject }),
    ).rejects.toThrow(/Cross-repo dependency/);
    expect(calls).toHaveLength(0); // nothing created
  });

  it("throws when the tracker cannot create issues", async () => {
    const { tracker } = makeMockTracker();
    const noCreate: Tracker = { ...tracker, createIssue: undefined };
    await expect(
      createPlanTickets({ plan: plan([{ ref: "t1", title: "A", body: "" }]), tracker: noCreate, project: baseProject }),
    ).rejects.toThrow(/does not support creating issues/);
  });
});

describe("decomposeGoal", () => {
  it("runs the injected planner and validates the result", async () => {
    const runPlanner = vi.fn(async () => ({
      rawOutput: '{"tickets":[{"ref":"t1","title":"A"}]}',
    }));
    const result = await decomposeGoal({
      goal: "do a thing",
      project: baseProject,
      projectId: "demo",
      runPlanner,
    });
    expect(runPlanner).toHaveBeenCalledOnce();
    expect(result.tickets[0].ref).toBe("t1");
  });

  it("throws when the planner returns no output", async () => {
    await expect(
      decomposeGoal({
        goal: "x",
        project: baseProject,
        projectId: "demo",
        runPlanner: async () => ({ rawOutput: "" }),
      }),
    ).rejects.toThrow(PlanValidationError);
  });
});

describe("decomposer agent resolution", () => {
  it("prefers decomposer, then orchestrator, then worker, then project/default agent", () => {
    expect(resolveDecomposerAgent({ decomposer: { agent: "codex" } })).toBe("codex");
    expect(resolveDecomposerAgent({}, { decomposer: { agent: "codex" } })).toBe("codex");
    expect(resolveDecomposerAgent({ orchestrator: { agent: "claude" } })).toBe("claude");
    expect(resolveDecomposerAgent({ worker: { agent: "grok" } })).toBe("grok");
    expect(resolveDecomposerAgent({}, { worker: { agent: "grok" } })).toBe("grok");
    expect(resolveDecomposerAgent({ agent: "aider" })).toBe("aider");
    expect(resolveDecomposerAgent({}, { agent: "opencode" })).toBe("opencode");
    expect(resolveDecomposerAgent({})).toBe("claude-code");
  });

  it("decomposer default outranks orchestrator/worker roles", () => {
    expect(
      resolveDecomposerAgent(
        { orchestrator: { agent: "claude" } },
        { decomposer: { agent: "codex" } },
      ),
    ).toBe("claude"); // project-level decomposer/orchestrator beats defaults
    expect(
      resolveDecomposerAgent({}, { decomposer: { agent: "codex" }, orchestrator: { agent: "claude" } }),
    ).toBe("codex");
  });

  it("resolves runners for supported agents and rejects others", () => {
    expect(resolvePlanRunner("codex")).toBeTypeOf("function");
    expect(resolvePlanRunner("claude-code")).toBeTypeOf("function");
    expect(() => resolvePlanRunner("aider")).toThrow(/not supported/);
  });
});

describe("decomposer command construction", () => {
  it("builds codex headless args", () => {
    const args = buildCodexDecomposerArgs("/out.json", "the prompt");
    expect(args).toEqual(["exec", "--sandbox", "read-only", "--output-last-message", "/out.json", "the prompt"]);
  });

  it("builds claude print args in read-only plan mode", () => {
    expect(buildClaudeDecomposerArgs("the prompt")).toEqual([
      "-p",
      "the prompt",
      "--permission-mode",
      "plan",
      "--output-format",
      "text",
    ]);
  });

  it("includes the goal and schema hint in the prompt", () => {
    const prompt = buildDecomposerPrompt({
      goal: "Add OAuth",
      project: baseProject,
      projectId: "demo",
    });
    expect(prompt).toContain("Add OAuth");
    expect(prompt).toContain('"tickets"');
    expect(prompt).toContain("acme/app");
  });
});
