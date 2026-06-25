/**
 * Unit tests for cost budget cap evaluation (resolveBudget + evaluateBudgetBreach).
 */

import { describe, it, expect } from "vitest";
import { resolveBudget, evaluateBudgetBreach } from "../budget.js";
import type { OrchestratorConfig, Session } from "../types.js";

function makeConfig(overrides: Partial<OrchestratorConfig> = {}): OrchestratorConfig {
  return {
    configPath: "/tmp/agent-orchestrator.yaml",
    readyThresholdMs: 300_000,
    defaults: { runtime: "tmux", agent: "claude-code", workspace: "worktree", notifiers: [] },
    projects: {},
    notifiers: {},
    notificationRouting: {} as OrchestratorConfig["notificationRouting"],
    reactions: {},
    ...overrides,
  };
}

function makeSession(projectId: string, estimatedCostUsd: number): Session {
  return {
    projectId,
    agentInfo:
      estimatedCostUsd > 0
        ? {
            summary: null,
            agentSessionId: null,
            cost: { inputTokens: 1000, outputTokens: 500, estimatedCostUsd },
          }
        : null,
  } as unknown as Session;
}

describe("resolveBudget", () => {
  it("prefers per-project caps over global defaults", () => {
    const config = makeConfig({
      budget: { perSessionUsd: 5, perProjectUsd: 50 },
      projects: {
        app: { budget: { perSessionUsd: 2 } } as OrchestratorConfig["projects"][string],
      },
    });
    expect(resolveBudget(config, "app")).toEqual({ perSessionUsd: 2, perProjectUsd: 50 });
  });

  it("falls back to global defaults when no project override exists", () => {
    const config = makeConfig({ budget: { perSessionUsd: 5 } });
    expect(resolveBudget(config, "missing")).toEqual({
      perSessionUsd: 5,
      perProjectUsd: undefined,
    });
  });
});

describe("evaluateBudgetBreach", () => {
  it("returns null when no caps are configured", () => {
    const config = makeConfig();
    expect(evaluateBudgetBreach(config, makeSession("app", 100), 100)).toBeNull();
  });

  it("returns null when cost is under the per-session cap", () => {
    const config = makeConfig({ budget: { perSessionUsd: 5 } });
    expect(evaluateBudgetBreach(config, makeSession("app", 4.99), 4.99)).toBeNull();
  });

  it("flags a per-session breach when cost exceeds the cap", () => {
    const config = makeConfig({ budget: { perSessionUsd: 5 } });
    const breach = evaluateBudgetBreach(config, makeSession("app", 6.25), 6.25);
    expect(breach).toMatchObject({ scope: "session", limitUsd: 5, actualUsd: 6.25 });
    expect(breach?.evidence).toContain("budget_exceeded session");
  });

  it("flags a per-project breach when the project total exceeds the cap", () => {
    const config = makeConfig({ budget: { perProjectUsd: 10 } });
    // Session itself is cheap, but the project total is over.
    const breach = evaluateBudgetBreach(config, makeSession("app", 1), 12);
    expect(breach).toMatchObject({ scope: "project", limitUsd: 10, actualUsd: 12 });
  });

  it("treats a zero cap as no limit", () => {
    const config = makeConfig({ budget: { perSessionUsd: 0, perProjectUsd: 0 } });
    expect(evaluateBudgetBreach(config, makeSession("app", 999), 999)).toBeNull();
  });

  it("per-session cap takes precedence over per-project when both breach", () => {
    const config = makeConfig({ budget: { perSessionUsd: 5, perProjectUsd: 10 } });
    const breach = evaluateBudgetBreach(config, makeSession("app", 8), 20);
    expect(breach?.scope).toBe("session");
  });

  it("uses the per-project override from project config", () => {
    const config = makeConfig({
      projects: {
        app: { budget: { perProjectUsd: 3 } } as OrchestratorConfig["projects"][string],
      },
    });
    const breach = evaluateBudgetBreach(config, makeSession("app", 1), 4);
    expect(breach).toMatchObject({ scope: "project", limitUsd: 3 });
  });
});
