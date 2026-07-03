import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
import { mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { createLifecycleManager } from "../lifecycle-manager.js";
import { recordActivityEvent } from "../activity-events.js";
import { DEFAULT_BUGBOT_COMMENTS_MESSAGE } from "../config.js";
import {
  resolvePREnrichmentDecision,
  resolvePRLiveDecision,
  resolveProbeDecision,
} from "../lifecycle-status-decisions.js";
import { createSessionManager } from "../session-manager.js";
import { updateMetadata, writeMetadata, readMetadataRaw } from "../metadata.js";
import { readObservabilitySummary } from "../observability.js";
import type {
  OrchestratorConfig,
  PluginRegistry,
  OpenCodeSessionManager,
  Agent,
  ActivityState,
  SessionStatus,
  SessionMetadata,
  PRInfo,
  SCM,
} from "../types.js";
import {
  createTestEnvironment,
  createMockPlugins,
  createMockRegistry,
  createMockSessionManager,
  createMockSCM,
  createMockNotifier,
  makeSession,
  makePR,
  type TestEnvironment,
  type MockPlugins,
} from "./test-utils.js";

vi.mock("../activity-events.js", () => ({
  recordActivityEvent: vi.fn(),
}));

let env: TestEnvironment;
let plugins: MockPlugins;
let mockRegistry: PluginRegistry;
let mockSessionManager: OpenCodeSessionManager;
let config: OrchestratorConfig;

beforeEach(() => {
  env = createTestEnvironment();
  plugins = createMockPlugins();
  mockRegistry = createMockRegistry({ runtime: plugins.runtime, agent: plugins.agent });
  mockSessionManager = createMockSessionManager();
  config = env.config;
  vi.mocked(recordActivityEvent).mockClear();
});

afterEach(() => {
  env.cleanup();
});

describe("status decision helpers", () => {
  it("promotes conflicting runtime evidence into detecting instead of terminating", () => {
    const decision = resolveProbeDecision({
      currentAttempts: 1,
      runtimeProbe: { state: "dead", failed: false },
      processProbe: { state: "alive", failed: false },
      canProbeRuntimeIdentity: true,
      activitySignal: {
        state: "valid",
        activity: "active",
        timestamp: new Date(),
        source: "native",
      },
      activityEvidence: "activity_signal=valid via_native activity=active",
      idleWasBlocked: false,
    });

    expect(decision).toEqual(
      expect.objectContaining({
        status: "detecting",
        sessionState: "detecting",
        sessionReason: "runtime_lost",
        detecting: expect.objectContaining({ attempts: 2 }),
      }),
    );
  });

  it("maps merged enrichment data to merged lifecycle state", () => {
    const decision = resolvePREnrichmentDecision(
      {
        state: "merged",
        ciStatus: "none",
        reviewDecision: "none",
        mergeable: false,
      },
      {
        shouldEscalateIdleToStuck: false,
        idleWasBlocked: false,
        activityEvidence: "activity_signal=valid",
      },
    );

    expect(decision).toEqual(
      expect.objectContaining({
        status: "merged",
        prState: "merged",
        prReason: "merged",
        sessionState: "idle",
        sessionReason: "merged_waiting_decision",
      }),
    );
  });

  it("maps live PR checks to review_pending without mutating other state", () => {
    const decision = resolvePRLiveDecision({
      prState: "open",
      ciStatus: "passing",
      reviewDecision: "pending",
      mergeable: false,
      shouldEscalateIdleToStuck: false,
      idleWasBlocked: false,
      activityEvidence: "activity_signal=valid",
    });

    expect(decision).toEqual(
      expect.objectContaining({
        status: "review_pending",
        prState: "open",
        prReason: "review_pending",
        sessionState: "idle",
        sessionReason: "awaiting_external_review",
      }),
    );
  });
});

/** Helper: write standard session metadata and return a lifecycle manager */
function setupCheck(
  sessionId: string,
  opts: {
    session: ReturnType<typeof makeSession>;
    metaOverrides?: Record<string, unknown>;
    registry?: PluginRegistry;
    configOverride?: OrchestratorConfig;
  },
) {
  const persistedMetadata = {
    worktree: "/tmp",
    branch: opts.session.branch ?? "main",
    status: opts.session.status,
    project: "my-app",
    agent: opts.session.metadata["agent"] ?? "mock-agent",
    runtimeHandle: opts.session.runtimeHandle ?? undefined,
    ...opts.metaOverrides,
  };
  const persistedStringMetadata = Object.fromEntries(
    Object.entries(persistedMetadata).filter(
      (entry): entry is [string, string] => typeof entry[1] === "string",
    ),
  );

  vi.mocked(mockSessionManager.get).mockResolvedValue({
    ...opts.session,
    metadata: {
      ...opts.session.metadata,
      ...persistedStringMetadata,
    },
  });

  writeMetadata(env.sessionsDir, sessionId, persistedMetadata as unknown as SessionMetadata);

  return createLifecycleManager({
    config: opts.configOverride ?? config,
    registry: opts.registry ?? mockRegistry,
    sessionManager: mockSessionManager,
  });
}

/** Create a PR whose owner/repo matches the test config's "org/my-app". */
function makeMatchingPR(overrides: Partial<PRInfo> = {}): PRInfo {
  return makePR({ owner: "org", repo: "my-app", ...overrides });
}

/** Build a batch enrichment mock that returns the given data for any PR. */
function mockBatchEnrichment(data: {
  state?: string;
  ciStatus?: string;
  reviewDecision?: string;
  mergeable?: boolean;
  hasConflicts?: boolean;
  ciChecks?: Array<{ name: string; status: string; conclusion?: string; url?: string }>;
}) {
  return vi.fn().mockImplementation(async (prs: PRInfo[]) => {
    const result = new Map();
    for (const p of prs) {
      result.set(`${p.owner}/${p.repo}#${p.number}`, {
        state: data.state ?? "open",
        ciStatus: data.ciStatus ?? "passing",
        reviewDecision: data.reviewDecision ?? "none",
        mergeable: data.mergeable ?? false,
        ...(data.hasConflicts !== undefined ? { hasConflicts: data.hasConflicts } : {}),
        ...(data.ciChecks !== undefined ? { ciChecks: data.ciChecks } : {}),
      });
    }
    return result;
  });
}

/**
 * Helper: set up a session with PR and run a pollAll cycle so the batch
 * enrichment cache is populated. Returns the lifecycle manager.
 *
 * Must be called inside a test that uses vi.useFakeTimers().
 */
function setupPollCheck(
  sessionId: string,
  opts: {
    session: ReturnType<typeof makeSession>;
    metaOverrides?: Record<string, unknown>;
    registry?: PluginRegistry;
    configOverride?: OrchestratorConfig;
  },
) {
  const persistedMetadata: Record<string, unknown> = {
    worktree: "/tmp",
    branch: opts.session.branch ?? "main",
    status: opts.session.status,
    project: "my-app",
    agent: opts.session.metadata["agent"] ?? "mock-agent",
    runtimeHandle: opts.session.runtimeHandle ?? undefined,
    ...opts.metaOverrides,
  };
  const persistedStringMetadata = Object.fromEntries(
    Object.entries(persistedMetadata).filter(
      (entry): entry is [string, string] => typeof entry[1] === "string",
    ),
  );

  const enrichedSession = {
    ...opts.session,
    metadata: {
      ...opts.session.metadata,
      ...persistedStringMetadata,
    },
  };

  vi.mocked(mockSessionManager.list).mockResolvedValue([enrichedSession]);
  vi.mocked(mockSessionManager.get).mockResolvedValue(enrichedSession);

  writeMetadata(env.sessionsDir, sessionId, persistedMetadata as unknown as SessionMetadata);

  return createLifecycleManager({
    config: opts.configOverride ?? config,
    registry: opts.registry ?? mockRegistry,
    sessionManager: mockSessionManager,
  });
}

describe("start / stop", () => {
  it("starts and stops the polling loop", () => {
    const lm = createLifecycleManager({
      config,
      registry: mockRegistry,
      sessionManager: mockSessionManager,
    });

    lm.start(60_000);
    // Should not throw on double start
    lm.start(60_000);
    lm.stop();
    // Should not throw on double stop
    lm.stop();
  });
});

describe("budget enforcement", () => {
  const withCost = (estimatedCostUsd: number) =>
    makeSession({
      status: "working",
      agentInfo: {
        summary: null,
        agentSessionId: null,
        cost: { inputTokens: 100, outputTokens: 50, estimatedCostUsd },
      },
    });

  it("pauses an over-budget working session into needs_input", async () => {
    vi.mocked(plugins.agent.getActivityState).mockResolvedValue({ state: "active" });
    const lm = setupCheck("app-1", {
      session: withCost(9.99),
      configOverride: { ...config, budget: { perSessionUsd: 5 } },
    });

    await lm.check("app-1");

    expect(lm.getStates().get("app-1")).toBe("needs_input");
    const meta = readMetadataRaw(env.sessionsDir, "app-1");
    expect(meta!["budgetPausedAt"]).toBeTruthy();
    expect(meta!["budgetPausedReason"]).toContain("budget_exceeded");
  });

  it("leaves an under-budget working session working", async () => {
    vi.mocked(plugins.agent.getActivityState).mockResolvedValue({ state: "active" });
    const lm = setupCheck("app-1", {
      session: withCost(1.0),
      configOverride: { ...config, budget: { perSessionUsd: 5 } },
    });

    await lm.check("app-1");

    expect(lm.getStates().get("app-1")).toBe("working");
    const meta = readMetadataRaw(env.sessionsDir, "app-1");
    expect(meta!["budgetPausedAt"]).toBeFalsy();
  });

  it("does not enforce when no budget is configured", async () => {
    vi.mocked(plugins.agent.getActivityState).mockResolvedValue({ state: "active" });
    const lm = setupCheck("app-1", { session: withCost(1000) });

    await lm.check("app-1");

    expect(lm.getStates().get("app-1")).toBe("working");
  });

  it("pauses on the per-project cap using the aggregate of all project sessions", async () => {
    vi.mocked(plugins.agent.getActivityState).mockResolvedValue({ state: "active" });
    // The session's own cost ($6) is under any per-session cap, but the project
    // total across two sessions ($12) exceeds perProjectUsd ($10).
    const target = withCost(6);
    const sibling = withCost(6);
    sibling.id = "app-2";
    const lm = setupCheck("app-1", {
      session: target,
      configOverride: { ...config, budget: { perProjectUsd: 10 } },
    });
    vi.mocked(mockSessionManager.list).mockResolvedValue([target, sibling]);

    await lm.check("app-1");

    expect(lm.getStates().get("app-1")).toBe("needs_input");
    const meta = readMetadataRaw(env.sessionsDir, "app-1");
    expect(meta!["budgetPausedReason"]).toContain("project");
  });

  it("does not reset the transition timestamp on a poll while already paused", async () => {
    vi.mocked(plugins.agent.getActivityState).mockResolvedValue({ state: "active" });
    const pausedAt = "2026-01-01T00:00:00.000Z";
    const session = withCost(9.99);
    session.metadata = { ...session.metadata, budgetPausedAt: pausedAt };
    const lm = setupCheck("app-1", {
      session,
      metaOverrides: { budgetPausedAt: pausedAt },
      configOverride: { ...config, budget: { perSessionUsd: 5 } },
    });

    await lm.check("app-1");

    expect(lm.getStates().get("app-1")).toBe("needs_input");
    const meta = readMetadataRaw(env.sessionsDir, "app-1");
    // While already paused (budgetPausedAt present), the transition time reuses
    // the original pause timestamp rather than being reset to "now".
    const lifecycle = JSON.parse(meta!["lifecycle"] as string);
    expect(lifecycle.session.lastTransitionAt).toBe(pausedAt);
  });

  it("interrupts the runtime when first pausing an over-budget session", async () => {
    vi.mocked(plugins.agent.getActivityState).mockResolvedValue({ state: "active" });
    const lm = setupCheck("app-1", {
      session: withCost(9.99),
      configOverride: { ...config, budget: { perSessionUsd: 5 } },
    });

    await lm.check("app-1");

    // The agent is actually stopped (not just relabeled) so it can't keep
    // spending tokens while reported paused.
    expect(plugins.runtime.interrupt).toHaveBeenCalledWith(
      expect.objectContaining({ id: "rt-1" }),
    );
    expect(lm.getStates().get("app-1")).toBe("needs_input");
  });

  it("does not re-interrupt a quiet paused session once the interrupt has landed", async () => {
    // The interrupt landed and the agent now sits at its prompt (waiting_input),
    // so it is no longer generating. The latch must suppress redundant interrupts.
    vi.mocked(plugins.agent.getActivityState).mockResolvedValue({ state: "waiting_input" });
    const session = withCost(9.99);
    session.metadata = {
      ...session.metadata,
      budgetPausedAt: "2026-01-01T00:00:00.000Z",
      budgetInterrupted: "true",
    };
    const lm = setupCheck("app-1", {
      session,
      metaOverrides: { budgetPausedAt: "2026-01-01T00:00:00.000Z", budgetInterrupted: "true" },
      configOverride: { ...config, budget: { perSessionUsd: 5 } },
    });

    await lm.check("app-1");

    expect(plugins.runtime.interrupt).not.toHaveBeenCalled();
    expect(lm.getStates().get("app-1")).toBe("needs_input");
  });

  it("re-interrupts an over-budget session that is generating again despite the latch", async () => {
    // The interrupt previously landed (budgetInterrupted latched), but the agent
    // is actively generating again while still over the cap (Escape didn't
    // cancel, or a human resumed the terminal). The latch must NOT suppress the
    // interrupt here, or the agent keeps accruing cost and the cap is defeated.
    vi.mocked(plugins.agent.getActivityState).mockResolvedValue({ state: "active" });
    const session = withCost(9.99);
    session.metadata = {
      ...session.metadata,
      budgetPausedAt: "2026-01-01T00:00:00.000Z",
      budgetInterrupted: "true",
    };
    const lm = setupCheck("app-1", {
      session,
      metaOverrides: { budgetPausedAt: "2026-01-01T00:00:00.000Z", budgetInterrupted: "true" },
      configOverride: { ...config, budget: { perSessionUsd: 5 } },
    });

    await lm.check("app-1");

    expect(plugins.runtime.interrupt).toHaveBeenCalledWith(
      expect.objectContaining({ id: "rt-1" }),
    );
    expect(lm.getStates().get("app-1")).toBe("needs_input");
  });

  it("uses the session handle's runtime (not the project default) to interrupt", async () => {
    vi.mocked(plugins.agent.getActivityState).mockResolvedValue({ state: "active" });
    // The session was launched on the "mock" runtime (its persisted handle), but
    // the project config has since been switched to a different runtime. The
    // interrupt must target the handle's runtime, not the current config value.
    const lm = setupCheck("app-1", {
      session: withCost(9.99),
      configOverride: {
        ...config,
        defaults: { ...config.defaults, runtime: "some-other-runtime" },
        budget: { perSessionUsd: 5 },
      },
    });

    await lm.check("app-1");

    expect(mockRegistry.get).toHaveBeenCalledWith("runtime", "mock");
    expect(plugins.runtime.interrupt).toHaveBeenCalledWith(
      expect.objectContaining({ id: "rt-1" }),
    );
  });

  it("retries the interrupt on the next poll after a transient failure", async () => {
    vi.mocked(plugins.agent.getActivityState).mockResolvedValue({ state: "active" });
    // Poll 1: the runtime interrupt fails transiently (tmux error / pty-host pipe
    // timeout). The session is still paused, but the interrupt latch must NOT be
    // set so a later poll retries instead of giving up.
    vi.mocked(plugins.runtime.interrupt!).mockRejectedValueOnce(new Error("pipe timeout"));
    const lm = setupCheck("app-1", {
      session: withCost(9.99),
      configOverride: { ...config, budget: { perSessionUsd: 5 } },
    });

    await lm.check("app-1");

    expect(lm.getStates().get("app-1")).toBe("needs_input");
    const afterFail = readMetadataRaw(env.sessionsDir, "app-1");
    expect(afterFail!["budgetPausedAt"]).toBeTruthy();
    expect(afterFail!["budgetInterrupted"]).toBeFalsy();
    expect(plugins.runtime.interrupt).toHaveBeenCalledTimes(1);

    // Poll 2: still over budget, latch persisted but interrupt not yet landed —
    // the interrupt is retried and this time succeeds.
    const repaused = withCost(9.99);
    repaused.metadata = {
      ...repaused.metadata,
      budgetPausedAt: afterFail!["budgetPausedAt"] as string,
      budgetPausedReason: afterFail!["budgetPausedReason"] as string,
    };
    vi.mocked(mockSessionManager.get).mockResolvedValue(repaused);

    await lm.check("app-1");

    expect(plugins.runtime.interrupt).toHaveBeenCalledTimes(2);
    const afterRetry = readMetadataRaw(env.sessionsDir, "app-1");
    expect(afterRetry!["budgetInterrupted"]).toBe("true");
  });

  it("keeps an interrupted, at-prompt session paused while still over budget", async () => {
    // Poll 1: an active, over-budget session is paused — this writes the latch
    // to disk and interrupts the agent.
    vi.mocked(plugins.agent.getActivityState).mockResolvedValue({ state: "active" });
    const lm = setupCheck("app-1", {
      session: withCost(9.99),
      configOverride: { ...config, budget: { perSessionUsd: 5 } },
    });
    await lm.check("app-1");
    const paused = readMetadataRaw(env.sessionsDir, "app-1");
    expect(paused!["budgetPausedAt"]).toBeTruthy();
    expect(paused!["budgetInterrupted"]).toBe("true");
    expect(plugins.runtime.interrupt).toHaveBeenCalledTimes(1);

    // Poll 2: the interrupt landed and the agent now sits at its prompt
    // (waiting_input). The reload sees the persisted latch; the pause must stay
    // sticky — the latch must NOT be cleared and the agent must not be
    // re-interrupted while the cost is still over the cap.
    vi.mocked(plugins.agent.getActivityState).mockResolvedValue({ state: "waiting_input" });
    const repaused = withCost(9.99);
    repaused.status = "needs_input";
    repaused.lifecycle.session.state = "needs_input";
    repaused.lifecycle.session.reason = "awaiting_user_input";
    repaused.metadata = {
      agent: "mock-agent",
      project: "my-app",
      branch: "feat/test",
      status: "needs_input",
      budgetPausedAt: paused!["budgetPausedAt"] as string,
      budgetPausedReason: paused!["budgetPausedReason"] as string,
      budgetInterrupted: paused!["budgetInterrupted"] as string,
    };
    vi.mocked(mockSessionManager.get).mockResolvedValue(repaused);

    await lm.check("app-1");

    expect(lm.getStates().get("app-1")).toBe("needs_input");
    const stillPaused = readMetadataRaw(env.sessionsDir, "app-1");
    expect(stillPaused!["budgetPausedAt"]).toBeTruthy();
    expect(plugins.runtime.interrupt).toHaveBeenCalledTimes(1);
  });

  it("enforces the cap on an over-budget session whose status is a PR overlay (ci_failed)", async () => {
    vi.mocked(plugins.agent.getActivityState).mockResolvedValue({ state: "active" });
    const session = withCost(9.99);
    // The agent is actively generating (canonical session state "working"), but
    // it has an open, CI-failing PR, so deriveLegacyStatus() reports "ci_failed"
    // rather than "working". The cap must still be enforced — otherwise an
    // over-budget agent can keep spending while fixing CI / addressing review.
    session.lifecycle.pr.state = "open";
    session.lifecycle.pr.reason = "ci_failing";
    session.lifecycle.session.state = "working";
    session.status = "ci_failed";
    const lm = setupCheck("app-1", {
      session,
      configOverride: { ...config, budget: { perSessionUsd: 5 } },
    });

    await lm.check("app-1");

    expect(lm.getStates().get("app-1")).toBe("needs_input");
    expect(plugins.runtime.interrupt).toHaveBeenCalledWith(
      expect.objectContaining({ id: "rt-1" }),
    );
    const meta = readMetadataRaw(env.sessionsDir, "app-1");
    expect(meta!["budgetPausedAt"]).toBeTruthy();
  });

  it("does not enforce the cap on a terminal (merged) session", async () => {
    vi.mocked(plugins.agent.getActivityState).mockResolvedValue({ state: "idle" });
    // A merged session is wrapping up (canonical idle, legacy "merged"). Even
    // wildly over budget, the budget path must leave terminal/cleanup lifecycle
    // untouched.
    const session = withCost(1000);
    session.lifecycle.pr.state = "merged";
    session.lifecycle.pr.reason = "merged";
    session.lifecycle.session.state = "idle";
    session.status = "merged";
    const lm = setupCheck("app-1", {
      session,
      configOverride: { ...config, budget: { perSessionUsd: 5 } },
    });

    await lm.check("app-1");

    // The budget path must not pause/interrupt a terminal session, regardless of
    // exactly which terminal/cleanup status the merge lifecycle derives.
    expect(lm.getStates().get("app-1")).not.toBe("needs_input");
    expect(plugins.runtime.interrupt).not.toHaveBeenCalled();
    const meta = readMetadataRaw(env.sessionsDir, "app-1");
    expect(meta!["budgetPausedAt"]).toBeFalsy();
  });

  it("enforces the cap on an active over-budget session in a PR-overlay idle state (review_pending)", async () => {
    // resolveOpenPRDecision() forces the canonical session state to "idle" for
    // review_pending even while the agent is still generating after opening a
    // PR. The cap must still be enforced — gating on canonical "working" alone
    // would miss this common post-PR path. (Mirrors the "keeps canonical session
    // state idle while waiting on external review" setup, with cost + a cap.)
    vi.mocked(plugins.agent.getActivityState).mockResolvedValue({ state: "active" });
    const mockSCM = createMockSCM({
      getReviewDecision: vi.fn().mockResolvedValue("pending"),
      enrichSessionsPRBatch: mockBatchEnrichment({ reviewDecision: "pending" }),
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });
    const session = makeSession({
      status: "pr_open",
      pr: makePR(),
      agentInfo: {
        summary: null,
        agentSessionId: null,
        cost: { inputTokens: 100, outputTokens: 50, estimatedCostUsd: 9.99 },
      },
    });
    vi.mocked(mockSessionManager.get).mockResolvedValue(session);
    writeMetadata(env.sessionsDir, "app-1", {
      worktree: "/tmp",
      branch: session.branch ?? "main",
      status: session.status,
      project: "my-app",
      pr: session.pr?.url,
      runtimeHandle: session.runtimeHandle ?? undefined,
    });
    const lm = createLifecycleManager({
      config: { ...config, budget: { perSessionUsd: 5 } },
      registry,
      sessionManager: mockSessionManager,
    });

    await lm.check("app-1");

    // Canonical state derived idle (review_pending), but active + over budget.
    expect(session.lifecycle.session.state).toBe("needs_input");
    expect(lm.getStates().get("app-1")).toBe("needs_input");
    expect(plugins.runtime.interrupt).toHaveBeenCalled();
  });

  it("keeps the budget pause latched (and sticky) when cost is transiently unavailable", async () => {
    // Already paused and still active, but this poll can't read cost
    // (getSessionInfo/log failure): agentInfo is null and no project aggregate
    // exists, so evaluateBudgetBreach sees $0 → no breach. That must NOT be read
    // as "under budget" — the latch and the needs_input pause must survive.
    vi.mocked(plugins.agent.getActivityState).mockResolvedValue({ state: "active" });
    const session = makeSession({ status: "working", agentInfo: null });
    session.metadata = {
      ...session.metadata,
      budgetPausedAt: "2026-01-01T00:00:00.000Z",
      budgetPausedReason: "budget_exceeded session $9.99 > $5.00",
      budgetInterrupted: "true",
    };
    const lm = setupCheck("app-1", {
      session,
      metaOverrides: {
        budgetPausedAt: "2026-01-01T00:00:00.000Z",
        budgetPausedReason: "budget_exceeded session $9.99 > $5.00",
        budgetInterrupted: "true",
      },
      configOverride: { ...config, budget: { perSessionUsd: 5 } },
    });

    await lm.check("app-1");

    // The latch (and the needs_input pause) survive the cost-read failure window.
    expect(lm.getStates().get("app-1")).toBe("needs_input");
    const meta = readMetadataRaw(env.sessionsDir, "app-1");
    expect(meta!["budgetPausedAt"]).toBeTruthy();
    expect(meta!["budgetInterrupted"]).toBe("true");
  });

  it("clears the pause latch when the cap is raised above current cost", async () => {
    vi.mocked(plugins.agent.getActivityState).mockResolvedValue({ state: "active" });
    const session = withCost(3.0);
    session.metadata = { ...session.metadata, budgetPausedAt: "2026-01-01T00:00:00.000Z" };
    // Cost is $3 but the (now raised) cap is $5 — no breach.
    const lm = setupCheck("app-1", {
      session,
      metaOverrides: { budgetPausedAt: "2026-01-01T00:00:00.000Z" },
      configOverride: { ...config, budget: { perSessionUsd: 5 } },
    });

    await lm.check("app-1");

    expect(lm.getStates().get("app-1")).toBe("working");
    const meta = readMetadataRaw(env.sessionsDir, "app-1");
    expect(meta!["budgetPausedAt"]).toBeFalsy();
  });
});

describe("check (single session)", () => {
  it("detects transition from spawning to working", async () => {
    const lm = setupCheck("app-1", {
      session: makeSession({ status: "spawning" }),
    });

    await lm.check("app-1");

    expect(lm.getStates().get("app-1")).toBe("working");
    const meta = readMetadataRaw(env.sessionsDir, "app-1");
    expect(meta!["status"]).toBe("working");
  });

  it("records lifecycle.transition when status changes", async () => {
    const lm = setupCheck("app-1", {
      session: makeSession({ status: "spawning" }),
    });

    await lm.check("app-1");

    expect(recordActivityEvent).toHaveBeenCalledWith({
      projectId: "my-app",
      sessionId: "app-1",
      source: "lifecycle",
      kind: "lifecycle.transition",
      level: "info",
      summary: "spawning → working",
      data: { from: "spawning", to: "working" },
    });
  });

  it("records activity.transition after observed activity changes", async () => {
    const session = makeSession({ id: "app-activity", status: "working" });
    const lm = setupCheck("app-activity", { session });

    await lm.check("app-activity");
    vi.mocked(recordActivityEvent).mockClear();
    vi.mocked(plugins.agent.getActivityState).mockResolvedValue({ state: "idle" });

    await lm.check("app-activity");

    expect(recordActivityEvent).toHaveBeenCalledWith({
      projectId: "my-app",
      sessionId: "app-activity",
      source: "lifecycle",
      kind: "activity.transition",
      summary: "active → idle",
      data: { from: "active", to: "idle" },
    });
  });

  it("records split lifecycle observability for transitions", async () => {
    const lm = setupCheck("app-1", {
      session: makeSession({ status: "spawning" }),
    });

    await lm.check("app-1");

    const summary = readObservabilitySummary(config);
    const trace = summary.projects["my-app"]?.recentTraces.find(
      (entry) => entry.operation === "lifecycle.transition" && entry.sessionId === "app-1",
    );

    expect(trace?.reason).toBe("task_in_progress");
    expect(trace?.data).toMatchObject({
      oldStatus: "spawning",
      newStatus: "working",
      previousSessionState: "not_started",
      newSessionState: "working",
      previousPRState: "none",
      newPRState: "none",
      previousRuntimeState: "alive",
      newRuntimeState: "alive",
      primaryReason: "task_in_progress",
      evidence: "activity_signal=valid via_native activity=active",
      signalsConsulted: ["activity_signal=valid", "via_native", "activity=active"],
      recoveryAction: null,
    });
  });

  it("does not mirror lifecycle transition observability logs to stderr during polling", async () => {
    const originalAoObservabilityStderr = process.env["AO_OBSERVABILITY_STDERR"];
    delete process.env["AO_OBSERVABILITY_STDERR"];

    const stderrSpy = vi.spyOn(process.stderr, "write").mockReturnValue(true);

    try {
      const lm = setupCheck("app-1", {
        session: makeSession({ status: "spawning" }),
      });

      await lm.check("app-1");

      expect(stderrSpy).not.toHaveBeenCalled();
    } finally {
      stderrSpy.mockRestore();
      if (originalAoObservabilityStderr === undefined) {
        delete process.env["AO_OBSERVABILITY_STDERR"];
      } else {
        process.env["AO_OBSERVABILITY_STDERR"] = originalAoObservabilityStderr;
      }
    }
  });

  it("clears stale lifecycle compatibility metadata in memory and on disk", async () => {
    const session = makeSession({
      status: "working",
      lifecycle: {
        ...makeSession().lifecycle,
        pr: {
          state: "none",
          reason: "not_created",
          number: null,
          url: null,
          lastObservedAt: null,
        },
        runtime: {
          state: "alive",
          reason: "process_running",
          lastObservedAt: null,
          handle: null,
          tmuxName: null,
        },
      },
      runtimeHandle: null,
      pr: null,
      metadata: {
        pr: "https://github.com/org/repo/pull/42",
        runtimeHandle: JSON.stringify({ id: "stale", runtimeName: "mock", data: {} }),
        tmuxName: "stale-tmux",
        role: "orchestrator",
      },
    });
    const staleHandle = { id: "stale", runtimeName: "mock", data: {} };
    const persistedMetadata = {
      worktree: "/tmp",
      branch: session.branch ?? "main",
      status: session.status,
      project: "my-app",
      pr: "https://github.com/org/repo/pull/42",
      runtimeHandle: staleHandle,
      tmuxName: "stale-tmux",
      role: "orchestrator",
    };
    const currentSession = {
      ...session,
      metadata: {
        ...session.metadata,
        ...persistedMetadata,
        runtimeHandle: JSON.stringify(staleHandle),
      },
    };

    vi.mocked(mockSessionManager.get).mockResolvedValue(currentSession);
    writeMetadata(env.sessionsDir, "app-1", persistedMetadata);

    const lm = createLifecycleManager({
      config,
      registry: mockRegistry,
      sessionManager: mockSessionManager,
    });

    await lm.check("app-1");

    const metadata = readMetadataRaw(env.sessionsDir, "app-1");
    expect(metadata?.["pr"]).toBeUndefined();
    expect(metadata?.["runtimeHandle"]).toBeUndefined();
    expect(metadata?.["tmuxName"]).toBeUndefined();
    expect(metadata?.["role"]).toBeUndefined();
    expect(currentSession.metadata["pr"]).toBeUndefined();
    expect(currentSession.metadata["runtimeHandle"]).toBeUndefined();
    expect(currentSession.metadata["tmuxName"]).toBeUndefined();
    expect(currentSession.metadata["role"]).toBeUndefined();
  });

  it("does not kill a spawning session when its runtime handle has not been persisted yet", async () => {
    vi.mocked(plugins.runtime.isAlive).mockResolvedValue(false);

    const lm = setupCheck("app-1", {
      session: makeSession({
        status: "spawning",
        runtimeHandle: { id: "app-1", runtimeName: "mock", data: {} },
        metadata: {},
      }),
      metaOverrides: {
        runtimeHandle: undefined,
        tmuxName: undefined,
      },
    });

    await lm.check("app-1");

    expect(lm.getStates().get("app-1")).toBe("working");
    expect(plugins.runtime.isAlive).not.toHaveBeenCalled();
  });

  it("does not kill a spawning session even when runtimeHandle IS persisted in metadata (#1035)", async () => {
    vi.mocked(plugins.runtime.isAlive).mockResolvedValue(false);

    const lm = setupCheck("app-1", {
      session: makeSession({
        status: "spawning",
        runtimeHandle: { id: "app-1", runtimeName: "mock", data: {} },
        metadata: {},
      }),
      // runtimeHandle IS in metadata — this is the production scenario
    });

    await lm.check("app-1");

    expect(lm.getStates().get("app-1")).toBe("working");
    expect(plugins.runtime.isAlive).not.toHaveBeenCalled();
  });

  it("does not kill a spawning session when agent reports exited activity (#1035)", async () => {
    vi.mocked(plugins.agent.getActivityState).mockResolvedValue({
      state: "exited" as ActivityState,
      timestamp: new Date(),
    });

    const lm = setupCheck("app-1", {
      session: makeSession({
        status: "spawning",
        runtimeHandle: { id: "app-1", runtimeName: "mock", data: {} },
        metadata: {},
      }),
    });

    await lm.check("app-1");

    // Should transition to working, not killed
    expect(lm.getStates().get("app-1")).toBe("working");
  });

  it("still probes a working session when it relies on a synthesized runtime handle", async () => {
    vi.mocked(plugins.runtime.isAlive).mockResolvedValue(false);

    const lm = setupCheck("app-1", {
      session: makeSession({
        status: "working",
        runtimeHandle: { id: "app-1", runtimeName: "mock", data: {} },
        metadata: {},
      }),
      metaOverrides: {
        runtimeHandle: undefined,
        tmuxName: undefined,
      },
    });

    await lm.check("app-1");

    expect(plugins.runtime.isAlive).toHaveBeenCalledWith({
      id: "app-1",
      runtimeName: "mock",
      data: {},
    });
    expect(lm.getStates().get("app-1")).toBe("detecting");
  });

  it("uses persisted session agent even when worker config differs", async () => {
    const codexAgent: Agent = {
      ...plugins.agent,
      name: "codex",
      processName: "codex",
      getActivityState: vi.fn().mockResolvedValue({ state: "active" as ActivityState }),
    };

    const registryWithMultipleAgents: PluginRegistry = {
      ...mockRegistry,
      get: vi.fn().mockImplementation((slot: string, name: string) => {
        if (slot === "runtime") return plugins.runtime;
        if (slot === "agent") {
          if (name === "codex") return codexAgent;
          if (name === "mock-agent") return plugins.agent;
        }
        return null;
      }),
    };

    const configWithWorkerAgent: OrchestratorConfig = {
      ...config,
      projects: {
        ...config.projects,
        "my-app": {
          ...config.projects["my-app"],
          agent: "mock-agent",
          worker: { agent: "mock-agent" },
        },
      },
    };

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "working", metadata: { agent: "codex" } }),
      registry: registryWithMultipleAgents,
      configOverride: configWithWorkerAgent,
    });

    await lm.check("app-1");

    expect(codexAgent.getActivityState).toHaveBeenCalled();
    expect(plugins.agent.getActivityState).not.toHaveBeenCalled();
  });

  it("detects killed state when runtime is dead", async () => {
    vi.mocked(plugins.runtime.isAlive).mockResolvedValue(false);
    vi.mocked(plugins.agent.getActivityState).mockResolvedValue({ state: "idle" });
    vi.mocked(plugins.agent.isProcessRunning).mockResolvedValue(false);

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "working" }),
    });

    await lm.check("app-1");
    expect(lm.getStates().get("app-1")).toBe("killed");
  });

  it("detects killed state when getActivityState returns exited", async () => {
    vi.mocked(plugins.agent.getActivityState).mockResolvedValue({ state: "exited" });
    vi.mocked(plugins.runtime.isAlive).mockResolvedValue(true);

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "working" }),
    });

    await lm.check("app-1");
    expect(lm.getStates().get("app-1")).toBe("detecting");
  });

  it("detects killed via terminal fallback when getActivityState returns null", async () => {
    vi.mocked(plugins.agent.getActivityState).mockResolvedValue(null);
    vi.mocked(plugins.agent.detectActivity).mockReturnValue("idle");
    vi.mocked(plugins.runtime.isAlive).mockResolvedValue(false);
    vi.mocked(plugins.agent.isProcessRunning).mockResolvedValue(false);

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "working" }),
    });

    await lm.check("app-1");
    expect(lm.getStates().get("app-1")).toBe("killed");
  });

  it("enters detecting when runtime is dead but recent activity is still fresh", async () => {
    vi.mocked(plugins.runtime.isAlive).mockResolvedValue(false);
    vi.mocked(plugins.agent.getActivityState).mockResolvedValue({
      state: "active",
      timestamp: new Date(Date.now() - 30_000),
    });
    vi.mocked(plugins.agent.isProcessRunning).mockResolvedValue(false);

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "working" }),
    });

    await lm.check("app-1");

    expect(lm.getStates().get("app-1")).toBe("detecting");
    const meta = readMetadataRaw(env.sessionsDir, "app-1");
    expect(meta?.["detectingAttempts"]).toBe("1");
    expect(meta?.["lifecycleEvidence"]).toContain("signal_disagreement");
  });

  it("enters detecting when runtime is dead but process state is unknown", async () => {
    const registryWithoutAgent = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
    });
    vi.mocked(registryWithoutAgent.get).mockImplementation((slot: string, _name?: string) => {
      if (slot === "runtime") return plugins.runtime;
      if (slot === "agent") return null;
      return null;
    });
    vi.mocked(plugins.runtime.isAlive).mockResolvedValue(false);

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "working" }),
      registry: registryWithoutAgent,
    });

    await lm.check("app-1");

    expect(lm.getStates().get("app-1")).toBe("detecting");
    const meta = readMetadataRaw(env.sessionsDir, "app-1");
    expect(meta?.["lifecycleEvidence"]).toContain("runtime_dead process_unknown");
    expect(meta?.["detectingAttempts"]).toBe("1");
  });

  it("escalates detecting to stuck after bounded retries", async () => {
    vi.mocked(plugins.runtime.isAlive).mockResolvedValue(false);
    vi.mocked(plugins.agent.getActivityState).mockResolvedValue({
      state: "active",
      timestamp: new Date(Date.now() - 30_000),
    });
    vi.mocked(plugins.agent.isProcessRunning).mockResolvedValue(false);

    const lm = setupCheck("app-1", {
      session: makeSession({
        status: "detecting",
        metadata: { detectingAttempts: "3" },
      }),
      metaOverrides: {
        detectingAttempts: "3",
      },
    });

    await lm.check("app-1");

    expect(lm.getStates().get("app-1")).toBe("stuck");
    const meta = readMetadataRaw(env.sessionsDir, "app-1");
    expect(meta?.["detectingAttempts"]).toBe("4");
    expect(meta?.["detectingEscalatedAt"]).toBeDefined();
  });

  it("stays working when agent is idle but process is still running (fallback path)", async () => {
    vi.mocked(plugins.agent.getActivityState).mockResolvedValue(null);
    vi.mocked(plugins.agent.detectActivity).mockReturnValue("idle");
    vi.mocked(plugins.agent.isProcessRunning).mockResolvedValue(true);

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "working" }),
    });

    await lm.check("app-1");
    expect(lm.getStates().get("app-1")).toBe("working");
  });

  it("leaves lifecycle metadata untouched when process probe is indeterminate", async () => {
    vi.mocked(plugins.runtime.isAlive).mockResolvedValue(true);
    vi.mocked(plugins.agent.getActivityState).mockResolvedValue(null);
    vi.mocked(plugins.agent.isProcessRunning).mockResolvedValue("indeterminate");

    const session = makeSession({
      status: "working",
      workspacePath: null,
      metadata: {
        lifecycleEvidence: "previous_evidence",
        detectingAttempts: "2",
      },
    });
    const lifecycle = JSON.stringify(session.lifecycle);
    const lm = setupCheck("app-1", {
      session,
      metaOverrides: {
        lifecycle,
        lifecycleEvidence: "previous_evidence",
        detectingAttempts: "2",
      },
    });
    const before = readMetadataRaw(env.sessionsDir, "app-1");

    await lm.check("app-1");

    expect(readMetadataRaw(env.sessionsDir, "app-1")).toEqual(before);
    expect(lm.getStates().get("app-1")).toBe("working");
  });

  it("does not mark a session stuck from terminal-only idle evidence without a timestamp", async () => {
    config.reactions = {
      "agent-stuck": { auto: true, action: "notify", threshold: "1m" },
    };

    vi.mocked(plugins.agent.getActivityState).mockResolvedValue(null);
    vi.mocked(plugins.agent.detectActivity).mockReturnValue("idle");
    vi.mocked(plugins.agent.isProcessRunning).mockResolvedValue(true);

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "working" }),
    });

    await lm.check("app-1");

    expect(lm.getStates().get("app-1")).toBe("working");
    const meta = readMetadataRaw(env.sessionsDir, "app-1");
    expect(meta?.["lifecycleEvidence"]).toContain("activity_signal=stale");
    expect(meta?.["lifecycleEvidence"]).toContain("activity=idle");
  });

  it("does not treat stale activity as recent liveness evidence during runtime-loss detection", async () => {
    vi.mocked(plugins.runtime.isAlive).mockResolvedValue(false);
    vi.mocked(plugins.agent.getActivityState).mockResolvedValue({
      state: "active",
      timestamp: new Date(Date.now() - 10 * 60_000),
    });
    vi.mocked(plugins.agent.isProcessRunning).mockResolvedValue(false);

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "working" }),
    });

    await lm.check("app-1");

    expect(lm.getStates().get("app-1")).toBe("killed");
    const meta = readMetadataRaw(env.sessionsDir, "app-1");
    expect(meta?.["lifecycleEvidence"]).toContain("activity_signal=stale");
  });

  it("records explicit probe-failure activity evidence", async () => {
    vi.mocked(plugins.agent.getActivityState).mockRejectedValue(new Error("boom"));

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "working" }),
    });

    await lm.check("app-1");

    expect(lm.getStates().get("app-1")).toBe("detecting");
    const meta = readMetadataRaw(env.sessionsDir, "app-1");
    expect(meta?.["lifecycleEvidence"]).toContain("activity_signal=probe_failure");
  });

  it("degrades stuck probe-failure sessions to detecting when runtime is alive but activity is unavailable", async () => {
    const registryWithoutAgent: PluginRegistry = {
      ...mockRegistry,
      get: vi.fn().mockImplementation((slot: string) => {
        if (slot === "runtime") return plugins.runtime;
        return null;
      }),
    };

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "stuck" }),
      registry: registryWithoutAgent,
    });

    await lm.check("app-1");

    expect(lm.getStates().get("app-1")).toBe("detecting");
    const meta = readMetadataRaw(env.sessionsDir, "app-1");
    expect(meta?.["lifecycleEvidence"]).toContain("activity_signal=unavailable");
  });

  it("detects needs_input from agent", async () => {
    vi.mocked(plugins.agent.getActivityState).mockResolvedValue({ state: "waiting_input" });

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "working" }),
    });

    await lm.check("app-1");
    expect(lm.getStates().get("app-1")).toBe("needs_input");
  });

  it("transitions to stuck when idle exceeds agent-stuck threshold (OpenCode-style activity)", async () => {
    config.reactions = {
      "agent-stuck": { auto: true, action: "notify", threshold: "1m" },
    };

    vi.mocked(plugins.agent.getActivityState).mockResolvedValue({
      state: "idle",
      timestamp: new Date(Date.now() - 120_000),
    });

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "working", metadata: { agent: "mock-agent" } }),
      metaOverrides: { agent: "mock-agent" },
    });

    await lm.check("app-1");
    expect(lm.getStates().get("app-1")).toBe("stuck");
  });

  it("uses global agent-stuck threshold when project override omits threshold", async () => {
    config.reactions = {
      "agent-stuck": { auto: true, action: "notify", threshold: "1m" },
    };
    config.projects["my-app"] = {
      ...config.projects["my-app"],
      reactions: { "agent-stuck": { auto: true, action: "notify" } },
    };

    vi.mocked(plugins.agent.getActivityState).mockResolvedValue({
      state: "idle",
      timestamp: new Date(Date.now() - 120_000),
    });

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "working", metadata: { agent: "mock-agent" } }),
      metaOverrides: { agent: "mock-agent" },
    });

    await lm.check("app-1");
    expect(lm.getStates().get("app-1")).toBe("stuck");
  });

  it("still auto-detects PR before marking idle sessions as stuck", async () => {
    config.reactions = {
      "agent-stuck": { auto: true, action: "notify", threshold: "1m" },
    };

    const mockSCM = createMockSCM({
      detectPR: vi.fn().mockResolvedValue(makePR()),
      getMergeability: vi.fn().mockResolvedValue({
        mergeable: false,
        ciPassing: true,
        approved: false,
        noConflicts: true,
        blockers: [],
      }),
    });

    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });

    vi.mocked(plugins.agent.getActivityState).mockResolvedValue({
      state: "idle",
      timestamp: new Date(Date.now() - 120_000),
    });

    const lm = setupCheck("app-1", {
      session: makeSession({
        status: "working",
        branch: "feat/test",
        pr: null,
        workspacePath: null,
        metadata: { agent: "mock-agent" },
      }),
      metaOverrides: { branch: "feat/test", agent: "mock-agent" },
      registry,
    });

    await lm.check("app-1");

    expect(mockSCM.detectPR).toHaveBeenCalledOnce();
    const meta = readMetadataRaw(env.sessionsDir, "app-1");
    expect(meta?.["pr"]).toBe(makePR().url);
    expect(lm.getStates().get("app-1")).toBe("stuck");
  });

  it("keeps prs metadata deduplicated across repeated detectPR polls", async () => {
    const detectedPR = makePR({
      owner: "aoagents",
      repo: "ReverbCode",
      number: 143,
      url: "https://github.com/aoagents/ReverbCode/pull/143",
    });
    const mockSCM = createMockSCM({
      detectPR: vi.fn().mockResolvedValue(detectedPR),
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      workspace: plugins.workspace,
      scm: mockSCM,
    });
    writeMetadata(env.sessionsDir, "app-1", {
      worktree: "/tmp",
      branch: "feat/reverb-fix",
      status: "working",
      project: "my-app",
      agent: "mock-agent",
    } as SessionMetadata);
    const realSessionManager = createSessionManager({ config, registry });
    const lm = createLifecycleManager({ config, registry, sessionManager: realSessionManager });

    for (let i = 0; i < 10; i += 1) {
      await lm.check("app-1");
    }

    const meta = readMetadataRaw(env.sessionsDir, "app-1");
    expect(meta?.["pr"]).toBe(detectedPR.url);
    expect(meta?.["prs"]?.split(",")).toEqual([detectedPR.url]);
    expect(mockSCM.detectPR).toHaveBeenCalledTimes(10);
  });

  it("refreshes worker branch metadata from the current worktree HEAD before PR detection", async () => {
    const workspacePath = join(env.tmpDir, "worker-ws");
    const gitDir = join(env.tmpDir, "repo", ".git", "worktrees", "app-1");
    mkdirSync(workspacePath, { recursive: true });
    mkdirSync(gitDir, { recursive: true });
    writeFileSync(join(workspacePath, ".git"), `gitdir: ${gitDir}\n`);
    writeFileSync(join(gitDir, "HEAD"), "ref: refs/heads/fix-login-v2\n");

    const mockSCM = createMockSCM({ detectPR: vi.fn().mockResolvedValue(null) });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });

    const lm = setupCheck("app-1", {
      session: makeSession({
        status: "working",
        branch: "fix-login",
        workspacePath,
        pr: null,
        metadata: { agent: "mock-agent" },
      }),
      metaOverrides: {
        worktree: workspacePath,
        branch: "fix-login",
        agent: "mock-agent",
      },
      registry,
    });

    await lm.check("app-1");

    expect(mockSCM.detectPR).toHaveBeenCalledWith(
      expect.objectContaining({ branch: "fix-login-v2" }),
      expect.anything(),
    );
    const meta = readMetadataRaw(env.sessionsDir, "app-1");
    expect(meta?.["branch"]).toBe("fix-login-v2");
  });

  it("refreshes worker branch metadata for clone-style repos with a .git directory", async () => {
    const workspacePath = join(env.tmpDir, "worker-clone");
    const gitDir = join(workspacePath, ".git");
    mkdirSync(gitDir, { recursive: true });
    writeFileSync(join(gitDir, "HEAD"), "ref: refs/heads/fix-login-v2\n");

    const mockSCM = createMockSCM({ detectPR: vi.fn().mockResolvedValue(null) });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });

    const lm = setupCheck("app-1", {
      session: makeSession({
        status: "working",
        branch: "fix-login",
        workspacePath,
        pr: null,
        metadata: { agent: "mock-agent" },
      }),
      metaOverrides: {
        worktree: workspacePath,
        branch: "fix-login",
        agent: "mock-agent",
      },
      registry,
    });

    await lm.check("app-1");

    expect(mockSCM.detectPR).toHaveBeenCalledWith(
      expect.objectContaining({ branch: "fix-login-v2" }),
      expect.anything(),
    );
    const meta = readMetadataRaw(env.sessionsDir, "app-1");
    expect(meta?.["branch"]).toBe("fix-login-v2");
  });

  it("does not overwrite an attached PR branch from a workspace checkout change", async () => {
    const workspacePath = join(env.tmpDir, "worker-ws-pr");
    const gitDir = join(env.tmpDir, "repo", ".git", "worktrees", "app-1-pr");
    mkdirSync(workspacePath, { recursive: true });
    mkdirSync(gitDir, { recursive: true });
    writeFileSync(join(workspacePath, ".git"), `gitdir: ${gitDir}\n`);
    writeFileSync(join(gitDir, "HEAD"), "ref: refs/heads/fix-login-v2\n");

    const pr = makePR({ branch: "fix-login" });
    const mockSCM = createMockSCM({ detectPR: vi.fn().mockResolvedValue(null) });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });

    const lm = setupCheck("app-1", {
      session: makeSession({
        status: "pr_open",
        branch: "fix-login",
        workspacePath,
        pr,
        metadata: { agent: "mock-agent" },
      }),
      metaOverrides: {
        worktree: workspacePath,
        branch: "fix-login",
        pr: pr.url,
        agent: "mock-agent",
      },
      registry,
    });

    await lm.check("app-1");

    expect(mockSCM.detectPR).not.toHaveBeenCalled();
    expect(pr.branch).toBe("fix-login");
    const meta = readMetadataRaw(env.sessionsDir, "app-1");
    expect(meta?.["branch"]).toBe("fix-login");
  });

  it("refreshes branch metadata again after a closed PR when the worker switches branches", async () => {
    const workspacePath = join(env.tmpDir, "worker-ws-closed-pr");
    const gitDir = join(env.tmpDir, "repo", ".git", "worktrees", "app-1-closed-pr");
    mkdirSync(workspacePath, { recursive: true });
    mkdirSync(gitDir, { recursive: true });
    writeFileSync(join(workspacePath, ".git"), `gitdir: ${gitDir}\n`);
    writeFileSync(join(gitDir, "HEAD"), "ref: refs/heads/follow-up-fix\n");

    const closedPR = makePR({ branch: "fix-login", url: "https://github.com/org/repo/pull/42" });
    const followUpPR = makePR({
      number: 43,
      branch: "follow-up-fix",
      url: "https://github.com/org/repo/pull/43",
      title: "Follow up fix",
    });
    const mockSCM = createMockSCM({
      detectPR: vi.fn().mockResolvedValue(followUpPR),
      // Enrichment cache must show closedPR as closed so the detectPR filter
      // can remove it using per-PR state rather than the aggregate lifecycle state.
      getPRState: vi.fn().mockImplementation((pr: PRInfo) =>
        Promise.resolve(pr.number === closedPR.number ? "closed" : "open"),
      ),
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });

    const session = makeSession({
      status: "idle",
      branch: "fix-login",
      workspacePath,
      pr: closedPR,
      metadata: { agent: "mock-agent" },
    });
    session.lifecycle.pr.state = "closed";
    session.lifecycle.pr.reason = "closed_unmerged";
    session.lifecycle.pr.number = closedPR.number;
    session.lifecycle.pr.url = closedPR.url;
    session.lifecycle.pr.lastObservedAt = new Date().toISOString();
    session.lifecycle.session.state = "idle";
    session.lifecycle.session.reason = "pr_closed_waiting_decision";

    const lm = setupCheck("app-1", {
      session,
      metaOverrides: {
        worktree: workspacePath,
        branch: "fix-login",
        pr: closedPR.url,
        agent: "mock-agent",
      },
      registry,
    });

    await lm.check("app-1");

    expect(mockSCM.detectPR).toHaveBeenCalledWith(
      expect.objectContaining({ branch: "follow-up-fix" }),
      expect.anything(),
    );
    const meta = readMetadataRaw(env.sessionsDir, "app-1");
    expect(meta?.["branch"]).toBe("follow-up-fix");
    expect(meta?.["pr"]).toBe(followUpPR.url);
  });

  it("clears stale worker branch metadata when the current worktree HEAD is detached", async () => {
    const workspacePath = join(env.tmpDir, "worker-ws-detached");
    const gitDir = join(env.tmpDir, "repo", ".git", "worktrees", "app-1-detached");
    mkdirSync(workspacePath, { recursive: true });
    mkdirSync(gitDir, { recursive: true });
    writeFileSync(join(workspacePath, ".git"), `gitdir: ${gitDir}\n`);
    writeFileSync(join(gitDir, "HEAD"), "6f1d2c3b4a5e67890123456789abcdef01234567\n");

    const mockSCM = createMockSCM({ detectPR: vi.fn().mockResolvedValue(null) });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });

    const lm = setupCheck("app-1", {
      session: makeSession({
        status: "working",
        branch: "fix-login",
        workspacePath,
        pr: null,
        metadata: { agent: "mock-agent" },
      }),
      metaOverrides: {
        worktree: workspacePath,
        branch: "fix-login",
        agent: "mock-agent",
      },
      registry,
    });

    await lm.check("app-1");

    expect(mockSCM.detectPR).not.toHaveBeenCalled();
    const meta = readMetadataRaw(env.sessionsDir, "app-1");
    expect(meta?.["branch"]).toBeUndefined();
  });

  for (const marker of [
    "rebase-merge",
    "rebase-apply",
    "CHERRY_PICK_HEAD",
    "BISECT_LOG",
  ] as const) {
    it(`keeps the previous branch during transient detached git state: ${marker}`, async () => {
      const workspacePath = join(env.tmpDir, `worker-ws-${marker}`);
      const gitDir = join(env.tmpDir, "repo", ".git", "worktrees", `app-1-${marker}`);
      mkdirSync(workspacePath, { recursive: true });
      mkdirSync(gitDir, { recursive: true });
      writeFileSync(join(workspacePath, ".git"), `gitdir: ${gitDir}\n`);
      writeFileSync(join(gitDir, "HEAD"), "6f1d2c3b4a5e67890123456789abcdef01234567\n");
      if (marker.includes("/")) {
        mkdirSync(join(gitDir, marker), { recursive: true });
      } else {
        if (marker === "rebase-merge" || marker === "rebase-apply") {
          mkdirSync(join(gitDir, marker), { recursive: true });
        } else {
          writeFileSync(join(gitDir, marker), "in-progress\n");
        }
      }

      const mockSCM = createMockSCM({ detectPR: vi.fn().mockResolvedValue(null) });
      const registry = createMockRegistry({
        runtime: plugins.runtime,
        agent: plugins.agent,
        scm: mockSCM,
      });

      const lm = setupCheck("app-1", {
        session: makeSession({
          status: "working",
          branch: "fix-login",
          workspacePath,
          pr: null,
          metadata: { agent: "mock-agent" },
        }),
        metaOverrides: {
          worktree: workspacePath,
          branch: "fix-login",
          agent: "mock-agent",
        },
        registry,
      });

      await lm.check("app-1");

      expect(mockSCM.detectPR).toHaveBeenCalledWith(
        expect.objectContaining({ branch: "fix-login" }),
        expect.anything(),
      );
      const meta = readMetadataRaw(env.sessionsDir, "app-1");
      expect(meta?.["branch"]).toBe("fix-login");
    });
  }

  it("keeps existing branch metadata when the current worktree HEAD cannot be read", async () => {
    const workspacePath = join(env.tmpDir, "worker-ws-missing-head");
    const gitDir = join(env.tmpDir, "repo", ".git", "worktrees", "app-1-missing-head");
    mkdirSync(workspacePath, { recursive: true });
    mkdirSync(gitDir, { recursive: true });
    writeFileSync(join(workspacePath, ".git"), `gitdir: ${gitDir}\n`);

    const mockSCM = createMockSCM({ detectPR: vi.fn().mockResolvedValue(null) });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });

    const lm = setupCheck("app-1", {
      session: makeSession({
        status: "working",
        branch: "fix-login",
        workspacePath,
        pr: null,
        metadata: { agent: "mock-agent" },
      }),
      metaOverrides: {
        worktree: workspacePath,
        branch: "fix-login",
        agent: "mock-agent",
      },
      registry,
    });

    await lm.check("app-1");

    expect(mockSCM.detectPR).toHaveBeenCalledWith(
      expect.objectContaining({ branch: "fix-login" }),
      expect.anything(),
    );
    const meta = readMetadataRaw(env.sessionsDir, "app-1");
    expect(meta?.["branch"]).toBe("fix-login");
  });

  it("does not adopt a branch already tracked by another active worker", async () => {
    const workspacePath = join(env.tmpDir, "worker-ws-conflict");
    const gitDir = join(env.tmpDir, "repo", ".git", "worktrees", "app-1-conflict");
    mkdirSync(workspacePath, { recursive: true });
    mkdirSync(gitDir, { recursive: true });
    writeFileSync(join(workspacePath, ".git"), `gitdir: ${gitDir}\n`);
    writeFileSync(join(gitDir, "HEAD"), "ref: refs/heads/fix-login-v2\n");

    const mockSCM = createMockSCM({ detectPR: vi.fn().mockResolvedValue(null) });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });

    const session = makeSession({
      id: "app-1",
      status: "working",
      branch: "fix-login",
      workspacePath,
      pr: null,
      metadata: { agent: "mock-agent" },
    });
    const sibling = makeSession({
      id: "app-2",
      status: "working",
      branch: "fix-login-v2",
      workspacePath: null,
      pr: null,
      metadata: { agent: "mock-agent" },
    });

    const lm = setupCheck("app-1", {
      session,
      metaOverrides: {
        worktree: workspacePath,
        branch: "fix-login",
        agent: "mock-agent",
      },
      registry,
    });
    vi.mocked(mockSessionManager.list).mockResolvedValue([session, sibling]);

    await lm.check("app-1");

    expect(mockSCM.detectPR).toHaveBeenCalledWith(
      expect.objectContaining({ branch: "fix-login" }),
      expect.anything(),
    );
    const meta = readMetadataRaw(env.sessionsDir, "app-1");
    expect(meta?.["branch"]).toBe("fix-login");
  });

  it("serializes competing branch adoption within one poll cycle without extra session list calls", async () => {
    const workspacePathA = join(env.tmpDir, "worker-ws-race-a");
    const workspacePathB = join(env.tmpDir, "worker-ws-race-b");
    const gitDirA = join(env.tmpDir, "repo", ".git", "worktrees", "app-1-race");
    const gitDirB = join(env.tmpDir, "repo", ".git", "worktrees", "app-2-race");
    mkdirSync(workspacePathA, { recursive: true });
    mkdirSync(workspacePathB, { recursive: true });
    mkdirSync(gitDirA, { recursive: true });
    mkdirSync(gitDirB, { recursive: true });
    writeFileSync(join(workspacePathA, ".git"), `gitdir: ${gitDirA}\n`);
    writeFileSync(join(workspacePathB, ".git"), `gitdir: ${gitDirB}\n`);
    writeFileSync(join(gitDirA, "HEAD"), "ref: refs/heads/shared-branch\n");
    writeFileSync(join(gitDirB, "HEAD"), "ref: refs/heads/shared-branch\n");

    const sessionA = makeSession({
      id: "app-1",
      status: "working",
      branch: "old-a",
      workspacePath: workspacePathA,
      pr: null,
      metadata: { agent: "mock-agent" },
    });
    const sessionB = makeSession({
      id: "app-2",
      status: "working",
      branch: "old-b",
      workspacePath: workspacePathB,
      pr: null,
      metadata: { agent: "mock-agent" },
    });
    vi.mocked(mockSessionManager.list).mockResolvedValue([sessionA, sessionB]);

    const lm = createLifecycleManager({
      config,
      registry: mockRegistry,
      sessionManager: mockSessionManager,
    });

    try {
      lm.start(60_000);
      // Poll for the cycle to finish — Windows fs is slower, a fixed 25ms wait
      // can race past the lifecycle list() call before adoption resolves.
      const deadline = Date.now() + 2000;
      while (Date.now() < deadline) {
        const adoptedCount = [sessionA.branch, sessionB.branch].filter(
          (branch) => branch === "shared-branch",
        ).length;
        if (vi.mocked(mockSessionManager.list).mock.calls.length >= 1 && adoptedCount > 0) {
          break;
        }
        await new Promise((resolve) => setTimeout(resolve, 10));
      }

      const adoptedCount = [sessionA.branch, sessionB.branch].filter(
        (branch) => branch === "shared-branch",
      ).length;
      expect(adoptedCount).toBe(1);
      expect(mockSessionManager.list).toHaveBeenCalledTimes(1);
    } finally {
      lm.stop();
    }
  });

  it("skips branch refresh for orchestrator sessions", async () => {
    const workspacePath = join(env.tmpDir, "orchestrator-ws");
    const gitDir = join(env.tmpDir, "repo", ".git", "worktrees", "app-orchestrator-1");
    mkdirSync(workspacePath, { recursive: true });
    mkdirSync(gitDir, { recursive: true });
    writeFileSync(join(workspacePath, ".git"), `gitdir: ${gitDir}\n`);
    writeFileSync(join(gitDir, "HEAD"), "ref: refs/heads/orchestrator-new\n");

    const mockSCM = createMockSCM({ detectPR: vi.fn().mockResolvedValue(null) });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });

    const lm = setupCheck("app-orchestrator-1", {
      session: makeSession({
        id: "app-orchestrator-1",
        status: "working",
        branch: "orchestrator-old",
        workspacePath,
        pr: null,
        metadata: { agent: "mock-agent", role: "orchestrator" },
      }),
      metaOverrides: {
        worktree: workspacePath,
        branch: "orchestrator-old",
        role: "orchestrator",
        agent: "mock-agent",
      },
      registry,
    });

    await lm.check("app-orchestrator-1");

    expect(mockSCM.detectPR).not.toHaveBeenCalled();
    const meta = readMetadataRaw(env.sessionsDir, "app-orchestrator-1");
    expect(meta?.["branch"]).toBe("orchestrator-old");
  });

  it("preserves stuck state when getActivityState throws", async () => {
    vi.mocked(plugins.agent.getActivityState).mockRejectedValue(new Error("probe failed"));

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "stuck" }),
    });

    await lm.check("app-1");
    expect(lm.getStates().get("app-1")).toBe("stuck");
  });

  it("preserves needs_input state when getActivityState throws", async () => {
    vi.mocked(plugins.agent.getActivityState).mockRejectedValue(new Error("probe failed"));

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "needs_input" }),
    });

    await lm.check("app-1");
    expect(lm.getStates().get("app-1")).toBe("needs_input");
  });

  it("preserves stuck state when getActivityState returns null and getOutput throws", async () => {
    vi.mocked(plugins.agent.getActivityState).mockResolvedValue(null);
    vi.mocked(plugins.runtime.getOutput).mockRejectedValue(new Error("tmux error"));

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "stuck" }),
    });

    await lm.check("app-1");
    expect(lm.getStates().get("app-1")).toBe("stuck");
  });

  it("preserves needs_input state when getActivityState returns null with no terminal evidence", async () => {
    vi.mocked(plugins.agent.getActivityState).mockResolvedValue(null);
    vi.mocked(plugins.runtime.getOutput).mockResolvedValue("");

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "needs_input" }),
    });

    await lm.check("app-1");
    expect(lm.getStates().get("app-1")).toBe("needs_input");
  });

  it("preserves stuck state across repeated polls with unchanged weak evidence", async () => {
    vi.mocked(plugins.agent.getActivityState).mockResolvedValue(null);
    vi.mocked(plugins.runtime.getOutput).mockResolvedValue("");

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "stuck" }),
    });

    await lm.check("app-1");
    expect(lm.getStates().get("app-1")).toBe("stuck");

    await lm.check("app-1");
    expect(lm.getStates().get("app-1")).toBe("stuck");
  });

  it("preserves needs_input across repeated polls with unchanged weak evidence", async () => {
    vi.mocked(plugins.agent.getActivityState).mockResolvedValue(null);
    vi.mocked(plugins.runtime.getOutput).mockResolvedValue("");

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "needs_input" }),
    });

    await lm.check("app-1");
    expect(lm.getStates().get("app-1")).toBe("needs_input");

    await lm.check("app-1");
    expect(lm.getStates().get("app-1")).toBe("needs_input");
  });

  it("preserves canonical needs_input when persisted status is stale working", async () => {
    vi.mocked(plugins.agent.getActivityState).mockResolvedValue(null);
    vi.mocked(plugins.runtime.getOutput).mockResolvedValue("");

    const session = makeSession({ status: "working" });
    session.lifecycle.session.state = "needs_input";
    session.lifecycle.session.reason = "awaiting_user_input";

    const lm = setupCheck("app-1", {
      session,
      metaOverrides: {
        status: "working",
      },
    });

    await lm.check("app-1");
    expect(lm.getStates().get("app-1")).toBe("needs_input");
    const meta = readMetadataRaw(env.sessionsDir, "app-1");
    expect(meta?.["status"]).toBe("needs_input");
  });

  it("detects PR states from SCM", async () => {
    vi.useFakeTimers();
    try {
      const pr = makeMatchingPR();
      const mockSCM = createMockSCM({
        getCISummary: vi.fn().mockResolvedValue("failing"),
        enrichSessionsPRBatch: mockBatchEnrichment({ ciStatus: "failing" }),
      });
      const registry = createMockRegistry({
        runtime: plugins.runtime,
        agent: plugins.agent,
        scm: mockSCM,
      });

      const lm = setupPollCheck("app-1", {
        session: makeSession({ status: "pr_open", pr }),
        registry,
      });

      lm.start(60_000);
      await vi.advanceTimersByTimeAsync(0);
      lm.stop();
      expect(lm.getStates().get("app-1")).toBe("ci_failed");
    } finally {
      vi.useRealTimers();
    }
  });

  it("keeps canonical session state idle while waiting on external review", async () => {
    const mockSCM = createMockSCM({
      getReviewDecision: vi.fn().mockResolvedValue("pending"),
      enrichSessionsPRBatch: mockBatchEnrichment({ reviewDecision: "pending" }),
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });
    const session = makeSession({ status: "pr_open", pr: makePR() });
    vi.mocked(mockSessionManager.get).mockResolvedValue(session);

    writeMetadata(env.sessionsDir, "app-1", {
      worktree: "/tmp",
      branch: session.branch ?? "main",
      status: session.status,
      project: "my-app",
      pr: session.pr?.url,
      runtimeHandle: session.runtimeHandle ?? undefined,
    });

    const lm = createLifecycleManager({
      config,
      registry,
      sessionManager: mockSessionManager,
    });

    await lm.check("app-1");

    expect(lm.getStates().get("app-1")).toBe("review_pending");
    expect(session.lifecycle.session.state).toBe("idle");
    expect(session.lifecycle.session.reason).toBe("awaiting_external_review");
  });

  it("skips PR auto-detection when metadata disables it", async () => {
    const mockSCM = createMockSCM({ detectPR: vi.fn().mockResolvedValue(makePR()) });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });

    writeMetadata(env.sessionsDir, "app-1", {
      worktree: "/tmp",
      branch: "feat/test",
      status: "working",
      project: "my-app",
      prAutoDetect: false,
    });

    const realSessionManager = createSessionManager({ config, registry });
    const session = await realSessionManager.get("app-1");

    expect(session).not.toBeNull();
    vi.mocked(mockSessionManager.get).mockResolvedValue(session);

    const lm = createLifecycleManager({
      config,
      registry,
      sessionManager: mockSessionManager,
    });

    await lm.check("app-1");

    expect(mockSCM.detectPR).not.toHaveBeenCalled();
    expect(lm.getStates().get("app-1")).toBe("working");
  });

  it("skips PR auto-detection for orchestrator sessions", async () => {
    const mockSCM = createMockSCM({ detectPR: vi.fn().mockResolvedValue(makePR()) });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });

    writeMetadata(env.sessionsDir, "app-1", {
      worktree: "/tmp",
      branch: "master",
      status: "working",
      project: "my-app",
      role: "orchestrator",
    });

    const realSessionManager = createSessionManager({ config, registry });
    const session = await realSessionManager.get("app-1");

    expect(session).not.toBeNull();
    vi.mocked(mockSessionManager.get).mockResolvedValue(session);

    const lm = createLifecycleManager({
      config,
      registry,
      sessionManager: mockSessionManager,
    });

    await lm.check("app-1");

    expect(mockSCM.detectPR).not.toHaveBeenCalled();
    expect(lm.getStates().get("app-1")).toBe("working");
  });

  it("skips PR auto-detection for orchestrator sessions identified by ID suffix (fallback)", async () => {
    const mockSCM = createMockSCM({ detectPR: vi.fn().mockResolvedValue(makePR()) });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });

    writeMetadata(env.sessionsDir, "app-orchestrator", {
      worktree: "/tmp",
      branch: "master",
      status: "working",
      project: "my-app",
    });

    const realSessionManager = createSessionManager({ config, registry });
    const session = await realSessionManager.get("app-orchestrator");

    expect(session).not.toBeNull();
    vi.mocked(mockSessionManager.get).mockResolvedValue(session);

    const lm = createLifecycleManager({
      config,
      registry,
      sessionManager: mockSessionManager,
    });

    await lm.check("app-orchestrator");

    expect(mockSCM.detectPR).not.toHaveBeenCalled();
    expect(lm.getStates().get("app-orchestrator")).toBe("working");
  });

  it("detects merged PR", async () => {
    vi.useFakeTimers();
    try {
      const pr = makeMatchingPR();
      const mockSCM = createMockSCM({
        getPRState: vi.fn().mockResolvedValue("merged"),
        enrichSessionsPRBatch: mockBatchEnrichment({ state: "merged", ciStatus: "none" }),
      });
      const registry = createMockRegistry({
        runtime: plugins.runtime,
        agent: plugins.agent,
        scm: mockSCM,
      });

      const lm = setupPollCheck("app-1", {
        session: makeSession({ status: "approved", pr }),
        registry,
      });

      lm.start(60_000);
      await vi.advanceTimersByTimeAsync(0);
      lm.stop();
      expect(lm.getStates().get("app-1")).toBe("merged");
    } finally {
      vi.useRealTimers();
    }
  });

  it("preserves merged PR truth in metadata instead of regressing to no-pr lifecycle state", async () => {
    vi.useFakeTimers();
    try {
      const pr = makeMatchingPR();
      const mockSCM = createMockSCM({
        getPRState: vi.fn().mockResolvedValue("merged"),
        enrichSessionsPRBatch: mockBatchEnrichment({ state: "merged", ciStatus: "none" }),
      });
      const registry = createMockRegistry({
        runtime: plugins.runtime,
        agent: plugins.agent,
        scm: mockSCM,
      });

      const lm = setupPollCheck("app-1", {
        session: makeSession({ status: "pr_open", pr }),
        registry,
      });

      lm.start(60_000);
      await vi.advanceTimersByTimeAsync(0);
      lm.stop();

      const meta = readMetadataRaw(env.sessionsDir, "app-1");
      expect(lm.getStates().get("app-1")).toBe("merged");
      expect(meta?.["status"]).toBe("merged");
      expect(meta?.["pr"]).toBe(pr.url);
      expect(meta?.["lifecycle"]).toContain('"state":"merged"');
      expect(meta?.["lifecycle"]).toContain('"reason":"merged"');
      expect(meta?.["lifecycle"]).not.toContain('"reason":"not_created"');
      expect(mockSessionManager.invalidateCache).toHaveBeenCalled();
    } finally {
      vi.useRealTimers();
    }
  });

  it("keeps closed PR sessions idle and emits a PR-closed notification", async () => {
    const mockSCM = createMockSCM({
      getPRState: vi.fn().mockResolvedValue("closed"),
      enrichSessionsPRBatch: mockBatchEnrichment({ state: "closed" }),
    });
    const notifier = createMockNotifier();
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
      notifier,
    });

    const session = makeSession({ status: "pr_open", pr: makePR() });
    const lm = setupCheck("app-1", {
      session,
      registry,
      configOverride: {
        ...config,
        notificationRouting: {
          ...config.notificationRouting,
          info: ["desktop"],
        },
      },
    });

    await lm.check("app-1");
    expect(lm.getStates().get("app-1")).toBe("idle");
    const meta = readMetadataRaw(env.sessionsDir, "app-1");
    expect(meta?.["status"]).toBe("idle");
    expect(meta?.["lifecycle"]).toContain('"state":"closed"');
    expect(meta?.["lifecycle"]).toContain('"reason":"pr_closed_waiting_decision"');
    expect(notifier.notify).toHaveBeenCalledWith(expect.objectContaining({ type: "pr.closed" }));
  });

  it("routes closed PR transitions through the pr-closed reaction key", async () => {
    const notifier = createMockNotifier();
    const mockSCM = createMockSCM({
      getPRState: vi.fn().mockResolvedValue("closed"),
      enrichSessionsPRBatch: mockBatchEnrichment({ state: "closed" }),
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
      notifier,
    });

    const session = makeSession({ status: "pr_open", pr: makePR() });
    const lm = setupCheck("app-1", {
      session,
      registry,
      configOverride: {
        ...config,
        reactions: {
          ...config.reactions,
          "pr-closed": {
            auto: true,
            action: "notify",
            priority: "action",
          },
        },
        notificationRouting: {
          ...config.notificationRouting,
          action: ["desktop"],
        },
      },
    });

    await lm.check("app-1");

    expect(notifier.notify).toHaveBeenCalledWith(
      expect.objectContaining({
        type: "reaction.triggered",
        data: expect.objectContaining({
          schemaVersion: 3,
          semanticType: "pr.closed",
          reaction: expect.objectContaining({ key: "pr-closed" }),
        }),
      }),
    );
  });

  it("detects mergeable when approved + CI green", async () => {
    const mockSCM = createMockSCM({
      getReviewDecision: vi.fn().mockResolvedValue("approved"),
      getMergeability: vi.fn().mockResolvedValue({
        mergeable: true,
        ciPassing: true,
        approved: true,
        noConflicts: true,
        blockers: [],
      }),
      enrichSessionsPRBatch: mockBatchEnrichment({ reviewDecision: "approved", mergeable: true }),
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makePR() }),
      registry,
    });

    await lm.check("app-1");
    expect(lm.getStates().get("app-1")).toBe("mergeable");
  });

  it("throws for nonexistent session", async () => {
    vi.mocked(mockSessionManager.get).mockResolvedValue(null);

    const lm = createLifecycleManager({
      config,
      registry: mockRegistry,
      sessionManager: mockSessionManager,
    });

    await expect(lm.check("nonexistent")).rejects.toThrow("not found");
  });

  it("does not change state when status is unchanged", async () => {
    const lm = setupCheck("app-1", {
      session: makeSession({ status: "working" }),
    });

    await lm.check("app-1");
    expect(lm.getStates().get("app-1")).toBe("working");

    // Second check — status remains working, no transition
    await lm.check("app-1");
    expect(lm.getStates().get("app-1")).toBe("working");
  });
});

describe("reactions", () => {
  it("fires report watcher reactions only once per active trigger", async () => {
    vi.useFakeTimers();

    const notifier = createMockNotifier();
    const registryWithNotifier = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      notifier,
    });
    const staleSession = makeSession({
      id: "app-1",
      status: "working",
      workspacePath: null,
      createdAt: new Date("2025-01-01T11:40:00.000Z"),
      metadata: {
        createdAt: "2025-01-01T11:40:00.000Z",
      },
    });

    config.reactions = {
      "report-no-acknowledge": { auto: true, action: "notify", priority: "urgent" },
    };
    vi.mocked(mockSessionManager.list).mockResolvedValue([staleSession]);

    const lm = createLifecycleManager({
      config,
      registry: registryWithNotifier,
      sessionManager: mockSessionManager,
    });

    try {
      vi.setSystemTime(new Date("2025-01-01T12:00:00.000Z"));
      lm.start(60_000);
      await vi.advanceTimersByTimeAsync(0);
      await vi.advanceTimersByTimeAsync(60_000);

      const reactionNotifications = vi.mocked(notifier.notify).mock.calls.filter((call) => {
        const event = call[0] as { type?: string; data?: Record<string, unknown> } | undefined;
        const reaction =
          event?.data?.reaction && typeof event.data.reaction === "object"
            ? (event.data.reaction as Record<string, unknown>)
            : null;
        return event?.type === "reaction.triggered" && reaction?.key === "report-no-acknowledge";
      });

      expect(reactionNotifications).toHaveLength(1);
      expect(staleSession.metadata["reportWatcherTriggerCount"]).toBe("2");
      expect(staleSession.metadata["reportWatcherActiveTrigger"]).toBe("no_acknowledge");
    } finally {
      lm.stop();
      vi.useRealTimers();
    }
  });

  it("triggers send-to-agent reaction on CI failure", async () => {
    config.reactions = {
      "ci-failed": {
        auto: true,
        action: "send-to-agent",
        message: "CI is failing. Fix it.",
        retries: 2,
        escalateAfter: 2,
      },
    };

    const mockSCM = createMockSCM({
      getCISummary: vi.fn().mockResolvedValue("failing"),
      enrichSessionsPRBatch: mockBatchEnrichment({ ciStatus: "failing" }),
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makePR() }),
      registry,
    });

    await lm.check("app-1");
    expect(mockSessionManager.send).toHaveBeenCalledWith("app-1", "CI is failing. Fix it.");
  });

  it("does not trigger reaction when auto=false", async () => {
    config.reactions = {
      "ci-failed": { auto: false, action: "send-to-agent", message: "CI is failing." },
    };

    const mockSCM = createMockSCM({
      getCISummary: vi.fn().mockResolvedValue("failing"),
      enrichSessionsPRBatch: mockBatchEnrichment({ ciStatus: "failing" }),
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makePR() }),
      registry,
    });

    await lm.check("app-1");
    expect(mockSessionManager.send).not.toHaveBeenCalled();
  });

  it("suppresses immediate notification when send-to-agent reaction handles the event", async () => {
    const notifier = createMockNotifier();
    const mockSCM = createMockSCM({
      getCISummary: vi.fn().mockResolvedValue("failing"),
      enrichSessionsPRBatch: mockBatchEnrichment({ ciStatus: "failing" }),
    });

    const registry: PluginRegistry = {
      ...mockRegistry,
      get: vi.fn().mockImplementation((slot: string, name: string) => {
        if (slot === "runtime") return plugins.runtime;
        if (slot === "agent") return plugins.agent;
        if (slot === "scm") return mockSCM;
        if (slot === "notifier" && name === "desktop") return notifier;
        return null;
      }),
    };

    vi.mocked(mockSessionManager.send).mockResolvedValue(undefined);

    const configWithReaction = {
      ...config,
      reactions: {
        "ci-failed": {
          auto: true,
          action: "send-to-agent" as const,
          message: "Fix CI",
          retries: 3,
          escalateAfter: 3,
        },
      },
    };

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makePR() }),
      registry,
      configOverride: configWithReaction,
    });

    await lm.check("app-1");

    expect(lm.getStates().get("app-1")).toBe("ci_failed");
    expect(mockSessionManager.send).toHaveBeenCalledWith("app-1", "Fix CI");
    expect(notifier.notify).not.toHaveBeenCalled();
  });

  it("dispatches unresolved review comments even when reviewDecision stays unchanged", async () => {
    config.reactions = {
      "changes-requested": {
        auto: true,
        action: "send-to-agent",
        message: "Handle review comments.",
      },
    };

    const mockSCM = createMockSCM({
      getReviewThreads: vi.fn().mockResolvedValue({
        threads: [
          {
            id: "c1",
            author: "reviewer",
            body: "Please rename this helper",
            path: "src/app.ts",
            line: 12,
            isResolved: false,
            createdAt: new Date(),
            url: "https://example.com/comment/1",
            isBot: false,
          },
        ],
        reviews: [],
      }),
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });

    vi.mocked(mockSessionManager.send).mockResolvedValue(undefined);

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makePR() }),
      registry,
    });

    await lm.check("app-1");
    expect(mockSessionManager.send).toHaveBeenCalledTimes(1);
    const sentMessage = vi.mocked(mockSessionManager.send).mock.calls[0]![1] as string;
    expect(sentMessage).toContain("src/app.ts:12");
    expect(sentMessage).toContain("@reviewer");
    expect(sentMessage).toContain("Please rename this helper");

    vi.mocked(mockSessionManager.send).mockClear();
    await lm.check("app-1");
    expect(mockSessionManager.send).not.toHaveBeenCalled();

    const metadata = readMetadataRaw(env.sessionsDir, "app-1");
    expect(metadata?.["lastPendingReviewDispatchHash"]).toBe("c1");
  });

  it("sends enriched review content on changes_requested transition alongside the generic message", async () => {
    config.reactions = {
      "changes-requested": {
        auto: true,
        action: "send-to-agent",
        message: "Handle requested changes.",
      },
    };

    const mockSCM = createMockSCM({
      getReviewDecision: vi.fn().mockResolvedValue("changes_requested"),
      enrichSessionsPRBatch: vi.fn().mockImplementation(async (prs: PRInfo[]) => {
        const result = new Map();
        for (const pr of prs) {
          result.set(`${pr.owner}/${pr.repo}#${pr.number}`, {
            state: "open",
            ciStatus: "passing",
            reviewDecision: "changes_requested",
            mergeable: false,
          });
        }
        return result;
      }),
      getReviewThreads: vi.fn().mockResolvedValue({
        threads: [
          {
            id: "c1",
            author: "reviewer",
            body: "Please add validation",
            path: "src/route.ts",
            line: 44,
            isResolved: false,
            createdAt: new Date(),
            url: "https://example.com/comment/2",
            isBot: false,
          },
        ],
        reviews: [],
      }),
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });

    vi.mocked(mockSessionManager.send).mockResolvedValue(undefined);

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makePR() }),
      registry,
    });

    await lm.check("app-1");

    // First call is the transition reaction (generic message), second is
    // the backlog dispatch with actual review comment content.
    expect(mockSessionManager.send).toHaveBeenCalledTimes(2);
    const enrichedMessage = vi.mocked(mockSessionManager.send).mock.calls[1]![1] as string;
    expect(enrichedMessage).toContain("src/route.ts:44");
    expect(enrichedMessage).toContain("@reviewer");
    expect(enrichedMessage).toContain("Please add validation");

    // Second check: throttled (within REVIEW_BACKLOG_THROTTLE_MS window) and
    // fingerprint already matches dispatch hash — neither path re-sends.
    vi.mocked(mockSessionManager.send).mockClear();
    await lm.check("app-1");
    expect(mockSessionManager.send).not.toHaveBeenCalled();
  });

  it("does not double-bill reaction attempts on changes_requested transition with retries:1", async () => {
    const notifier = createMockNotifier();

    config.reactions = {
      "changes-requested": {
        auto: true,
        action: "send-to-agent",
        message: "Handle requested changes.",
        retries: 1,
      },
    };

    const mockSCM = createMockSCM({
      getReviewDecision: vi.fn().mockResolvedValue("changes_requested"),
      enrichSessionsPRBatch: vi.fn().mockImplementation(async (prs: PRInfo[]) => {
        const result = new Map();
        for (const pr of prs) {
          result.set(`${pr.owner}/${pr.repo}#${pr.number}`, {
            state: "open",
            ciStatus: "passing",
            reviewDecision: "changes_requested",
            mergeable: false,
          });
        }
        return result;
      }),
      getReviewThreads: vi.fn().mockResolvedValue({
        threads: [
          {
            id: "c1",
            author: "reviewer",
            body: "Needs validation",
            path: "src/handler.ts",
            line: 10,
            isResolved: false,
            createdAt: new Date(),
            url: "https://example.com/comment/retries",
            isBot: false,
          },
        ],
        reviews: [],
      }),
    });

    const registry: PluginRegistry = {
      ...mockRegistry,
      get: vi.fn().mockImplementation((slot: string, name: string) => {
        if (slot === "runtime") return plugins.runtime;
        if (slot === "agent") return plugins.agent;
        if (slot === "scm") return mockSCM;
        if (slot === "notifier" && name === "desktop") return notifier;
        return null;
      }),
    };

    vi.mocked(mockSessionManager.send).mockResolvedValue(undefined);

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makePR() }),
      registry,
    });

    await lm.check("app-1");

    // Transition handler sends the generic message (attempt 1), and the backlog
    // dispatch sends the enriched message directly (no attempt increment).
    // Total sends = 2 but reaction attempts = 1, so no escalation.
    expect(mockSessionManager.send).toHaveBeenCalledTimes(2);
    expect(notifier.notify).not.toHaveBeenCalledWith(
      expect.objectContaining({ type: "reaction.escalated" }),
    );

    // The enriched message should contain the actual review content
    const enrichedMessage = vi.mocked(mockSessionManager.send).mock.calls[1]![1] as string;
    expect(enrichedMessage).toContain("src/handler.ts:10");
    expect(enrichedMessage).toContain("Needs validation");
  });

  it("routes enriched review dispatch through executeReaction when action is notify (not send-to-agent)", async () => {
    const notifier = createMockNotifier();

    config.reactions = {
      "changes-requested": {
        auto: true,
        action: "notify",
        message: "Review changes requested.",
      },
    };
    config.notificationRouting = {
      ...config.notificationRouting,
      info: ["desktop"],
    };

    const mockSCM = createMockSCM({
      getReviewDecision: vi.fn().mockResolvedValue("changes_requested"),
      enrichSessionsPRBatch: vi.fn().mockImplementation(async (prs: PRInfo[]) => {
        const result = new Map();
        for (const pr of prs) {
          result.set(`${pr.owner}/${pr.repo}#${pr.number}`, {
            state: "open",
            ciStatus: "passing",
            reviewDecision: "changes_requested",
            mergeable: false,
          });
        }
        return result;
      }),
      getReviewThreads: vi.fn().mockResolvedValue({
        threads: [
          {
            id: "c1",
            author: "reviewer",
            body: "Fix the type",
            path: "src/api.ts",
            line: 5,
            isResolved: false,
            createdAt: new Date(),
            url: "https://example.com/comment/notify",
            isBot: false,
          },
        ],
        reviews: [],
      }),
    });

    const registry: PluginRegistry = {
      ...mockRegistry,
      get: vi.fn().mockImplementation((slot: string, name: string) => {
        if (slot === "runtime") return plugins.runtime;
        if (slot === "agent") return plugins.agent;
        if (slot === "scm") return mockSCM;
        if (slot === "notifier" && name === "desktop") return notifier;
        return null;
      }),
    };

    vi.mocked(mockSessionManager.send).mockResolvedValue(undefined);

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makePR() }),
      registry,
    });

    await lm.check("app-1");

    // action: "notify" should NOT send to the agent — it routes through
    // executeReaction → notifyHuman. The bypass branch must not fire.
    expect(mockSessionManager.send).not.toHaveBeenCalled();
    expect(notifier.notify).toHaveBeenCalled();
  });

  it("dispatches detailed automated review comments when using the default sentinel message", async () => {
    config.reactions = {
      "bugbot-comments": {
        auto: true,
        action: "send-to-agent",
        // Sentinel — dispatcher replaces with formatted detail listing.
        message: DEFAULT_BUGBOT_COMMENTS_MESSAGE,
      },
    };

    const mockSCM = createMockSCM({
      getReviewThreads: vi.fn().mockResolvedValue({
        threads: [
          {
            id: "bot-1",
            author: "cursor[bot]",
            body: "Potential issue detected",
            path: "src/worker.ts",
            line: 9,
            isResolved: false,
            createdAt: new Date(),
            url: "https://example.com/comment/3",
            isBot: true,
          },
        ],
        reviews: [],
      }),
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });

    vi.mocked(mockSessionManager.send).mockResolvedValue(undefined);

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makePR() }),
      registry,
    });

    await lm.check("app-1");
    expect(mockSessionManager.send).toHaveBeenCalledTimes(1);
    const sentMessage = vi.mocked(mockSessionManager.send).mock.calls[0]![1] as string;
    expect(sentMessage).toContain("src/worker.ts:9");
    expect(sentMessage).toContain("@cursor[bot]");
    expect(sentMessage).toContain("Potential issue detected");

    vi.mocked(mockSessionManager.send).mockClear();
    await lm.check("app-1");
    expect(mockSessionManager.send).not.toHaveBeenCalled();

    const metadata = readMetadataRaw(env.sessionsDir, "app-1");
    expect(metadata?.["lastAutomatedReviewDispatchHash"]).toBe("bot-1");
  });

  it("respects a user-customized bugbot-comments message (no silent override)", async () => {
    // The review backlog dispatch always formats bot comments inline so the
    // agent has the data without re-fetching.  A custom config message is
    // overridden by the formatted detail listing.
    config.reactions = {
      "bugbot-comments": {
        auto: true,
        action: "send-to-agent",
        message: "Custom internal playbook. Follow ORG-1234.",
      },
    };

    const mockSCM = createMockSCM({
      getReviewThreads: vi.fn().mockResolvedValue({
        threads: [
          {
            id: "bot-1",
            author: "cursor[bot]",
            body: "Potential issue detected",
            path: "src/worker.ts",
            line: 9,
            isResolved: false,
            createdAt: new Date(),
            url: "https://example.com/comment/3",
            isBot: true,
          },
        ],
        reviews: [],
      }),
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });

    vi.mocked(mockSessionManager.send).mockResolvedValue(undefined);

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makePR() }),
      registry,
    });

    await lm.check("app-1");
    expect(mockSessionManager.send).toHaveBeenCalledTimes(1);
    const sentMessage = vi.mocked(mockSessionManager.send).mock.calls[0]![1] as string;
    expect(sentMessage).toContain("src/worker.ts:9");
    expect(sentMessage).toContain("@cursor[bot]");
    expect(sentMessage).toContain("Potential issue detected");
  });

  it("dispatches CI failure summary with failed step and log tail", async () => {
    config.reactions = {
      "ci-failed": {
        auto: true,
        action: "send-to-agent",
        retries: 3,
        escalateAfter: 3,
      },
    };

    const ciChecks = [
      {
        name: "build",
        status: "failed",
        url: "https://github.com/org/repo/actions/runs/123/job/456",
        conclusion: "FAILURE",
      },
    ];
    const mockSCM = createMockSCM({
      getCISummary: vi.fn().mockResolvedValue("failing"),
      getCIFailureSummary: vi.fn().mockResolvedValue({
        failedJobs: [
          {
            name: "build",
            failedStep: "Run pnpm test",
            runUrl: "https://github.com/org/repo/actions/runs/123/job/456",
            logTail:
              "AssertionError: expected true to be false\n```\nProcess completed with exit code 1",
          },
        ],
      }),
      enrichSessionsPRBatch: mockBatchEnrichment({ ciStatus: "failing", ciChecks }),
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });

    vi.mocked(mockSessionManager.send).mockResolvedValue(undefined);

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makePR() }),
      registry,
    });

    await lm.check("app-1");
    expect(mockSessionManager.send).toHaveBeenCalledTimes(1);
    const sentMessage = vi.mocked(mockSessionManager.send).mock.calls[0]![1];
    expect(sentMessage).toContain("CI is failing on your PR.");
    expect(sentMessage).toContain("Failed: build → Run pnpm test");
    expect(sentMessage).toContain("Failure URL: https://github.com/org/repo/actions/runs/123/job/456");
    expect(sentMessage).toContain("Log tail (last 3 lines):");
    expect(sentMessage).toContain("AssertionError: expected true to be false");
    expect(sentMessage).toContain("\u200B```");
    expect(sentMessage).toContain("Fix the issues and push again.");
    expect(mockSCM.getCIFailureSummary).toHaveBeenCalledWith(makePR(), ciChecks);
  });

  it("falls back to check names and URLs when SCM lacks getCIFailureSummary", async () => {
    config.reactions = {
      "ci-failed": {
        auto: true,
        action: "send-to-agent",
        message: "CI is failing. Fix it.",
        retries: 3,
        escalateAfter: 3,
      },
    };

    const ciChecks = [
      {
        name: "lint",
        status: "failed",
        url: "https://github.com/org/repo/actions/runs/123",
        conclusion: "FAILURE",
      },
      {
        name: "test",
        status: "passed",
        url: "https://github.com/org/repo/actions/runs/124",
        conclusion: "SUCCESS",
      },
      {
        name: "typecheck",
        status: "failed",
        url: "https://github.com/org/repo/actions/runs/125",
        conclusion: "FAILURE",
      },
    ];
    const mockSCM = createMockSCM({
      getCISummary: vi.fn().mockResolvedValue("failing"),
      getCIChecks: vi.fn().mockResolvedValue(ciChecks),
      enrichSessionsPRBatch: mockBatchEnrichment({ ciStatus: "failing", ciChecks }),
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });

    vi.mocked(mockSessionManager.send).mockResolvedValue(undefined);

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makePR() }),
      registry,
    });

    // First check: transition to ci_failed — sends detailed CI info directly
    await lm.check("app-1");
    expect(lm.getStates().get("app-1")).toBe("ci_failed");
    expect(mockSessionManager.send).toHaveBeenCalledTimes(1);
    const sentMessage = vi.mocked(mockSessionManager.send).mock.calls[0]![1];
    expect(sentMessage).toContain("CI checks are failing on your PR.");
    expect(sentMessage).toContain("lint");
    expect(sentMessage).toContain("typecheck");
    expect(sentMessage).toContain("https://github.com/org/repo/actions/runs/123");
    expect(sentMessage).toContain("https://github.com/org/repo/actions/runs/125");
    // Should NOT include the passing check
    expect(sentMessage).not.toContain("runs/124");
  });

  it("does not re-send CI failure details on subsequent polls (transition fires once)", async () => {
    config.reactions = {
      "ci-failed": {
        auto: true,
        action: "send-to-agent",
        message: "CI is failing.",
        retries: 3,
        escalateAfter: 3,
      },
    };

    const ciChecks = [{ name: "lint", status: "failed", conclusion: "FAILURE" }];
    const mockSCM = createMockSCM({
      getCISummary: vi.fn().mockResolvedValue("failing"),
      getCIChecks: vi.fn().mockResolvedValue(ciChecks),
      enrichSessionsPRBatch: mockBatchEnrichment({ ciStatus: "failing", ciChecks }),
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });

    vi.mocked(mockSessionManager.send).mockResolvedValue(undefined);

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makePR() }),
      registry,
    });

    // First check: transition to ci_failed — sends detailed CI info
    await lm.check("app-1");
    expect(mockSessionManager.send).toHaveBeenCalledTimes(1);

    vi.mocked(mockSessionManager.send).mockClear();

    // Second check: still ci_failed, same failures — no transition, no message
    await lm.check("app-1");
    expect(mockSessionManager.send).not.toHaveBeenCalled();
  });

  it("uses notify action for merge conflicts when configured", async () => {
    const notifier = createMockNotifier();

    const configWithNotify = {
      ...config,
      reactions: {
        "merge-conflicts": {
          auto: true,
          action: "notify" as const,
        },
      },
      notificationRouting: {
        ...config.notificationRouting,
        warning: ["desktop"],
        info: ["desktop"],
      },
    };

    const mockSCM = createMockSCM({
      getMergeability: vi.fn().mockResolvedValue({
        mergeable: false,
        ciPassing: true,
        approved: false,
        noConflicts: false,
        blockers: ["Merge conflicts"],
      }),
      enrichSessionsPRBatch: mockBatchEnrichment({ hasConflicts: true }),
    });

    const registry: PluginRegistry = {
      ...mockRegistry,
      get: vi.fn().mockImplementation((slot: string, name: string) => {
        if (slot === "runtime") return plugins.runtime;
        if (slot === "agent") return plugins.agent;
        if (slot === "scm") return mockSCM;
        if (slot === "notifier" && name === "desktop") return notifier;
        return null;
      }),
    };

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makePR() }),
      registry,
      configOverride: configWithNotify,
    });

    await lm.check("app-1");
    expect(notifier.notify).toHaveBeenCalled();
    expect(mockSessionManager.send).not.toHaveBeenCalled();
  });

  it("dispatches merge conflict notification when PR has conflicts", async () => {
    config.reactions = {
      "merge-conflicts": {
        auto: true,
        action: "send-to-agent",
        message: "Your branch has merge conflicts. Rebase and resolve them.",
      },
    };

    const mockSCM = createMockSCM({
      getMergeability: vi.fn().mockResolvedValue({
        mergeable: false,
        ciPassing: true,
        approved: false,
        noConflicts: false,
        blockers: ["Merge conflicts"],
      }),
      enrichSessionsPRBatch: mockBatchEnrichment({ hasConflicts: true }),
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });

    vi.mocked(mockSessionManager.send).mockResolvedValue(undefined);

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makePR() }),
      registry,
    });

    await lm.check("app-1");
    expect(mockSessionManager.send).toHaveBeenCalledWith(
      "app-1",
      "Your branch has merge conflicts. Rebase and resolve them.",
    );
  });

  it("does not re-dispatch merge conflict notification when already dispatched", async () => {
    config.reactions = {
      "merge-conflicts": {
        auto: true,
        action: "send-to-agent",
        message: "Resolve merge conflicts.",
      },
    };

    const mockSCM = createMockSCM({
      getMergeability: vi.fn().mockResolvedValue({
        mergeable: false,
        ciPassing: true,
        approved: false,
        noConflicts: false,
        blockers: ["Merge conflicts"],
      }),
      enrichSessionsPRBatch: mockBatchEnrichment({ hasConflicts: true }),
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });

    vi.mocked(mockSessionManager.send).mockResolvedValue(undefined);

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makePR() }),
      registry,
    });

    await lm.check("app-1");
    expect(mockSessionManager.send).toHaveBeenCalledTimes(1);

    vi.mocked(mockSessionManager.send).mockClear();

    // Second check — same conflicts, should not re-send
    await lm.check("app-1");
    expect(mockSessionManager.send).not.toHaveBeenCalled();
  });

  it("re-dispatches merge conflict notification after conflicts are resolved and recur", async () => {
    config.reactions = {
      "merge-conflicts": {
        auto: true,
        action: "send-to-agent",
        message: "Resolve merge conflicts.",
      },
    };

    const getMergeabilityMock = vi.fn().mockResolvedValue({
      mergeable: false,
      ciPassing: true,
      approved: false,
      noConflicts: false,
      blockers: ["Merge conflicts"],
    });
    const mockSCM = createMockSCM({
      getMergeability: getMergeabilityMock,
      enrichSessionsPRBatch: mockBatchEnrichment({ hasConflicts: true }),
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });

    vi.mocked(mockSessionManager.send).mockResolvedValue(undefined);

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makePR() }),
      registry,
    });

    // First: conflicts detected, notification sent
    await lm.check("app-1");
    expect(mockSessionManager.send).toHaveBeenCalledTimes(1);
    vi.mocked(mockSessionManager.send).mockClear();

    // Second: conflicts resolved
    getMergeabilityMock.mockResolvedValue({
      mergeable: true,
      ciPassing: true,
      approved: false,
      noConflicts: true,
      blockers: [],
    });
    vi.mocked(mockSCM.enrichSessionsPRBatch!).mockImplementation(
      mockBatchEnrichment({ hasConflicts: false }),
    );
    await lm.check("app-1");
    expect(mockSessionManager.send).not.toHaveBeenCalled();

    const metadata = readMetadataRaw(env.sessionsDir, "app-1");
    expect(metadata?.["lastMergeConflictDispatched"]).toBeFalsy();

    // Third: conflicts recur — should re-dispatch
    getMergeabilityMock.mockResolvedValue({
      mergeable: false,
      ciPassing: true,
      approved: false,
      noConflicts: false,
      blockers: ["Merge conflicts"],
    });
    vi.mocked(mockSCM.enrichSessionsPRBatch!).mockImplementation(
      mockBatchEnrichment({ hasConflicts: true }),
    );
    await lm.check("app-1");
    expect(mockSessionManager.send).toHaveBeenCalledTimes(1);
  });

  it("clears merge conflict tracking when PR is merged", async () => {
    config.reactions = {
      "merge-conflicts": {
        auto: true,
        action: "send-to-agent",
        message: "Resolve merge conflicts.",
      },
    };

    const mockSCM = createMockSCM({
      getMergeability: vi.fn().mockResolvedValue({
        mergeable: false,
        ciPassing: true,
        approved: false,
        noConflicts: false,
        blockers: ["Merge conflicts"],
      }),
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });

    vi.mocked(mockSessionManager.send).mockResolvedValue(undefined);

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makePR() }),
      registry,
    });

    await lm.check("app-1");

    // Now PR is merged
    vi.mocked(mockSCM.getPRState).mockResolvedValue("merged");

    await lm.check("app-1");

    const metadata = readMetadataRaw(env.sessionsDir, "app-1");
    expect(metadata?.["lastMergeConflictDispatched"]).toBeFalsy();
  });

  it("clears merge conflict tracking when PR is closed", async () => {
    config.reactions = {
      "merge-conflicts": {
        auto: true,
        action: "send-to-agent",
        message: "Resolve merge conflicts.",
      },
    };

    const getMergeability = vi.fn();
    const mockSCM = createMockSCM({
      getPRState: vi.fn().mockResolvedValue("closed"),
      getMergeability,
      enrichSessionsPRBatch: mockBatchEnrichment({ state: "closed" }),
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });

    const lm = setupCheck("app-1", {
      session: makeSession({
        status: "pr_open",
        pr: makePR(),
        metadata: { lastMergeConflictDispatched: "true" },
      }),
      registry,
    });

    await lm.check("app-1");

    const metadata = readMetadataRaw(env.sessionsDir, "app-1");
    expect(metadata?.["lastMergeConflictDispatched"]).toBeFalsy();
    expect(getMergeability).not.toHaveBeenCalled();
  });

  it("notifies humans on significant transitions without reaction config", async () => {
    const notifier = createMockNotifier();
    const mockSCM = createMockSCM({
      getPRState: vi.fn().mockResolvedValue("merged"),
      enrichSessionsPRBatch: mockBatchEnrichment({ state: "merged" }),
    });

    const registry: PluginRegistry = {
      ...mockRegistry,
      get: vi.fn().mockImplementation((slot: string, name: string) => {
        if (slot === "runtime") return plugins.runtime;
        if (slot === "agent") return plugins.agent;
        if (slot === "scm") return mockSCM;
        if (slot === "notifier" && name === "desktop") return notifier;
        return null;
      }),
    };

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "approved", pr: makePR() }),
      registry,
    });

    await lm.check("app-1");

    expect(lm.getStates().get("app-1")).toBe("merged");
    expect(notifier.notify).toHaveBeenCalled();
    expect(notifier.notify).toHaveBeenCalledWith(
      expect.objectContaining({ type: "merge.completed" }),
    );

    const summary = readObservabilitySummary(config);
    expect(summary.projects["my-app"]?.metrics["notification_delivery"]?.success).toBe(1);
    expect(
      summary.projects["my-app"]?.recentTraces.some(
        (trace) =>
          trace.operation === "notification.deliver" &&
          trace.outcome === "success" &&
          trace.data?.["targetReference"] === "desktop",
      ),
    ).toBe(true);
  });

  it("fires the spawn-session reaction on the real merge transition (merge.completed)", async () => {
    // Finding 2 (#10): `merge.completed` must map to a reaction key so a
    // `spawn-session` reaction is reachable when the PR actually merges —
    // previously there was no mapping, so the reaction never ran.
    const mockSCM = createMockSCM({
      getPRState: vi.fn().mockResolvedValue("merged"),
      enrichSessionsPRBatch: mockBatchEnrichment({ state: "merged" }),
    });
    const registry: PluginRegistry = {
      ...mockRegistry,
      get: vi.fn().mockImplementation((slot: string, name: string) => {
        if (slot === "runtime") return plugins.runtime;
        if (slot === "agent") return plugins.agent;
        if (slot === "scm") return mockSCM;
        return name === "desktop" ? createMockNotifier() : null;
      }),
    };
    const configOverride: OrchestratorConfig = {
      ...config,
      reactions: {
        ...config.reactions,
        "pr-merged": { auto: true, action: "spawn-session" },
      },
    };
    // The reaction triggers an unscoped scheduler pass over the session list.
    vi.mocked(mockSessionManager.list).mockResolvedValue([]);

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "approved", pr: makePR() }),
      registry,
      configOverride,
    });

    await lm.check("app-1");

    expect(lm.getStates().get("app-1")).toBe("merged");
    expect(recordActivityEvent).toHaveBeenCalledWith(
      expect.objectContaining({
        kind: "reaction.action_succeeded",
        data: expect.objectContaining({ action: "spawn-session" }),
      }),
    );
  });

  it("records notifier delivery failures without interrupting lifecycle transitions", async () => {
    const notifier = createMockNotifier();
    vi.mocked(notifier.notify).mockRejectedValue(new Error("webhook failed"));
    const mockSCM = createMockSCM({
      getPRState: vi.fn().mockResolvedValue("merged"),
      enrichSessionsPRBatch: mockBatchEnrichment({ state: "merged" }),
    });

    const registry: PluginRegistry = {
      ...mockRegistry,
      get: vi.fn().mockImplementation((slot: string, name: string) => {
        if (slot === "runtime") return plugins.runtime;
        if (slot === "agent") return plugins.agent;
        if (slot === "scm") return mockSCM;
        if (slot === "notifier" && name === "desktop") return notifier;
        return null;
      }),
    };

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "approved", pr: makePR() }),
      registry,
    });

    await lm.check("app-1");

    expect(lm.getStates().get("app-1")).toBe("merged");
    expect(recordActivityEvent).toHaveBeenCalledWith(
      expect.objectContaining({
        source: "notifier",
        kind: "notification.delivery_failed",
        level: "warn",
        data: expect.objectContaining({
          eventType: "merge.completed",
          targetReference: "desktop",
          targetPlugin: "desktop",
        }),
      }),
    );

    const summary = readObservabilitySummary(config);
    expect(summary.projects["my-app"]?.metrics["notification_delivery"]?.failure).toBe(1);
    expect(summary.projects["my-app"]?.health["notification.delivery.desktop"]?.status).toBe(
      "warn",
    );
  });

  it("records missing notifier targets as delivery failures", async () => {
    const mockSCM = createMockSCM({
      getPRState: vi.fn().mockResolvedValue("merged"),
      enrichSessionsPRBatch: mockBatchEnrichment({ state: "merged" }),
    });
    const configWithMissingNotifier: OrchestratorConfig = {
      ...config,
      notificationRouting: {
        ...config.notificationRouting,
        action: ["missing"],
      },
    };

    const registry: PluginRegistry = {
      ...mockRegistry,
      get: vi.fn().mockImplementation((slot: string) => {
        if (slot === "runtime") return plugins.runtime;
        if (slot === "agent") return plugins.agent;
        if (slot === "scm") return mockSCM;
        return null;
      }),
    };

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "approved", pr: makePR() }),
      registry,
      configOverride: configWithMissingNotifier,
    });

    await lm.check("app-1");

    expect(recordActivityEvent).toHaveBeenCalledWith(
      expect.objectContaining({
        source: "notifier",
        kind: "notification.target_missing",
        level: "warn",
        data: expect.objectContaining({
          eventType: "merge.completed",
          targetReference: "missing",
          targetPlugin: "missing",
        }),
      }),
    );

    const summary = readObservabilitySummary(configWithMissingNotifier);
    expect(summary.projects["my-app"]?.metrics["notification_delivery"]?.failure).toBe(1);
  });

  it("resolves notifier aliases from notificationRouting before dispatch", async () => {
    const notifier = createMockNotifier();
    const mockSCM = createMockSCM({
      getPRState: vi.fn().mockResolvedValue("merged"),
      enrichSessionsPRBatch: mockBatchEnrichment({ state: "merged" }),
    });

    const configWithAliasRouting: OrchestratorConfig = {
      ...config,
      notifiers: {
        alerts: {
          plugin: "desktop",
        },
      },
      notificationRouting: {
        ...config.notificationRouting,
        action: ["alerts"],
      },
    };

    const registry: PluginRegistry = {
      ...mockRegistry,
      get: vi.fn().mockImplementation((slot: string, name: string) => {
        if (slot === "runtime") return plugins.runtime;
        if (slot === "agent") return plugins.agent;
        if (slot === "scm") return mockSCM;
        if (slot === "notifier" && name === "desktop") return notifier;
        return null;
      }),
    };

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "approved", pr: makePR() }),
      registry,
      configOverride: configWithAliasRouting,
    });

    await lm.check("app-1");

    expect(notifier.notify).toHaveBeenCalledWith(
      expect.objectContaining({ type: "merge.completed" }),
    );
  });

  it("resolves notifier aliases from defaults.notifiers when routing falls back", async () => {
    const notifier = createMockNotifier();
    const mockSCM = createMockSCM({
      getPRState: vi.fn().mockResolvedValue("merged"),
      enrichSessionsPRBatch: mockBatchEnrichment({ state: "merged" }),
    });

    const configWithAliasDefaults: OrchestratorConfig = {
      ...config,
      defaults: {
        ...config.defaults,
        notifiers: ["alerts"],
      },
      notifiers: {
        alerts: {
          plugin: "desktop",
        },
      },
      notificationRouting: {
        urgent: ["desktop"],
        warning: ["desktop"],
        info: ["desktop"],
      } as OrchestratorConfig["notificationRouting"],
    };

    const registry: PluginRegistry = {
      ...mockRegistry,
      get: vi.fn().mockImplementation((slot: string, name: string) => {
        if (slot === "runtime") return plugins.runtime;
        if (slot === "agent") return plugins.agent;
        if (slot === "scm") return mockSCM;
        if (slot === "notifier" && name === "desktop") return notifier;
        return null;
      }),
    };

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "approved", pr: makePR() }),
      registry,
      configOverride: configWithAliasDefaults,
    });

    await lm.check("app-1");

    expect(notifier.notify).toHaveBeenCalledWith(
      expect.objectContaining({ type: "merge.completed" }),
    );
  });

  it("prefers alias-specific notifier instances over shared plugin instances", async () => {
    const alertsNotifier = createMockNotifier();
    const opsNotifier = createMockNotifier();
    const mockSCM = createMockSCM({
      getPRState: vi.fn().mockResolvedValue("merged"),
      enrichSessionsPRBatch: mockBatchEnrichment({ state: "merged" }),
    });

    const configWithSharedPluginAliases: OrchestratorConfig = {
      ...config,
      notifiers: {
        alerts: {
          plugin: "desktop",
        },
        ops: {
          plugin: "desktop",
        },
      },
      notificationRouting: {
        ...config.notificationRouting,
        action: ["ops"],
      },
    };

    const registry: PluginRegistry = {
      ...mockRegistry,
      get: vi.fn().mockImplementation((slot: string, name: string) => {
        if (slot === "runtime") return plugins.runtime;
        if (slot === "agent") return plugins.agent;
        if (slot === "scm") return mockSCM;
        if (slot === "notifier" && name === "ops") return opsNotifier;
        if (slot === "notifier" && name === "desktop") return alertsNotifier;
        return null;
      }),
    };

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "approved", pr: makePR() }),
      registry,
      configOverride: configWithSharedPluginAliases,
    });

    await lm.check("app-1");

    expect(opsNotifier.notify).toHaveBeenCalledWith(
      expect.objectContaining({ type: "merge.completed" }),
    );
    expect(alertsNotifier.notify).not.toHaveBeenCalled();
  });

  it("CI failure tracker survives status oscillation and escalates after retries", async () => {
    const notifier = createMockNotifier();

    config.reactions = {
      "ci-failed": {
        auto: true,
        action: "send-to-agent",
        message: "CI is failing. Fix it.",
        retries: 2,
        escalateAfter: 2,
      },
    };

    const batchMock = mockBatchEnrichment({ ciStatus: "failing" });
    const mockSCM = createMockSCM({
      enrichSessionsPRBatch: batchMock,
    });

    const registry: PluginRegistry = {
      ...mockRegistry,
      get: vi.fn().mockImplementation((slot: string, name: string) => {
        if (slot === "runtime") return plugins.runtime;
        if (slot === "agent") return plugins.agent;
        if (slot === "scm") return mockSCM;
        if (slot === "notifier" && name === "desktop") return notifier;
        return null;
      }),
    };

    vi.mocked(mockSessionManager.send).mockResolvedValue(undefined);

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makePR() }),
      registry,
    });

    // Oscillation 1: pr_open → ci_failed (attempt 1 — send to agent)
    await lm.check("app-1");
    expect(lm.getStates().get("app-1")).toBe("ci_failed");
    expect(mockSessionManager.send).toHaveBeenCalledTimes(1);
    vi.mocked(mockSessionManager.send).mockClear();

    // CI starts passing → ci_failed → pr_open (tracker survives)
    vi.mocked(mockSCM.enrichSessionsPRBatch!).mockImplementation(
      mockBatchEnrichment({ ciStatus: "passing" }),
    );
    await lm.check("app-1");
    expect(lm.getStates().get("app-1")).toBe("pr_open");
    expect(mockSessionManager.send).not.toHaveBeenCalled();

    // Oscillation 2: pr_open → ci_failed (attempt 2 — send to agent)
    vi.mocked(mockSCM.enrichSessionsPRBatch!).mockImplementation(
      mockBatchEnrichment({ ciStatus: "failing" }),
    );
    await lm.check("app-1");
    expect(lm.getStates().get("app-1")).toBe("ci_failed");
    expect(mockSessionManager.send).toHaveBeenCalledTimes(1);
    vi.mocked(mockSessionManager.send).mockClear();

    // CI passes again
    vi.mocked(mockSCM.enrichSessionsPRBatch!).mockImplementation(
      mockBatchEnrichment({ ciStatus: "passing" }),
    );
    await lm.check("app-1");

    // Oscillation 3: pr_open → ci_failed (attempt 3 > retries:2 — escalate)
    vi.mocked(mockSCM.enrichSessionsPRBatch!).mockImplementation(
      mockBatchEnrichment({ ciStatus: "failing" }),
    );
    vi.mocked(notifier.notify).mockClear();
    await lm.check("app-1");

    // Should NOT send to agent — should escalate to human
    expect(mockSessionManager.send).not.toHaveBeenCalled();
    expect(notifier.notify).toHaveBeenCalledWith(
      expect.objectContaining({ type: "reaction.escalated" }),
    );

    // After escalation, tracker is marked escalated — needs 2 stable passing polls to clear
    vi.mocked(mockSCM.enrichSessionsPRBatch!).mockImplementation(
      mockBatchEnrichment({ ciStatus: "passing" }),
    );
    await lm.check("app-1"); // stableCount = 1
    await lm.check("app-1"); // stableCount = 2 → clearReactionTracker
    vi.mocked(mockSessionManager.send).mockClear();
    vi.mocked(notifier.notify).mockClear();

    vi.mocked(mockSCM.enrichSessionsPRBatch!).mockImplementation(
      mockBatchEnrichment({ ciStatus: "failing" }),
    );
    await lm.check("app-1");

    // Fresh budget — sends to agent (attempt 1 again), not escalate
    expect(mockSessionManager.send).toHaveBeenCalledTimes(1);
    expect(notifier.notify).not.toHaveBeenCalledWith(
      expect.objectContaining({ type: "reaction.escalated" }),
    );
  });

  it("merge conflict tracker resets on resolve — recurrence gets fresh budget", async () => {
    const notifier = createMockNotifier();

    config.reactions = {
      "merge-conflicts": {
        auto: true,
        action: "send-to-agent",
        message: "Resolve merge conflicts.",
        retries: 1,
        escalateAfter: 1,
      },
    };

    const batchMock = mockBatchEnrichment({ hasConflicts: true });
    const mockSCM = createMockSCM({
      enrichSessionsPRBatch: batchMock,
    });

    const registry: PluginRegistry = {
      ...mockRegistry,
      get: vi.fn().mockImplementation((slot: string, name: string) => {
        if (slot === "runtime") return plugins.runtime;
        if (slot === "agent") return plugins.agent;
        if (slot === "scm") return mockSCM;
        if (slot === "notifier" && name === "desktop") return notifier;
        return null;
      }),
    };

    vi.mocked(mockSessionManager.send).mockResolvedValue(undefined);

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makePR() }),
      registry,
    });

    // First conflict — dispatched to agent (attempt 1)
    await lm.check("app-1");
    expect(mockSessionManager.send).toHaveBeenCalledTimes(1);
    vi.mocked(mockSessionManager.send).mockClear();

    // Conflicts resolve — tracker clears (incident boundary)
    vi.mocked(mockSCM.enrichSessionsPRBatch!).mockImplementation(
      mockBatchEnrichment({ hasConflicts: false }),
    );
    await lm.check("app-1");
    const metadata = readMetadataRaw(env.sessionsDir, "app-1");
    expect(metadata?.["lastMergeConflictDispatched"]).toBeFalsy();

    // Conflicts recur — fresh tracker (attempt 1 again, not 2)
    vi.mocked(mockSCM.enrichSessionsPRBatch!).mockImplementation(
      mockBatchEnrichment({ hasConflicts: true }),
    );
    vi.mocked(notifier.notify).mockClear();
    await lm.check("app-1");

    // Fresh budget — sends to agent (attempt 1), not escalate
    expect(mockSessionManager.send).toHaveBeenCalledTimes(1);
    expect(notifier.notify).not.toHaveBeenCalledWith(
      expect.objectContaining({ type: "reaction.escalated" }),
    );
  });

  it("non-persistent reaction keys still clear on status exit", async () => {
    config.reactions = {
      "changes-requested": {
        auto: true,
        action: "send-to-agent",
        message: "Address review comments.",
        retries: 1,
        escalateAfter: 1,
      },
    };

    const batchMock = mockBatchEnrichment({ reviewDecision: "changes_requested" });
    const mockSCM = createMockSCM({
      enrichSessionsPRBatch: batchMock,
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });

    vi.mocked(mockSessionManager.send).mockResolvedValue(undefined);

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makePR() }),
      registry,
    });

    // Transition to changes_requested (attempt 1 — send to agent)
    await lm.check("app-1");
    expect(lm.getStates().get("app-1")).toBe("changes_requested");
    expect(mockSessionManager.send).toHaveBeenCalledTimes(1);
    vi.mocked(mockSessionManager.send).mockClear();

    // Transition away — tracker clears (non-persistent key)
    vi.mocked(mockSCM.enrichSessionsPRBatch!).mockImplementation(
      mockBatchEnrichment({ ciStatus: "passing", reviewDecision: "none" }),
    );
    await lm.check("app-1");

    // Transition back — fresh tracker (attempt 1 again, NOT 2)
    vi.mocked(mockSCM.enrichSessionsPRBatch!).mockImplementation(
      mockBatchEnrichment({ reviewDecision: "changes_requested" }),
    );
    await lm.check("app-1");
    expect(mockSessionManager.send).toHaveBeenCalledTimes(1);

    // Transition away and back again — still attempt 1, not escalating
    vi.mocked(mockSCM.enrichSessionsPRBatch!).mockImplementation(
      mockBatchEnrichment({ ciStatus: "passing", reviewDecision: "none" }),
    );
    await lm.check("app-1");
    vi.mocked(mockSessionManager.send).mockClear();

    vi.mocked(mockSCM.enrichSessionsPRBatch!).mockImplementation(
      mockBatchEnrichment({ reviewDecision: "changes_requested" }),
    );
    await lm.check("app-1");
    // With retries:1, attempt 2 would escalate. But tracker was cleared,
    // so this is attempt 1 again — still sends to agent.
    expect(mockSessionManager.send).toHaveBeenCalledTimes(1);
  });

  it("CI escalation silences further dispatches — clears only after stable CI pass", async () => {
    // retries:1 → attempt 1 sends, attempt 2 escalates.
    // After escalation: tracker.escalated=true silences subsequent oscillations.
    // Tracker clears only after 2 consecutive passing polls; then next failure gets fresh budget.
    const notifier = createMockNotifier();

    config.reactions = {
      "ci-failed": {
        auto: true,
        action: "send-to-agent",
        message: "CI is failing. Fix it.",
        retries: 1,
        escalateAfter: 1,
      },
    };

    const batchMock = mockBatchEnrichment({ ciStatus: "failing" });
    const mockSCM = createMockSCM({
      enrichSessionsPRBatch: batchMock,
    });

    const registry: PluginRegistry = {
      ...mockRegistry,
      get: vi.fn().mockImplementation((slot: string, name: string) => {
        if (slot === "runtime") return plugins.runtime;
        if (slot === "agent") return plugins.agent;
        if (slot === "scm") return mockSCM;
        if (slot === "notifier" && name === "desktop") return notifier;
        return null;
      }),
    };

    vi.mocked(mockSessionManager.send).mockResolvedValue(undefined);

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makePR() }),
      registry,
    });

    // Oscillation 1: pr_open → ci_failed (attempt 1 — send to agent)
    await lm.check("app-1");
    expect(mockSessionManager.send).toHaveBeenCalledTimes(1);
    vi.mocked(mockSessionManager.send).mockClear();

    // CI passes briefly (ci_failed → pr_open, stableCount = 1)
    vi.mocked(mockSCM.enrichSessionsPRBatch!).mockImplementation(
      mockBatchEnrichment({ ciStatus: "passing" }),
    );
    await lm.check("app-1");

    // Oscillation 2: pr_open → ci_failed (attempt 2 > retries:1 — escalate, tracker.escalated = true)
    vi.mocked(mockSCM.enrichSessionsPRBatch!).mockImplementation(
      mockBatchEnrichment({ ciStatus: "failing" }),
    );
    await lm.check("app-1");
    expect(mockSessionManager.send).not.toHaveBeenCalled();
    expect(notifier.notify).toHaveBeenCalledWith(
      expect.objectContaining({ type: "reaction.escalated" }),
    );
    vi.mocked(notifier.notify).mockClear();

    // CI passes once (stableCount = 1 — not enough to clear yet)
    vi.mocked(mockSCM.enrichSessionsPRBatch!).mockImplementation(
      mockBatchEnrichment({ ciStatus: "passing" }),
    );
    await lm.check("app-1");

    // Oscillation 3: pr_open → ci_failed — escalated tracker short-circuits, NO dispatch
    vi.mocked(mockSCM.enrichSessionsPRBatch!).mockImplementation(
      mockBatchEnrichment({ ciStatus: "failing" }),
    );
    await lm.check("app-1");
    expect(mockSessionManager.send).not.toHaveBeenCalled();
    expect(notifier.notify).not.toHaveBeenCalledWith(
      expect.objectContaining({ type: "reaction.escalated" }),
    );
    vi.mocked(mockSessionManager.send).mockClear();
    vi.mocked(notifier.notify).mockClear();

    // CI passes twice stably (stableCount → 1 → 2 → tracker cleared)
    vi.mocked(mockSCM.enrichSessionsPRBatch!).mockImplementation(
      mockBatchEnrichment({ ciStatus: "passing" }),
    );
    await lm.check("app-1"); // stableCount = 1
    await lm.check("app-1"); // stableCount = 2 → clearReactionTracker

    // Oscillation 4: pr_open → ci_failed — fresh budget: attempt 1, sends (not escalate)
    vi.mocked(mockSCM.enrichSessionsPRBatch!).mockImplementation(
      mockBatchEnrichment({ ciStatus: "failing" }),
    );
    await lm.check("app-1");
    expect(mockSessionManager.send).toHaveBeenCalledTimes(1);
    expect(notifier.notify).not.toHaveBeenCalledWith(
      expect.objectContaining({ type: "reaction.escalated" }),
    );
  });

  it("single passing poll does not reset escalated ci-failed tracker", async () => {
    // Regression: one passing poll must NOT clear the tracker. Requires 2 consecutive passing polls.
    const notifier = createMockNotifier();

    config.reactions = {
      "ci-failed": {
        auto: true,
        action: "send-to-agent",
        message: "CI is failing.",
        retries: 1,
        escalateAfter: 1,
      },
    };

    const batchMock = mockBatchEnrichment({ ciStatus: "failing" });
    const mockSCM = createMockSCM({ enrichSessionsPRBatch: batchMock });

    const registry: PluginRegistry = {
      ...mockRegistry,
      get: vi.fn().mockImplementation((slot: string, name: string) => {
        if (slot === "runtime") return plugins.runtime;
        if (slot === "agent") return plugins.agent;
        if (slot === "scm") return mockSCM;
        if (slot === "notifier" && name === "desktop") return notifier;
        return null;
      }),
    };

    vi.mocked(mockSessionManager.send).mockResolvedValue(undefined);

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makePR() }),
      registry,
    });

    // Reach escalated state: attempt 1 → send, attempt 2 → escalate
    await lm.check("app-1"); // pr_open → ci_failed: attempt 1, send
    vi.mocked(mockSessionManager.send).mockClear();
    vi.mocked(mockSCM.enrichSessionsPRBatch!).mockImplementation(
      mockBatchEnrichment({ ciStatus: "passing" }),
    );
    await lm.check("app-1"); // ci_failed → pr_open
    vi.mocked(mockSCM.enrichSessionsPRBatch!).mockImplementation(
      mockBatchEnrichment({ ciStatus: "failing" }),
    );
    await lm.check("app-1"); // pr_open → ci_failed: attempt 2 → escalate
    expect(notifier.notify).toHaveBeenCalledWith(
      expect.objectContaining({ type: "reaction.escalated" }),
    );
    vi.mocked(notifier.notify).mockClear();

    // ONE passing poll (stableCount = 1, not enough)
    vi.mocked(mockSCM.enrichSessionsPRBatch!).mockImplementation(
      mockBatchEnrichment({ ciStatus: "passing" }),
    );
    await lm.check("app-1");

    // Next CI failure: tracker still escalated → short-circuit
    vi.mocked(mockSCM.enrichSessionsPRBatch!).mockImplementation(
      mockBatchEnrichment({ ciStatus: "failing" }),
    );
    await lm.check("app-1");
    expect(mockSessionManager.send).not.toHaveBeenCalled();
    expect(notifier.notify).not.toHaveBeenCalledWith(
      expect.objectContaining({ type: "reaction.escalated" }),
    );
  });

  it("pending CI does not count toward ci-failed tracker resolution", async () => {
    // Regression: real CI goes failing → pending (new run started) → failing.
    // "pending" must NOT count as resolution — only "passing" does.
    // Without this, 2 pending polls between failures wipe the tracker and we're back at #1409.
    config.reactions = {
      "ci-failed": {
        auto: true,
        action: "send-to-agent",
        message: "CI is failing.",
        retries: 2,
      },
    };

    const batchMock = mockBatchEnrichment({ ciStatus: "failing" });
    const mockSCM = createMockSCM({ enrichSessionsPRBatch: batchMock });

    const registry: PluginRegistry = {
      ...mockRegistry,
      get: vi.fn().mockImplementation((slot: string) => {
        if (slot === "runtime") return plugins.runtime;
        if (slot === "agent") return plugins.agent;
        if (slot === "scm") return mockSCM;
        return null;
      }),
    };

    vi.mocked(mockSessionManager.send).mockResolvedValue(undefined);

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makePR() }),
      registry,
    });

    // CI failing: pr_open → ci_failed, attempt 1 — send
    await lm.check("app-1");
    expect(mockSessionManager.send).toHaveBeenCalledTimes(1);
    vi.mocked(mockSessionManager.send).mockClear();

    // CI goes pending (agent pushed a fix, new run started): ci_failed → pr_open
    vi.mocked(mockSCM.enrichSessionsPRBatch!).mockImplementation(
      mockBatchEnrichment({ ciStatus: "pending" }),
    );
    await lm.check("app-1"); // stableCount must NOT increment
    await lm.check("app-1"); // two pending polls — must NOT clear tracker

    // CI fails again (run completed failing): pr_open → ci_failed, attempt 2 — send
    vi.mocked(mockSCM.enrichSessionsPRBatch!).mockImplementation(
      mockBatchEnrichment({ ciStatus: "failing" }),
    );
    await lm.check("app-1");
    // If pending had wrongly cleared the tracker, this would be attempt 1 (fresh), not attempt 2.
    // Attempt 2 ≤ retries:2 → sends to agent (not escalates)
    expect(mockSessionManager.send).toHaveBeenCalledTimes(1);
    vi.mocked(mockSessionManager.send).mockClear();

    // CI goes pending again, then failing — attempt 3 > retries:2 → escalate
    vi.mocked(mockSCM.enrichSessionsPRBatch!).mockImplementation(
      mockBatchEnrichment({ ciStatus: "pending" }),
    );
    await lm.check("app-1"); // pending: no clear
    await lm.check("app-1"); // pending: no clear
    vi.mocked(mockSCM.enrichSessionsPRBatch!).mockImplementation(
      mockBatchEnrichment({ ciStatus: "failing" }),
    );
    await lm.check("app-1");
    expect(mockSessionManager.send).not.toHaveBeenCalled(); // escalated, not sent to agent
  });

  it("only passing CI resets ci-failed tracker — pending mid-run does not interfere", async () => {
    // Complementary to previous: failing → pending(many) → passing(2) → failing SHOULD clear.
    // Pending during CI run doesn't block resolution; only the final passing state matters.
    const notifier = createMockNotifier();

    config.reactions = {
      "ci-failed": {
        auto: true,
        action: "send-to-agent",
        message: "CI is failing.",
        retries: 1,
        escalateAfter: 1,
      },
    };

    const batchMock = mockBatchEnrichment({ ciStatus: "failing" });
    const mockSCM = createMockSCM({ enrichSessionsPRBatch: batchMock });

    const registry: PluginRegistry = {
      ...mockRegistry,
      get: vi.fn().mockImplementation((slot: string, name: string) => {
        if (slot === "runtime") return plugins.runtime;
        if (slot === "agent") return plugins.agent;
        if (slot === "scm") return mockSCM;
        if (slot === "notifier" && name === "desktop") return notifier;
        return null;
      }),
    };

    vi.mocked(mockSessionManager.send).mockResolvedValue(undefined);

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makePR() }),
      registry,
    });

    // Reach escalated state: attempt 1 → send, attempt 2 → escalate
    await lm.check("app-1"); // attempt 1, send
    vi.mocked(mockSessionManager.send).mockClear();
    vi.mocked(mockSCM.enrichSessionsPRBatch!).mockImplementation(
      mockBatchEnrichment({ ciStatus: "passing" }),
    );
    await lm.check("app-1"); // ci_failed → pr_open
    vi.mocked(mockSCM.enrichSessionsPRBatch!).mockImplementation(
      mockBatchEnrichment({ ciStatus: "failing" }),
    );
    await lm.check("app-1"); // attempt 2 → escalate
    vi.mocked(notifier.notify).mockClear();

    // CI goes pending (new run) — stableCount stays 0, does NOT progress toward resolution
    vi.mocked(mockSCM.enrichSessionsPRBatch!).mockImplementation(
      mockBatchEnrichment({ ciStatus: "pending" }),
    );
    await lm.check("app-1");
    await lm.check("app-1");
    await lm.check("app-1"); // many pending polls — stableCount never reaches threshold

    // CI finally passes (2 stable polls) → tracker cleared
    vi.mocked(mockSCM.enrichSessionsPRBatch!).mockImplementation(
      mockBatchEnrichment({ ciStatus: "passing" }),
    );
    await lm.check("app-1"); // stableCount = 1
    await lm.check("app-1"); // stableCount = 2 → clearReactionTracker

    // Next CI failure gets fresh budget: attempt 1, send
    vi.mocked(mockSCM.enrichSessionsPRBatch!).mockImplementation(
      mockBatchEnrichment({ ciStatus: "failing" }),
    );
    await lm.check("app-1");
    expect(mockSessionManager.send).toHaveBeenCalledTimes(1);
    expect(notifier.notify).not.toHaveBeenCalledWith(
      expect.objectContaining({ type: "reaction.escalated" }),
    );
  });

  it("merge-conflict notify action preserves warning priority", async () => {
    const notifier = createMockNotifier();

    config.reactions = {
      "merge-conflicts": {
        auto: true,
        action: "notify",
      },
    };
    config.notificationRouting = {
      urgent: ["desktop"],
      action: ["desktop"],
      warning: ["desktop"],
      info: [],
    };

    const batchMock = mockBatchEnrichment({ hasConflicts: true });
    const mockSCM = createMockSCM({
      enrichSessionsPRBatch: batchMock,
    });

    const registry: PluginRegistry = {
      ...mockRegistry,
      get: vi.fn().mockImplementation((slot: string, name: string) => {
        if (slot === "runtime") return plugins.runtime;
        if (slot === "agent") return plugins.agent;
        if (slot === "scm") return mockSCM;
        if (slot === "notifier" && name === "desktop") return notifier;
        return null;
      }),
    };

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makePR() }),
      registry,
    });

    await lm.check("app-1");

    // With info routing empty and warning routing to desktop,
    // notify should fire at "warning" priority (not "info")
    expect(notifier.notify).toHaveBeenCalledWith(
      expect.objectContaining({
        type: "reaction.triggered",
        priority: "warning",
      }),
    );
  });
});

describe("pollAll terminal status accounting", () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it("treats all TERMINAL_STATUSES as inactive for all-complete", async () => {
    const notifier = createMockNotifier();
    const registryWithNotifier: PluginRegistry = {
      ...mockRegistry,
      get: vi.fn().mockImplementation((slot: string, name: string) => {
        if (slot === "runtime") return plugins.runtime;
        if (slot === "agent") return plugins.agent;
        if (slot === "notifier" && name === "desktop") return notifier;
        return null;
      }),
    };

    // All sessions in various terminal states — should count as inactive
    const terminalSessions = [
      makeSession({ id: "s-1", status: "killed" as SessionStatus }),
      makeSession({ id: "s-2", status: "merged" as SessionStatus }),
      makeSession({ id: "s-3", status: "done" as SessionStatus }),
      makeSession({ id: "s-4", status: "errored" as SessionStatus }),
      makeSession({ id: "s-5", status: "terminated" as SessionStatus }),
      makeSession({ id: "s-6", status: "cleanup" as SessionStatus }),
    ];

    vi.mocked(mockSessionManager.list).mockResolvedValue(terminalSessions);

    // Route info-priority notifications to desktop so we can observe them
    config.notificationRouting.info = ["desktop"];
    config.reactions = {
      "all-complete": { auto: true, action: "notify" },
    };

    const lm = createLifecycleManager({
      config,
      registry: registryWithNotifier,
      sessionManager: mockSessionManager,
    });

    lm.start(60_000);
    // Let the immediate pollAll() run
    await vi.advanceTimersByTimeAsync(0);

    expect(notifier.notify).toHaveBeenCalledWith(
      expect.objectContaining({ type: "reaction.triggered" }),
    );

    lm.stop();
  });

  it("honors a per-project all-complete reaction override on a scoped worker", async () => {
    const notifier = createMockNotifier();
    const registryWithNotifier: PluginRegistry = {
      ...mockRegistry,
      get: vi.fn().mockImplementation((slot: string, name: string) => {
        if (slot === "runtime") return plugins.runtime;
        if (slot === "agent") return plugins.agent;
        if (slot === "notifier" && name === "desktop") return notifier;
        return null;
      }),
    };

    const terminalSessions = [
      makeSession({ id: "s-1", status: "done" as SessionStatus }),
      makeSession({ id: "s-2", status: "merged" as SessionStatus }),
    ];
    vi.mocked(mockSessionManager.list).mockResolvedValue(terminalSessions);

    config.notificationRouting.info = ["desktop"];
    // Top-level all-complete WOULD fire (notify), but the per-project override
    // disables it (auto:false + non-notify action). A scoped worker must consult
    // the project's reaction override, not the global top-level reaction.
    config.reactions = { "all-complete": { auto: true, action: "notify" } };
    config.projects["my-app"] = {
      ...config.projects["my-app"],
      reactions: { "all-complete": { auto: false, action: "send-to-agent" } },
    };

    const lm = createLifecycleManager({
      config,
      registry: registryWithNotifier,
      sessionManager: mockSessionManager,
      projectId: "my-app",
    });

    lm.start(60_000);
    await vi.advanceTimersByTimeAsync(0);

    const allCompleteNotifications = vi
      .mocked(notifier.notify)
      .mock.calls.filter((call: unknown[]) => {
        const event = call[0] as Record<string, unknown> | undefined;
        const data = event?.data as Record<string, unknown> | undefined;
        const reaction =
          data?.reaction && typeof data.reaction === "object"
            ? (data.reaction as Record<string, unknown>)
            : null;
        return event?.type === "reaction.triggered" && reaction?.key === "all-complete";
      });
    expect(allCompleteNotifications).toHaveLength(0);

    lm.stop();
  });

  it("does not fire all-complete when a session is in non-terminal status like done is missing", async () => {
    const notifier = createMockNotifier();
    const registryWithNotifier: PluginRegistry = {
      ...mockRegistry,
      get: vi.fn().mockImplementation((slot: string, name: string) => {
        if (slot === "runtime") return plugins.runtime;
        if (slot === "agent") return plugins.agent;
        if (slot === "notifier" && name === "desktop") return notifier;
        return null;
      }),
    };

    // Mix of terminal and active sessions
    const sessions = [
      makeSession({ id: "s-1", status: "killed" as SessionStatus }),
      makeSession({ id: "s-2", status: "working" as SessionStatus }),
    ];

    vi.mocked(mockSessionManager.list).mockResolvedValue(sessions);

    config.reactions = {
      "all-complete": { auto: true, action: "notify" },
    };

    const lm = createLifecycleManager({
      config,
      registry: registryWithNotifier,
      sessionManager: mockSessionManager,
    });

    lm.start(60_000);
    await vi.advanceTimersByTimeAsync(0);

    // all-complete should NOT have fired — "working" is still active
    const allCompleteNotifications = vi
      .mocked(notifier.notify)
      .mock.calls.filter((call: unknown[]) => {
        const event = call[0] as Record<string, unknown> | undefined;
        const data = event?.data as Record<string, unknown> | undefined;
        const reaction =
          data?.reaction && typeof data.reaction === "object"
            ? (data.reaction as Record<string, unknown>)
            : null;
        return event?.type === "reaction.triggered" && reaction?.key === "all-complete";
      });
    expect(allCompleteNotifications).toHaveLength(0);

    lm.stop();
  });

  it("skips polling sessions in terminal statuses like done or errored", async () => {
    const isolatedPlugins = createMockPlugins();
    const isolatedRegistry = createMockRegistry({
      runtime: isolatedPlugins.runtime,
      agent: isolatedPlugins.agent,
    });

    // Sessions in "done" / "errored" should not be polled
    const sessions = [
      makeSession({ id: "s-done", status: "done" as SessionStatus }),
      makeSession({ id: "s-errored", status: "errored" as SessionStatus }),
    ];

    vi.mocked(mockSessionManager.list).mockResolvedValue(sessions);

    // If these sessions were polled, determineStatus would call runtime.isAlive.
    // Reset call count and verify it's not called.
    vi.mocked(isolatedPlugins.runtime.isAlive).mockClear();

    const lm = createLifecycleManager({
      config,
      registry: isolatedRegistry,
      sessionManager: mockSessionManager,
    });

    lm.start(60_000);
    await vi.advanceTimersByTimeAsync(0);

    // Terminal sessions should not be polled — runtime.isAlive should not be called
    expect(isolatedPlugins.runtime.isAlive).not.toHaveBeenCalled();

    lm.stop();
  });
});

describe("getStates", () => {
  it("returns copy of states map", async () => {
    const lm = setupCheck("app-1", {
      session: makeSession({ status: "spawning" }),
    });

    await lm.check("app-1");

    const states = lm.getStates();
    expect(states.get("app-1")).toBe("working");

    // Modifying returned map shouldn't affect internal state
    states.set("app-1", "killed");
    expect(lm.getStates().get("app-1")).toBe("working");
  });
});

describe("rate limiting optimizations", () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  // PR with owner/repo that matches the test config's "org/my-app"
  function makeMatchingPR() {
    return makePR({ owner: "org", repo: "my-app" });
  }

  it("skips getMergeability() when batch enrichment has hasConflicts data", async () => {
    config.reactions = {
      "merge-conflicts": {
        auto: true,
        action: "send-to-agent",
        message: "Resolve conflicts.",
      },
    };

    const pr = makeMatchingPR();
    const getMergeabilityMock = vi.fn();
    const mockSCM = createMockSCM({
      getMergeability: getMergeabilityMock,
      getCISummary: vi.fn().mockResolvedValue("passing"),
      enrichSessionsPRBatch: vi.fn().mockResolvedValue(
        new Map([
          [
            `${pr.owner}/${pr.repo}#${pr.number}`,
            {
              state: "open" as const,
              ciStatus: "passing" as const,
              reviewDecision: "none" as const,
              mergeable: false,
              hasConflicts: true,
            },
          ],
        ]),
      ),
    });

    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });

    const session = makeSession({ id: "s-1", status: "pr_open", pr, workspacePath: null });
    vi.mocked(mockSessionManager.list).mockResolvedValue([session]);
    vi.mocked(mockSessionManager.send).mockResolvedValue(undefined);

    const lm = createLifecycleManager({ config, registry, sessionManager: mockSessionManager });
    lm.start(60_000);
    await vi.advanceTimersByTimeAsync(0);
    lm.stop();

    // getMergeability() should NOT be called — batch enrichment has the data
    expect(getMergeabilityMock).not.toHaveBeenCalled();
    // Conflict notification should have been sent
    expect(mockSessionManager.send).toHaveBeenCalledWith("s-1", "Resolve conflicts.");
  });

  it("skips getCIChecks() when batch enrichment has ciChecks data", async () => {
    config.reactions = {
      "ci-failed": {
        auto: true,
        action: "send-to-agent",
        message: "CI failing.",
        retries: 3,
        escalateAfter: 3,
      },
    };

    const pr = makeMatchingPR();
    const getCIChecksMock = vi.fn();
    const mockSCM = createMockSCM({
      getCIChecks: getCIChecksMock,
      getCISummary: vi.fn().mockResolvedValue("failing"),
      enrichSessionsPRBatch: vi.fn().mockResolvedValue(
        new Map([
          [
            `${pr.owner}/${pr.repo}#${pr.number}`,
            {
              state: "open" as const,
              ciStatus: "failing" as const,
              reviewDecision: "none" as const,
              mergeable: false,
              hasConflicts: false,
              ciChecks: [
                {
                  name: "lint",
                  status: "failed" as const,
                  conclusion: "FAILURE",
                  url: "https://example.com/lint",
                },
                { name: "test", status: "passed" as const, conclusion: "SUCCESS" },
              ],
            },
          ],
        ]),
      ),
    });

    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });

    // Start with pr_open state so that ci_failed transition happens on first poll
    const session = makeSession({ id: "s-2", status: "pr_open", pr, workspacePath: null });
    vi.mocked(mockSessionManager.list).mockResolvedValue([session]);
    vi.mocked(mockSessionManager.send).mockResolvedValue(undefined);

    const lm = createLifecycleManager({ config, registry, sessionManager: mockSessionManager });
    lm.start(60_000);
    // First poll: transitions to ci_failed and sends the enriched reaction message.
    await vi.advanceTimersByTimeAsync(0);

    // getCIChecks() should NOT be called — batch enrichment has ciChecks
    expect(getCIChecksMock).not.toHaveBeenCalled();
    // Detailed message with lint check name/URL should be sent
    const calls = vi.mocked(mockSessionManager.send).mock.calls;
    const sentMessages = calls.map((c) => c[1] as string);
    const detailMessage = sentMessages.find((m) => m.includes("lint"));
    expect(detailMessage).toBeDefined();
    expect(detailMessage).toContain("https://example.com/lint");
    // Passing check should not be included
    expect(detailMessage).not.toContain("test");

    lm.stop();
  });

  it("throttles review backlog API calls to at most once per 2 minutes", async () => {
    config.reactions = {
      "changes-requested": {
        auto: true,
        action: "send-to-agent",
        message: "Handle review comments.",
      },
    };

    const getReviewThreadsMock = vi.fn().mockResolvedValue({
      threads: [
        {
          id: "c1",
          author: "reviewer",
          body: "Please fix this",
          path: "src/index.ts",
          line: 10,
          isResolved: false,
          createdAt: new Date(),
          url: "https://example.com/comment/1",
          isBot: false,
        },
      ],
      reviews: [],
    });
    const mockSCM = createMockSCM({
      getReviewThreads: getReviewThreadsMock,
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });

    vi.mocked(mockSessionManager.send).mockResolvedValue(undefined);

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makePR() }),
      registry,
    });

    // First check: API called, dispatch happens
    await lm.check("app-1");
    expect(getReviewThreadsMock).toHaveBeenCalledTimes(1);
    vi.mocked(mockSessionManager.send).mockClear();
    getReviewThreadsMock.mockClear();

    // Second check immediately after: throttled — API NOT called
    await lm.check("app-1");
    expect(getReviewThreadsMock).not.toHaveBeenCalled();
    expect(mockSessionManager.send).not.toHaveBeenCalled();

    // Advance time past the 2-minute throttle window
    await vi.advanceTimersByTimeAsync(2 * 60 * 1000 + 100);

    // Third check: throttle expired — API called again
    await lm.check("app-1");
    expect(getReviewThreadsMock).toHaveBeenCalledTimes(1);
  });

  it("clears review backlog tracking when PR is closed", async () => {
    const getPendingMock = vi.fn();
    const getAutomatedMock = vi.fn();
    const mockSCM = createMockSCM({
      getPRState: vi.fn().mockResolvedValue("closed"),
      getPendingComments: getPendingMock,
      enrichSessionsPRBatch: mockBatchEnrichment({ state: "closed" }),
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });

    const lm = setupCheck("app-1", {
      session: makeSession({
        status: "pr_open",
        pr: makePR(),
        metadata: {
          lastPendingReviewFingerprint: "fingerprint",
          lastPendingReviewDispatchHash: "dispatch",
          lastPendingReviewDispatchAt: "2025-01-01T00:00:00.000Z",
          lastAutomatedReviewFingerprint: "auto-fingerprint",
          lastAutomatedReviewDispatchHash: "auto-dispatch",
          lastAutomatedReviewDispatchAt: "2025-01-01T00:00:00.000Z",
        },
      }),
      registry,
    });

    await lm.check("app-1");

    const metadata = readMetadataRaw(env.sessionsDir, "app-1");
    expect(metadata?.["lastPendingReviewFingerprint"]).toBeFalsy();
    expect(metadata?.["lastPendingReviewDispatchHash"]).toBeFalsy();
    expect(metadata?.["lastPendingReviewDispatchAt"]).toBeFalsy();
    expect(metadata?.["lastAutomatedReviewFingerprint"]).toBeFalsy();
    expect(metadata?.["lastAutomatedReviewDispatchHash"]).toBeFalsy();
    expect(metadata?.["lastAutomatedReviewDispatchAt"]).toBeFalsy();
    expect(getPendingMock).not.toHaveBeenCalled();
    expect(getAutomatedMock).not.toHaveBeenCalled();
  });
});

describe("review loop round-cap + completion detection (#4)", () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  const THROTTLE_STEP_MS = 2 * 60 * 1000 + 100;

  function botThread(id: string) {
    return {
      id,
      author: "chatgpt-codex-connector[bot]",
      body: "Potential issue here",
      path: "src/worker.ts",
      line: 9,
      isResolved: false,
      createdAt: new Date(),
      url: `https://example.com/comment/${id}`,
      isBot: true,
      isReviewBot: true,
    };
  }

  function codexReview(overrides: Record<string, unknown> = {}) {
    return {
      author: "chatgpt-codex-connector[bot]",
      state: "commented",
      body: "",
      submittedAt: new Date(),
      isBot: true,
      isReviewBot: true,
      ...overrides,
    };
  }

  /**
   * A getReviewThreads result that carries a head SHA and a Codex review scoped
   * to that head — the shape scm-github always returns. Completion detection now
   * requires a head-scoped review, so satisfied-path tests use this.
   */
  const HEAD = "sha-head";
  function reviewedResult(
    opts: { threads?: unknown[]; head?: string; reviewedHead?: string; truncated?: boolean } = {},
  ) {
    const head = opts.head ?? HEAD;
    return {
      threads: opts.threads ?? [],
      reviews: [codexReview({ commitSha: opts.reviewedHead ?? head })],
      headSha: head,
      threadsTruncated: opts.truncated ?? false,
    };
  }

  it("stops the bot review loop and escalates to needs_input after maxRounds", async () => {
    config.reactions = {
      "bugbot-comments": {
        auto: true,
        action: "send-to-agent",
        message: DEFAULT_BUGBOT_COMMENTS_MESSAGE,
        maxRounds: 2,
      },
    };

    let currentBotId = "r1";
    const getReviewThreads = vi.fn().mockImplementation(async () => ({
      threads: [botThread(currentBotId)],
      reviews: [],
    }));
    const mockSCM = createMockSCM({ getReviewThreads });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
      notifier: createMockNotifier(), // escalation latches only on delivered notify
    });
    vi.mocked(mockSessionManager.send).mockResolvedValue(undefined);

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makePR() }),
      registry,
    });

    // Rounds 1 and 2: each distinct batch of bot comments is dispatched.
    await lm.check("app-1");
    await vi.advanceTimersByTimeAsync(THROTTLE_STEP_MS);
    currentBotId = "r2";
    await lm.check("app-1");
    expect(mockSessionManager.send).toHaveBeenCalledTimes(2);

    // Round 3 exceeds maxRounds → escalate instead of dispatching again.
    await vi.advanceTimersByTimeAsync(THROTTLE_STEP_MS);
    currentBotId = "r3";
    vi.mocked(recordActivityEvent).mockClear();
    await lm.check("app-1");

    expect(mockSessionManager.send).toHaveBeenCalledTimes(2); // no new dispatch
    const escalateMeta = readMetadataRaw(env.sessionsDir, "app-1");
    expect(escalateMeta?.["reviewRoundsEscalated"]).toBe("true");
    expect(escalateMeta?.["reviewRoundCount"]).toBe("2");
    const escalateKinds = vi
      .mocked(recordActivityEvent)
      .mock.calls.map((c) => (c[0] as { kind: string }).kind);
    expect(escalateKinds).toContain("reaction.escalated");

    // Next poll parks the session in needs_input — the loop no longer spins.
    await vi.advanceTimersByTimeAsync(THROTTLE_STEP_MS);
    await lm.check("app-1");
    expect(lm.getStates().get("app-1")).toBe("needs_input");
    expect(mockSessionManager.send).toHaveBeenCalledTimes(2);
  });

  it("does not latch the escalation when no notification was delivered", async () => {
    config.reactions = {
      "bugbot-comments": {
        auto: true,
        action: "send-to-agent",
        message: DEFAULT_BUGBOT_COMMENTS_MESSAGE,
        maxRounds: 1,
      },
    };
    let currentBotId = "r1";
    const getReviewThreads = vi.fn().mockImplementation(async () => ({
      threads: [botThread(currentBotId)],
      reviews: [],
    }));
    // No notifier registered — urgent routes to "desktop", which is missing, so
    // notifyHuman delivers nothing. The escalation must NOT latch (it retries).
    const mockSCM = createMockSCM({ getReviewThreads });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });
    vi.mocked(mockSessionManager.send).mockResolvedValue(undefined);

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makePR() }),
      registry,
    });

    await lm.check("app-1"); // round 1 dispatched
    await vi.advanceTimersByTimeAsync(THROTTLE_STEP_MS);
    currentBotId = "r2";
    await lm.check("app-1"); // exceeds maxRounds → escalation attempted but undelivered

    // Not latched → the session is not silently parked; the next poll retries.
    expect(readMetadataRaw(env.sessionsDir, "app-1")?.["reviewRoundsEscalated"]).toBeFalsy();
  });

  it("marks the review satisfied when bot threads clear and CI is green", async () => {
    config.reactions = {
      "bugbot-comments": {
        auto: true,
        action: "send-to-agent",
        message: DEFAULT_BUGBOT_COMMENTS_MESSAGE,
      },
    };

    let hasBotThreads = true;
    const getReviewThreads = vi.fn().mockImplementation(async () =>
      reviewedResult({ threads: hasBotThreads ? [botThread("b1")] : [] }),
    );
    // Default createMockSCM enrichment reports ciStatus "passing".
    const mockSCM = createMockSCM({ getReviewThreads });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });
    vi.mocked(mockSessionManager.send).mockResolvedValue(undefined);

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makePR() }),
      registry,
    });

    await lm.check("app-1"); // dispatch round 1
    expect(mockSessionManager.send).toHaveBeenCalledTimes(1);

    // Codex resolves everything — no unresolved bot threads remain.
    hasBotThreads = false;
    await vi.advanceTimersByTimeAsync(THROTTLE_STEP_MS);
    vi.mocked(recordActivityEvent).mockClear();
    await lm.check("app-1");

    const meta = readMetadataRaw(env.sessionsDir, "app-1");
    expect(meta?.["reviewSatisfiedAt"]).toBeTruthy();
    expect(meta?.["reviewRoundCount"]).toBeFalsy();
    const satisfiedKinds = vi
      .mocked(recordActivityEvent)
      .mock.calls.map((c) => (c[0] as { kind: string }).kind);
    expect(satisfiedKinds).toContain("review.satisfied");
  });

  it("does not mark satisfied while CI is not green", async () => {
    config.reactions = {
      "bugbot-comments": {
        auto: true,
        action: "send-to-agent",
        message: DEFAULT_BUGBOT_COMMENTS_MESSAGE,
      },
    };
    const mockSCM = createMockSCM({
      getReviewThreads: vi.fn().mockResolvedValue({ threads: [], reviews: [] }),
      getCISummary: vi.fn().mockResolvedValue("pending"),
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makePR() }),
      registry,
    });

    await lm.check("app-1");
    const meta = readMetadataRaw(env.sessionsDir, "app-1");
    expect(meta?.["reviewSatisfiedAt"]).toBeFalsy();
  });

  it("re-checks review threads immediately when the agent reports ready_for_review", async () => {
    config.reactions = {
      "bugbot-comments": {
        auto: true,
        action: "send-to-agent",
        message: DEFAULT_BUGBOT_COMMENTS_MESSAGE,
      },
    };
    const getReviewThreads = vi.fn().mockResolvedValue({ threads: [], reviews: [] });
    const mockSCM = createMockSCM({ getReviewThreads });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });

    const session = makeSession({ status: "pr_open", pr: makePR() });
    const lm = setupCheck("app-1", { session, registry });

    // First poll fetches and stamps the throttle timestamp.
    await lm.check("app-1");
    expect(getReviewThreads).toHaveBeenCalledTimes(1);

    // Within the throttle window and with no report → throttled, no fetch.
    await vi.advanceTimersByTimeAsync(10_000);
    getReviewThreads.mockClear();
    await lm.check("app-1");
    expect(getReviewThreads).not.toHaveBeenCalled();

    // A fresh ready_for_review report forces an immediate re-check.
    vi.mocked(mockSessionManager.get).mockResolvedValue({
      ...session,
      metadata: {
        ...session.metadata,
        agent: "mock-agent",
        agentReportedState: "ready_for_review",
        agentReportedAt: new Date().toISOString(),
      },
    });
    await lm.check("app-1");
    expect(getReviewThreads).toHaveBeenCalledTimes(1);
  });

  it("does not mark satisfied until the bot reviewer has engaged", async () => {
    config.reactions = {
      "bugbot-comments": {
        auto: true,
        action: "send-to-agent",
        message: DEFAULT_BUGBOT_COMMENTS_MESSAGE,
      },
    };
    // Fresh PR: no threads ever, CI green — the bot reviewer has not run yet.
    const mockSCM = createMockSCM({
      getReviewThreads: vi.fn().mockResolvedValue({ threads: [], reviews: [] }),
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makePR() }),
      registry,
    });

    await lm.check("app-1");
    const meta = readMetadataRaw(env.sessionsDir, "app-1");
    expect(meta?.["reviewSatisfiedAt"]).toBeFalsy();
    expect(meta?.["botReviewObserved"]).toBeFalsy();
  });

  it("clears the satisfied mark when a human thread reappears", async () => {
    config.reactions = {
      "bugbot-comments": {
        auto: true,
        action: "send-to-agent",
        message: DEFAULT_BUGBOT_COMMENTS_MESSAGE,
      },
    };
    let phase: "bot" | "clear" | "human" = "bot";
    const humanThread = {
      id: "h1",
      author: "reviewer",
      body: "Actually, please revert this",
      path: "src/worker.ts",
      line: 3,
      isResolved: false,
      createdAt: new Date(),
      url: "https://example.com/comment/h1",
      isBot: false,
    };
    const getReviewThreads = vi.fn().mockImplementation(async () => {
      if (phase === "bot") return reviewedResult({ threads: [botThread("b1")] });
      if (phase === "clear") return reviewedResult();
      return reviewedResult({ threads: [humanThread] });
    });
    const mockSCM = createMockSCM({ getReviewThreads });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });
    vi.mocked(mockSessionManager.send).mockResolvedValue(undefined);

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makePR() }),
      registry,
    });

    await lm.check("app-1"); // bot round → botReviewObserved recorded
    phase = "clear";
    await vi.advanceTimersByTimeAsync(THROTTLE_STEP_MS);
    await lm.check("app-1"); // threads clear + CI green → satisfied
    expect(readMetadataRaw(env.sessionsDir, "app-1")?.["reviewSatisfiedAt"]).toBeTruthy();

    phase = "human";
    await vi.advanceTimersByTimeAsync(THROTTLE_STEP_MS);
    await lm.check("app-1"); // human thread reappears → satisfied mark cleared
    expect(readMetadataRaw(env.sessionsDir, "app-1")?.["reviewSatisfiedAt"]).toBeFalsy();
  });

  it("lets a merged PR override the escalation latch", async () => {
    config.reactions = {
      "bugbot-comments": {
        auto: true,
        action: "send-to-agent",
        message: DEFAULT_BUGBOT_COMMENTS_MESSAGE,
        maxRounds: 1,
      },
    };
    let currentBotId = "r1";
    let prState: "open" | "merged" | "closed" = "open";
    const getReviewThreads = vi.fn().mockImplementation(async () => ({
      threads: [botThread(currentBotId)],
      reviews: [],
    }));
    const mockSCM = createMockSCM({
      getReviewThreads,
      getPRState: vi.fn().mockImplementation(async () => prState),
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
      notifier: createMockNotifier(), // escalation latches only on delivered notify
    });
    vi.mocked(mockSessionManager.send).mockResolvedValue(undefined);

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makePR() }),
      registry,
    });

    await lm.check("app-1"); // round 1 dispatched
    await vi.advanceTimersByTimeAsync(THROTTLE_STEP_MS);
    currentBotId = "r2";
    await lm.check("app-1"); // round 2 exceeds maxRounds → escalate
    expect(readMetadataRaw(env.sessionsDir, "app-1")?.["reviewRoundsEscalated"]).toBe("true");

    await vi.advanceTimersByTimeAsync(THROTTLE_STEP_MS);
    await lm.check("app-1"); // parked in needs_input
    expect(lm.getStates().get("app-1")).toBe("needs_input");

    // A human merges the PR while the latch is still set — terminal state wins.
    prState = "merged";
    await vi.advanceTimersByTimeAsync(THROTTLE_STEP_MS);
    await lm.check("app-1");
    expect(lm.getStates().get("app-1")).toBe("merged");
    expect(readMetadataRaw(env.sessionsDir, "app-1")?.["reviewRoundsEscalated"]).toBeFalsy();
  });

  it("defers multi-PR sessions (never marks satisfied) to the merge gate", async () => {
    config.reactions = {
      "bugbot-comments": {
        auto: true,
        action: "send-to-agent",
        message: DEFAULT_BUGBOT_COMMENTS_MESSAGE,
      },
    };
    const pr1 = makePR({ number: 42 });
    const pr2 = makePR({ number: 43, url: "https://github.com/org/my-app/pull/43" });
    let primaryHasBot = true;
    // Both PRs fully clean + green, but multi-PR completion is deferred to #15
    // so the session must NOT emit a false clean signal for either PR.
    const getReviewThreads = vi.fn().mockImplementation(async (pr: PRInfo) => {
      if (pr.number === 42) return { threads: primaryHasBot ? [botThread("b1")] : [], reviews: [] };
      return { threads: [], reviews: [] };
    });
    const mockSCM = createMockSCM({ getReviewThreads });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });
    vi.mocked(mockSessionManager.send).mockResolvedValue(undefined);

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: pr1, prs: [pr1, pr2] }),
      registry,
    });

    await lm.check("app-1"); // primary bot round
    primaryHasBot = false;
    await vi.advanceTimersByTimeAsync(THROTTLE_STEP_MS);
    await lm.check("app-1"); // everything clean, but multi-PR → not satisfied
    expect(readMetadataRaw(env.sessionsDir, "app-1")?.["reviewSatisfiedAt"]).toBeFalsy();
  });

  it("recognizes a clean bot review with no inline comments", async () => {
    config.reactions = {
      "bugbot-comments": {
        auto: true,
        action: "send-to-agent",
        message: DEFAULT_BUGBOT_COMMENTS_MESSAGE,
      },
    };
    // Codex submits a review with zero inline comment threads (the no-issue case).
    const mockSCM = createMockSCM({
      getReviewThreads: vi.fn().mockResolvedValue(reviewedResult()),
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makePR() }),
      registry,
    });

    await lm.check("app-1");
    const meta = readMetadataRaw(env.sessionsDir, "app-1");
    expect(meta?.["botReviewObserved"]).toBe("true");
    expect(meta?.["reviewSatisfiedAt"]).toBeTruthy();
  });

  it("does not count subset resolution of one batch as a new round", async () => {
    config.reactions = {
      "bugbot-comments": {
        auto: true,
        action: "send-to-agent",
        message: DEFAULT_BUGBOT_COMMENTS_MESSAGE,
        maxRounds: 1,
      },
    };
    // One review batch of two comments; the agent then resolves one at a time.
    let threads = [botThread("a1"), botThread("a2")];
    const getReviewThreads = vi.fn().mockImplementation(async () => ({ threads, reviews: [] }));
    const mockSCM = createMockSCM({ getReviewThreads });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });
    vi.mocked(mockSessionManager.send).mockResolvedValue(undefined);

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makePR() }),
      registry,
    });

    await lm.check("app-1"); // round 1: dispatch {a1,a2}
    expect(mockSessionManager.send).toHaveBeenCalledTimes(1);

    // Agent resolves a1 — the unresolved set shrinks to a subset of the batch.
    threads = [botThread("a2")];
    await vi.advanceTimersByTimeAsync(THROTTLE_STEP_MS);
    await lm.check("app-1");

    // No NEW bot review happened, so no re-dispatch, no new round, no escalation
    // (with maxRounds=1 a false round here would immediately escalate).
    expect(mockSessionManager.send).toHaveBeenCalledTimes(1);
    const meta = readMetadataRaw(env.sessionsDir, "app-1");
    expect(meta?.["reviewRoundCount"]).toBe("1");
    expect(meta?.["reviewRoundsEscalated"]).toBeFalsy();
  });

  it("clears the satisfied mark when CI regresses", async () => {
    config.reactions = {
      "bugbot-comments": {
        auto: true,
        action: "send-to-agent",
        message: DEFAULT_BUGBOT_COMMENTS_MESSAGE,
      },
    };
    let hasBotThreads = true;
    let ci = "passing";
    const getReviewThreads = vi.fn().mockImplementation(async () =>
      reviewedResult({ threads: hasBotThreads ? [botThread("b1")] : [] }),
    );
    const mockSCM = createMockSCM({
      getReviewThreads,
      getCISummary: vi.fn().mockImplementation(async () => ci),
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });
    vi.mocked(mockSessionManager.send).mockResolvedValue(undefined);

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makePR() }),
      registry,
    });

    await lm.check("app-1"); // bot round → botReviewObserved
    hasBotThreads = false;
    await vi.advanceTimersByTimeAsync(THROTTLE_STEP_MS);
    await lm.check("app-1"); // threads clear + CI green → satisfied
    expect(readMetadataRaw(env.sessionsDir, "app-1")?.["reviewSatisfiedAt"]).toBeTruthy();

    // A later push flips CI back to pending while threads stay clear.
    ci = "pending";
    await vi.advanceTimersByTimeAsync(THROTTLE_STEP_MS);
    await lm.check("app-1");
    expect(readMetadataRaw(env.sessionsDir, "app-1")?.["reviewSatisfiedAt"]).toBeFalsy();
  });

  it("releases the escalation latch on a live merge when enrichment is missing", async () => {
    config.reactions = {
      "bugbot-comments": {
        auto: true,
        action: "send-to-agent",
        message: DEFAULT_BUGBOT_COMMENTS_MESSAGE,
        maxRounds: 1,
      },
    };
    let currentBotId = "r1";
    let prState: "open" | "merged" | "closed" = "open";
    const getReviewThreads = vi.fn().mockImplementation(async () => ({
      threads: [botThread(currentBotId)],
      reviews: [],
    }));
    const mockSCM = createMockSCM({
      getReviewThreads,
      getPRState: vi.fn().mockImplementation(async () => prState),
      // Force a batch enrichment cache miss so the guard must consult getPRState.
      enrichSessionsPRBatch: vi.fn().mockResolvedValue(new Map()),
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
      notifier: createMockNotifier(), // escalation latches only on delivered notify
    });
    vi.mocked(mockSessionManager.send).mockResolvedValue(undefined);

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makePR() }),
      registry,
    });

    await lm.check("app-1"); // round 1 dispatched
    await vi.advanceTimersByTimeAsync(THROTTLE_STEP_MS);
    currentBotId = "r2";
    await lm.check("app-1"); // exceeds maxRounds → escalate
    expect(readMetadataRaw(env.sessionsDir, "app-1")?.["reviewRoundsEscalated"]).toBe("true");

    await vi.advanceTimersByTimeAsync(THROTTLE_STEP_MS);
    await lm.check("app-1");
    expect(lm.getStates().get("app-1")).toBe("needs_input");

    // Human merges; enrichment is still absent, so only the live getPRState
    // fallback in the guard can release the latch.
    prState = "merged";
    await vi.advanceTimersByTimeAsync(THROTTLE_STEP_MS);
    await lm.check("app-1");
    expect(lm.getStates().get("app-1")).toBe("merged");
    expect(readMetadataRaw(env.sessionsDir, "app-1")?.["reviewRoundsEscalated"]).toBeFalsy();
  });

  it("force-fetches while escalated so a human UI resolution unblocks the loop", async () => {
    config.reactions = {
      "bugbot-comments": {
        auto: true,
        action: "send-to-agent",
        message: DEFAULT_BUGBOT_COMMENTS_MESSAGE,
        maxRounds: 1,
      },
    };
    let botId = "r1";
    let resolved = false;
    const getReviewThreads = vi.fn().mockImplementation(async () => ({
      threads: resolved ? [] : [botThread(botId)],
      reviews: [codexReview({ commitSha: "h1" })],
      headSha: "h1",
    }));
    const mockSCM = createMockSCM({ getReviewThreads });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
      notifier: createMockNotifier(), // escalation latches only on delivered notify
    });
    vi.mocked(mockSessionManager.send).mockResolvedValue(undefined);

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makePR() }),
      registry,
    });

    await lm.check("app-1"); // round 1
    await vi.advanceTimersByTimeAsync(THROTTLE_STEP_MS);
    botId = "r2";
    await lm.check("app-1"); // exceeds maxRounds → escalate
    expect(readMetadataRaw(env.sessionsDir, "app-1")?.["reviewRoundsEscalated"]).toBe("true");

    // While escalated, the fetch is forced fresh so GraphQL-only resolution is seen.
    getReviewThreads.mockClear();
    resolved = true; // human resolves the threads in the GitHub UI
    await vi.advanceTimersByTimeAsync(THROTTLE_STEP_MS);
    await lm.check("app-1");
    expect(getReviewThreads.mock.calls[0]?.[1]).toMatchObject({ forceFresh: true });
    // Threads now clear → the escalation latch is released.
    expect(readMetadataRaw(env.sessionsDir, "app-1")?.["reviewRoundsEscalated"]).toBeFalsy();
  });

  it("forces a fresh thread fetch on a ready-for-review recheck", async () => {
    config.reactions = {
      "bugbot-comments": {
        auto: true,
        action: "send-to-agent",
        message: DEFAULT_BUGBOT_COMMENTS_MESSAGE,
      },
    };
    const getReviewThreads = vi.fn().mockResolvedValue({ threads: [], reviews: [] });
    const mockSCM = createMockSCM({ getReviewThreads });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });

    const session = makeSession({ status: "pr_open", pr: makePR() });
    const lm = setupCheck("app-1", { session, registry });

    await lm.check("app-1"); // normal fetch — not forced
    expect(getReviewThreads.mock.calls[0]?.[1]).toMatchObject({ forceFresh: false });

    await vi.advanceTimersByTimeAsync(10_000);
    getReviewThreads.mockClear();
    vi.mocked(mockSessionManager.get).mockResolvedValue({
      ...session,
      metadata: {
        ...session.metadata,
        agent: "mock-agent",
        agentReportedState: "ready_for_review",
        agentReportedAt: new Date().toISOString(),
      },
    });
    await lm.check("app-1"); // ready recheck → forceFresh bypasses the cache
    expect(getReviewThreads.mock.calls[0]?.[1]).toMatchObject({ forceFresh: true });
  });

  it("does not satisfy on a non-reviewer bot review", async () => {
    config.reactions = {
      "bugbot-comments": {
        auto: true,
        action: "send-to-agent",
        message: DEFAULT_BUGBOT_COMMENTS_MESSAGE,
      },
    };
    // A CI/coverage bot submitted a review, but it is not a code reviewer.
    const mockSCM = createMockSCM({
      getReviewThreads: vi.fn().mockResolvedValue({
        threads: [],
        reviews: [codexReview({ author: "github-actions[bot]", isReviewBot: false })],
      }),
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makePR() }),
      registry,
    });

    await lm.check("app-1");
    const meta = readMetadataRaw(env.sessionsDir, "app-1");
    expect(meta?.["botReviewObserved"]).toBeFalsy();
    expect(meta?.["reviewSatisfiedAt"]).toBeFalsy();
  });

  it("does not satisfy while a changes_requested review is open", async () => {
    config.reactions = {
      "bugbot-comments": {
        auto: true,
        action: "send-to-agent",
        message: DEFAULT_BUGBOT_COMMENTS_MESSAGE,
      },
    };
    // Codex reviewed (clean, no inline threads) but a human requested changes
    // at the top level — no inline threads, CI green.
    const mockSCM = createMockSCM({
      getReviewThreads: vi.fn().mockResolvedValue(reviewedResult()),
      getReviewDecision: vi.fn().mockResolvedValue("changes_requested"),
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makePR() }),
      registry,
    });

    await lm.check("app-1");
    expect(readMetadataRaw(env.sessionsDir, "app-1")?.["reviewSatisfiedAt"]).toBeFalsy();
  });

  it("satisfies via live CI when batch enrichment is unavailable", async () => {
    config.reactions = {
      "bugbot-comments": {
        auto: true,
        action: "send-to-agent",
        message: DEFAULT_BUGBOT_COMMENTS_MESSAGE,
      },
    };
    const mockSCM = createMockSCM({
      getReviewThreads: vi.fn().mockResolvedValue(reviewedResult()),
      getCISummary: vi.fn().mockResolvedValue("passing"),
      getReviewDecision: vi.fn().mockResolvedValue("none"),
      // No batch enrichment → completion must fall back to live getCISummary.
      enrichSessionsPRBatch: vi.fn().mockResolvedValue(new Map()),
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makePR() }),
      registry,
    });

    await lm.check("app-1");
    expect(readMetadataRaw(env.sessionsDir, "app-1")?.["reviewSatisfiedAt"]).toBeTruthy();
  });

  it("requires a review of the current head after a new push", async () => {
    config.reactions = {
      "bugbot-comments": {
        auto: true,
        action: "send-to-agent",
        message: DEFAULT_BUGBOT_COMMENTS_MESSAGE,
      },
    };
    let head = "sha-a";
    let reviewedSha = "sha-a";
    let ci = "passing";
    const getReviewThreads = vi.fn().mockImplementation(async () => ({
      threads: [],
      reviews: [codexReview({ commitSha: reviewedSha })],
      headSha: head,
    }));
    const mockSCM = createMockSCM({
      getReviewThreads,
      getCISummary: vi.fn().mockImplementation(async () => ci),
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makePR() }),
      registry,
    });

    await lm.check("app-1"); // review of head sha-a + CI green → satisfied
    let meta = readMetadataRaw(env.sessionsDir, "app-1");
    expect(meta?.["reviewSatisfiedAt"]).toBeTruthy();
    expect(meta?.["botReviewObservedSha"]).toBe("sha-a");

    // New push → head sha-b, CI pending. The review is still of sha-a.
    head = "sha-b";
    ci = "pending";
    await vi.advanceTimersByTimeAsync(THROTTLE_STEP_MS);
    await lm.check("app-1");
    expect(readMetadataRaw(env.sessionsDir, "app-1")?.["reviewSatisfiedAt"]).toBeFalsy();

    // CI passes on sha-b but the review still only covers sha-a → NOT satisfied.
    ci = "passing";
    await vi.advanceTimersByTimeAsync(THROTTLE_STEP_MS);
    await lm.check("app-1");
    expect(readMetadataRaw(env.sessionsDir, "app-1")?.["reviewSatisfiedAt"]).toBeFalsy();

    // The reviewer reviews sha-b → satisfied again on the current head.
    reviewedSha = "sha-b";
    await vi.advanceTimersByTimeAsync(THROTTLE_STEP_MS);
    await lm.check("app-1");
    meta = readMetadataRaw(env.sessionsDir, "app-1");
    expect(meta?.["reviewSatisfiedAt"]).toBeTruthy();
    expect(meta?.["botReviewObservedSha"]).toBe("sha-b");
  });

  it("fails closed when the review-thread page is truncated", async () => {
    config.reactions = {
      "bugbot-comments": {
        auto: true,
        action: "send-to-agent",
        message: DEFAULT_BUGBOT_COMMENTS_MESSAGE,
      },
    };
    // No threads returned, but the result is flagged truncated — an unresolved
    // thread could exist outside the fetched window.
    const mockSCM = createMockSCM({
      getReviewThreads: vi.fn().mockResolvedValue(reviewedResult({ truncated: true })),
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makePR() }),
      registry,
    });

    await lm.check("app-1");
    expect(readMetadataRaw(env.sessionsDir, "app-1")?.["reviewSatisfiedAt"]).toBeFalsy();
  });

  it("treats a no-CI PR as green for satisfaction", async () => {
    config.reactions = {
      "bugbot-comments": {
        auto: true,
        action: "send-to-agent",
        message: DEFAULT_BUGBOT_COMMENTS_MESSAGE,
      },
    };
    const mockSCM = createMockSCM({
      getReviewThreads: vi.fn().mockResolvedValue(reviewedResult()),
      getCISummary: vi.fn().mockResolvedValue("none"), // no configured checks
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makePR() }),
      registry,
    });

    await lm.check("app-1");
    expect(readMetadataRaw(env.sessionsDir, "app-1")?.["reviewSatisfiedAt"]).toBeTruthy();
  });

  it("fails closed when the review decision cannot be read", async () => {
    config.reactions = {
      "bugbot-comments": {
        auto: true,
        action: "send-to-agent",
        message: DEFAULT_BUGBOT_COMMENTS_MESSAGE,
      },
    };
    const mockSCM = createMockSCM({
      getReviewThreads: vi.fn().mockResolvedValue(reviewedResult()),
      getReviewDecision: vi.fn().mockRejectedValue(new Error("permission denied")),
      // Cache miss forces the live getReviewDecision path (which throws).
      enrichSessionsPRBatch: vi.fn().mockResolvedValue(new Map()),
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makePR() }),
      registry,
    });

    await lm.check("app-1");
    expect(readMetadataRaw(env.sessionsDir, "app-1")?.["reviewSatisfiedAt"]).toBeFalsy();
  });

  it("revalidates a satisfied session despite the throttle", async () => {
    config.reactions = {
      "bugbot-comments": {
        auto: true,
        action: "send-to-agent",
        message: DEFAULT_BUGBOT_COMMENTS_MESSAGE,
      },
    };
    let hasHumanThread = false;
    const humanThread = {
      id: "h1",
      author: "reviewer",
      body: "wait",
      path: "src/x.ts",
      line: 1,
      isResolved: false,
      createdAt: new Date(),
      url: "https://example.com/h1",
      isBot: false,
    };
    const getReviewThreads = vi.fn().mockImplementation(async () =>
      reviewedResult({ threads: hasHumanThread ? [humanThread] : [] }),
    );
    const mockSCM = createMockSCM({ getReviewThreads });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makePR() }),
      registry,
    });

    await lm.check("app-1"); // clean review + CI green → satisfied
    expect(readMetadataRaw(env.sessionsDir, "app-1")?.["reviewSatisfiedAt"]).toBeTruthy();

    // A human thread appears only 10s later — inside the 2-minute throttle window.
    hasHumanThread = true;
    getReviewThreads.mockClear();
    await vi.advanceTimersByTimeAsync(10_000);
    await lm.check("app-1");

    // A satisfied session bypasses the throttle AND forces a fresh fetch (a thread
    // flipping back to unresolved is a GraphQL-only change the REST ETag misses).
    expect(getReviewThreads.mock.calls[0]?.[1]).toMatchObject({ forceFresh: true });
    expect(readMetadataRaw(env.sessionsDir, "app-1")?.["reviewSatisfiedAt"]).toBeFalsy();
  });

  it("accumulates the round count across re-reviews that clear without satisfying", async () => {
    config.reactions = {
      "bugbot-comments": {
        auto: true,
        action: "send-to-agent",
        message: DEFAULT_BUGBOT_COMMENTS_MESSAGE,
        maxRounds: 2,
      },
    };
    // CI stays pending, so a threads-clear between rounds never satisfies — the
    // round counter must persist so maxRounds eventually fires.
    let threads: unknown[] = [botThread("b1")];
    const getReviewThreads = vi.fn().mockImplementation(async () => ({
      threads,
      reviews: [codexReview({ commitSha: "h1" })],
      headSha: "h1",
    }));
    const mockSCM = createMockSCM({
      getReviewThreads,
      getCISummary: vi.fn().mockResolvedValue("pending"),
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
      notifier: createMockNotifier(), // escalation latches only on delivered notify
    });
    vi.mocked(mockSessionManager.send).mockResolvedValue(undefined);

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makePR() }),
      registry,
    });

    await lm.check("app-1"); // round 1: dispatch b1
    expect(readMetadataRaw(env.sessionsDir, "app-1")?.["reviewRoundCount"]).toBe("1");

    // Agent resolves b1; CI still pending → not satisfied → counter must NOT reset.
    threads = [];
    await vi.advanceTimersByTimeAsync(THROTTLE_STEP_MS);
    await lm.check("app-1");
    expect(readMetadataRaw(env.sessionsDir, "app-1")?.["reviewRoundCount"]).toBe("1");

    // Codex re-reviews (round 2).
    threads = [botThread("b2")];
    await vi.advanceTimersByTimeAsync(THROTTLE_STEP_MS);
    await lm.check("app-1");
    expect(readMetadataRaw(env.sessionsDir, "app-1")?.["reviewRoundCount"]).toBe("2");

    threads = [];
    await vi.advanceTimersByTimeAsync(THROTTLE_STEP_MS);
    await lm.check("app-1"); // clears again, still pending → no reset

    // Third distinct review exceeds maxRounds → escalate (would never happen if
    // the counter reset on each threads-clear).
    threads = [botThread("b3")];
    await vi.advanceTimersByTimeAsync(THROTTLE_STEP_MS);
    await lm.check("app-1");
    expect(readMetadataRaw(env.sessionsDir, "app-1")?.["reviewRoundsEscalated"]).toBe("true");
  });

  it("emits a review.satisfied notification when the loop completes", async () => {
    config.reactions = {
      "bugbot-comments": {
        auto: true,
        action: "send-to-agent",
        message: DEFAULT_BUGBOT_COMMENTS_MESSAGE,
      },
    };
    const notifier = createMockNotifier();
    const mockSCM = createMockSCM({
      getReviewThreads: vi.fn().mockResolvedValue(reviewedResult()),
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
      notifier,
    });

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makePR() }),
      registry,
    });

    await lm.check("app-1");
    expect(readMetadataRaw(env.sessionsDir, "app-1")?.["reviewSatisfiedAt"]).toBeTruthy();
    expect(notifier.notify).toHaveBeenCalledWith(
      expect.objectContaining({ type: "review.satisfied" }),
    );
  });
});

describe("auto-nudge stuck/idle agents with pending PR comments (#5)", () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  const THROTTLE_STEP_MS = 2 * 60 * 1000 + 100;

  function botThread(id: string) {
    return {
      id,
      author: "chatgpt-codex-connector[bot]",
      body: "Potential issue here",
      path: "src/worker.ts",
      line: 9,
      isResolved: false,
      createdAt: new Date(),
      url: `https://example.com/comment/${id}`,
      isBot: true,
      isReviewBot: true,
    };
  }

  /**
   * Build a lifecycle manager for a session whose agent is idle beyond the
   * agent-stuck threshold and whose PR has unresolved Codex threads. By default
   * the PR resolves to STUCK (open, non-mergeable, no formal review), but callers
   * can pass an `enrichment` override to exercise the overlay statuses
   * (review_pending, changes_requested, mergeable, …) where the agent is still
   * idle-beyond-threshold but never surfaces as legacy `stuck`.
   */
  function setupStuckSession(
    getReviewThreads: SCM["getReviewThreads"],
    opts: {
      nudgeRetries?: number;
      withNotifier?: boolean;
      stuckAuto?: boolean;
      metaOverrides?: Record<string, string>;
      notifier?: ReturnType<typeof createMockNotifier>;
      enrichment?: Parameters<typeof mockBatchEnrichment>[0];
    } = {},
  ) {
    config.reactions = {
      "agent-stuck": {
        auto: opts.stuckAuto ?? true,
        action: "notify",
        priority: "urgent",
        threshold: "1m",
        ...(opts.nudgeRetries !== undefined ? { nudgeRetries: opts.nudgeRetries } : {}),
      },
      "bugbot-comments": {
        auto: true,
        action: "send-to-agent",
        message: DEFAULT_BUGBOT_COMMENTS_MESSAGE,
      },
    };

    // Idle beyond the 1m threshold; the timestamp is recomputed on every call so
    // it stays a fixed 2 min old as the fake clock advances.
    vi.mocked(plugins.agent.getActivityState).mockImplementation(async () => ({
      state: "idle" as ActivityState,
      timestamp: new Date(Date.now() - 120_000),
    }));

    const mockSCM = createMockSCM({
      getReviewThreads,
      enrichSessionsPRBatch: mockBatchEnrichment(
        opts.enrichment ?? {
          state: "open",
          ciStatus: "passing",
          reviewDecision: "none",
          mergeable: false,
        },
      ),
    });
    const notifier = opts.notifier ?? (opts.withNotifier ? createMockNotifier() : undefined);
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
      ...(notifier ? { notifier } : {}),
    });
    vi.mocked(mockSessionManager.send).mockResolvedValue(undefined);

    return setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makePR(), metadata: { agent: "mock-agent" } }),
      metaOverrides: { agent: "mock-agent", ...opts.metaOverrides },
      registry,
    });
  }

  it("re-delivers already-dispatched comments to a stuck agent (bypassing the fingerprint guard)", async () => {
    const getReviewThreads = vi.fn().mockResolvedValue({ threads: [botThread("c1")], reviews: [] });
    const lm = setupStuckSession(getReviewThreads, { nudgeRetries: 3, withNotifier: true });

    // Poll 1: session is stuck; the bugbot loop delivers the comment once.
    await lm.check("app-1");
    expect(lm.getStates().get("app-1")).toBe("stuck");
    expect(mockSessionManager.send).toHaveBeenCalledTimes(1);

    // Poll 2: no new comments (unchanged fingerprint) — the normal loop would
    // stay silent, but the stuck-agent nudge re-delivers them.
    await vi.advanceTimersByTimeAsync(THROTTLE_STEP_MS);
    await lm.check("app-1");
    expect(mockSessionManager.send).toHaveBeenCalledTimes(2);
    const nudgeMessage = vi.mocked(mockSessionManager.send).mock.calls[1][1] as string;
    expect(nudgeMessage).toContain("unaddressed review comment");
    expect(readMetadataRaw(env.sessionsDir, "app-1")?.["stuckNudgeCount"]).toBe("1");
  });

  it("escalates to needs_input + notify after nudgeRetries are exhausted", async () => {
    const getReviewThreads = vi.fn().mockResolvedValue({ threads: [botThread("c1")], reviews: [] });
    const notifier = createMockNotifier();
    config.reactions = {
      "agent-stuck": {
        auto: true,
        action: "notify",
        priority: "urgent",
        threshold: "1m",
        nudgeRetries: 2,
      },
      "bugbot-comments": {
        auto: true,
        action: "send-to-agent",
        message: DEFAULT_BUGBOT_COMMENTS_MESSAGE,
      },
    };
    vi.mocked(plugins.agent.getActivityState).mockImplementation(async () => ({
      state: "idle" as ActivityState,
      timestamp: new Date(Date.now() - 120_000),
    }));
    const mockSCM = createMockSCM({
      getReviewThreads,
      enrichSessionsPRBatch: mockBatchEnrichment({
        state: "open",
        ciStatus: "passing",
        reviewDecision: "none",
        mergeable: false,
      }),
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
      notifier,
    });
    vi.mocked(mockSessionManager.send).mockResolvedValue(undefined);
    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makePR(), metadata: { agent: "mock-agent" } }),
      metaOverrides: { agent: "mock-agent" },
      registry,
    });

    await lm.check("app-1"); // dispatch (send #1)
    await vi.advanceTimersByTimeAsync(THROTTLE_STEP_MS);
    await lm.check("app-1"); // nudge 1 (send #2)
    await vi.advanceTimersByTimeAsync(THROTTLE_STEP_MS);
    await lm.check("app-1"); // nudge 2 (send #3)
    expect(mockSessionManager.send).toHaveBeenCalledTimes(3);
    expect(readMetadataRaw(env.sessionsDir, "app-1")?.["stuckNudgeCount"]).toBe("2");

    // Next poll exhausts the budget → escalate instead of nudging again.
    vi.mocked(recordActivityEvent).mockClear();
    await vi.advanceTimersByTimeAsync(THROTTLE_STEP_MS);
    await lm.check("app-1");
    expect(mockSessionManager.send).toHaveBeenCalledTimes(3); // no further nudge
    const meta = readMetadataRaw(env.sessionsDir, "app-1");
    expect(meta?.["stuckNudgeEscalated"]).toBe("true");
    const kinds = vi
      .mocked(recordActivityEvent)
      .mock.calls.map((c) => (c[0] as { kind: string }).kind);
    expect(kinds).toContain("reaction.escalated");

    // The escalation latch parks the session in needs_input on the next poll.
    await vi.advanceTimersByTimeAsync(THROTTLE_STEP_MS);
    await lm.check("app-1");
    expect(lm.getStates().get("app-1")).toBe("needs_input");
    expect(mockSessionManager.send).toHaveBeenCalledTimes(3);
  });

  it("clears the nudge latch once the agent addresses the comments", async () => {
    let threads: ReturnType<typeof botThread>[] = [botThread("c1")];
    const getReviewThreads = vi.fn().mockImplementation(async () => ({ threads, reviews: [] }));
    const lm = setupStuckSession(getReviewThreads, {
      nudgeRetries: 1,
      withNotifier: true,
      // Start already-escalated (latched in needs_input) so we only verify the release path.
      metaOverrides: {
        stuckNudgeEscalated: "true",
        stuckNudgeCount: "1",
        stuckNudgeFingerprint: "stale",
      },
    });

    // Comments resolved → the backlog is empty, so the latch and counters clear.
    threads = [];
    await lm.check("app-1");
    const meta = readMetadataRaw(env.sessionsDir, "app-1");
    expect(meta?.["stuckNudgeEscalated"]).toBeFalsy();
    expect(meta?.["stuckNudgeCount"]).toBeFalsy();
  });

  it("does not nudge an actively working agent that has pending comments", async () => {
    const getReviewThreads = vi.fn().mockResolvedValue({ threads: [botThread("c1")], reviews: [] });
    config.reactions = {
      "agent-stuck": { auto: true, action: "notify", threshold: "1m", nudgeRetries: 3 },
      "bugbot-comments": {
        auto: true,
        action: "send-to-agent",
        message: DEFAULT_BUGBOT_COMMENTS_MESSAGE,
      },
    };
    // Active agent — never stuck.
    vi.mocked(plugins.agent.getActivityState).mockResolvedValue({ state: "active" });
    const mockSCM = createMockSCM({ getReviewThreads });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });
    vi.mocked(mockSessionManager.send).mockResolvedValue(undefined);
    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makePR(), metadata: { agent: "mock-agent" } }),
      metaOverrides: { agent: "mock-agent" },
      registry,
    });

    await lm.check("app-1"); // bugbot dispatch (send #1)
    await vi.advanceTimersByTimeAsync(THROTTLE_STEP_MS);
    await lm.check("app-1"); // still active → no nudge
    expect(mockSessionManager.send).toHaveBeenCalledTimes(1);
    expect(readMetadataRaw(env.sessionsDir, "app-1")?.["stuckNudgeCount"]).toBeFalsy();
  });

  // Finding #1: nudge must fire for an idle-beyond-threshold agent even when the
  // PR resolves to an overlay status (never legacy `stuck`).
  it("nudges an idle agent whose PR is in an overlay status (review_pending), not just stuck", async () => {
    const getReviewThreads = vi.fn().mockResolvedValue({ threads: [botThread("c1")], reviews: [] });
    // reviewDecision "pending" → resolveOpenPRDecision returns review_pending
    // BEFORE it would ever escalate idle→stuck, so newStatus is never "stuck".
    const lm = setupStuckSession(getReviewThreads, {
      nudgeRetries: 3,
      withNotifier: true,
      enrichment: { state: "open", ciStatus: "passing", reviewDecision: "pending", mergeable: false },
    });

    await lm.check("app-1"); // bugbot delivers the comment once
    expect(lm.getStates().get("app-1")).toBe("review_pending");
    expect(mockSessionManager.send).toHaveBeenCalledTimes(1);

    await vi.advanceTimersByTimeAsync(THROTTLE_STEP_MS);
    await lm.check("app-1");
    // Still review_pending (never stuck), yet the idle-beyond-threshold agent is nudged.
    expect(lm.getStates().get("app-1")).toBe("review_pending");
    expect(mockSessionManager.send).toHaveBeenCalledTimes(2);
    expect(readMetadataRaw(env.sessionsDir, "app-1")?.["stuckNudgeCount"]).toBe("1");
  });

  // Finding #2: the immediate agent-stuck notify must be deferred when the nudge
  // path will handle the session (PR comments already delivered before it stuck).
  it("defers the agent-stuck human notify to the nudge path when comments were already delivered", async () => {
    const notifier = createMockNotifier();
    const getReviewThreads = vi.fn().mockResolvedValue({ threads: [botThread("c1")], reviews: [] });
    // Simulate a prior poll having already dispatched the comment (hash set),
    // then the session goes stuck this poll.
    const lm = setupStuckSession(getReviewThreads, {
      nudgeRetries: 3,
      notifier,
      metaOverrides: {
        lastAutomatedReviewFingerprint: "c1",
        lastAutomatedReviewDispatchHash: "c1",
      },
    });

    await lm.check("app-1"); // pr_open → stuck transition
    expect(lm.getStates().get("app-1")).toBe("stuck");
    // The stuck transition notify is suppressed — the nudge (send-to-agent) fires instead.
    expect(notifier.notify).not.toHaveBeenCalled();
    expect(mockSessionManager.send).toHaveBeenCalledTimes(1);
    expect(readMetadataRaw(env.sessionsDir, "app-1")?.["stuckNudgeCount"]).toBe("1");
  });

  // Finding #3: agent-stuck configured with auto:false must NOT message the agent.
  it("does not nudge when the agent-stuck reaction is disabled (auto:false)", async () => {
    const notifier = createMockNotifier();
    const getReviewThreads = vi.fn().mockResolvedValue({ threads: [botThread("c1")], reviews: [] });
    // Comments already delivered so bugbot won't re-dispatch — isolates the nudge.
    const lm = setupStuckSession(getReviewThreads, {
      stuckAuto: false,
      notifier,
      metaOverrides: {
        lastAutomatedReviewFingerprint: "c1",
        lastAutomatedReviewDispatchHash: "c1",
      },
    });

    await lm.check("app-1");
    expect(lm.getStates().get("app-1")).toBe("stuck");
    // No automatic agent message; auto:false routes stuck handling to humans, so
    // the transition notify still fires.
    expect(mockSessionManager.send).not.toHaveBeenCalled();
    expect(readMetadataRaw(env.sessionsDir, "app-1")?.["stuckNudgeCount"]).toBeFalsy();
    expect(notifier.notify).toHaveBeenCalled();
  });

  // Finding #4: a backlog that is a SUBSET of a previously delivered batch (the
  // agent resolved part of it) is still "already delivered" and must be nudged.
  it("nudges a partially-addressed backlog (subset of the delivered batch)", async () => {
    // AO previously dispatched {c1,c2}; the agent resolved c1, leaving {c2}.
    const getReviewThreads = vi.fn().mockResolvedValue({ threads: [botThread("c2")], reviews: [] });
    const lm = setupStuckSession(getReviewThreads, {
      nudgeRetries: 3,
      withNotifier: true,
      metaOverrides: {
        lastAutomatedReviewFingerprint: "c1,c2",
        lastAutomatedReviewDispatchHash: "c1,c2",
      },
    });

    await lm.check("app-1");
    expect(lm.getStates().get("app-1")).toBe("stuck");
    // The remaining subset {c2} is re-delivered even though its fingerprint no
    // longer equals the prior dispatch hash "c1,c2".
    expect(mockSessionManager.send).toHaveBeenCalledTimes(1);
    const nudgeMessage = vi.mocked(mockSessionManager.send).mock.calls[0][1] as string;
    expect(nudgeMessage).toContain("comment/c2");
    expect(nudgeMessage).not.toContain("comment/c1");
    expect(readMetadataRaw(env.sessionsDir, "app-1")?.["stuckNudgeCount"]).toBe("1");
  });

  // Round-2 finding: a notify-only review reaction records a dispatch hash after
  // a human notification; the nudge must NOT turn those into an agent send.
  it("does not nudge comments whose review reaction is notify-only (opt-out preserved)", async () => {
    const notifier = createMockNotifier();
    config.reactions = {
      "agent-stuck": {
        auto: true,
        action: "notify",
        priority: "urgent",
        threshold: "1m",
        nudgeRetries: 3,
      },
      // bugbot-comments in watch/notify-only mode.
      "bugbot-comments": {
        auto: true,
        action: "notify",
        message: DEFAULT_BUGBOT_COMMENTS_MESSAGE,
      },
    };
    vi.mocked(plugins.agent.getActivityState).mockImplementation(async () => ({
      state: "idle" as ActivityState,
      timestamp: new Date(Date.now() - 120_000),
    }));
    const getReviewThreads = vi.fn().mockResolvedValue({ threads: [botThread("c1")], reviews: [] });
    const mockSCM = createMockSCM({
      getReviewThreads,
      enrichSessionsPRBatch: mockBatchEnrichment({
        state: "open",
        ciStatus: "passing",
        reviewDecision: "none",
        mergeable: false,
      }),
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
      notifier,
    });
    vi.mocked(mockSessionManager.send).mockResolvedValue(undefined);
    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makePR(), metadata: { agent: "mock-agent" } }),
      metaOverrides: {
        agent: "mock-agent",
        // A prior notify-only dispatch recorded the hash (surfaced to a human only).
        lastAutomatedReviewFingerprint: "c1",
        lastAutomatedReviewDispatchHash: "c1",
      },
      registry,
    });

    await lm.check("app-1");
    expect(lm.getStates().get("app-1")).toBe("stuck");
    // Notify-only bot comments are never re-sent to the agent...
    expect(mockSessionManager.send).not.toHaveBeenCalled();
    expect(readMetadataRaw(env.sessionsDir, "app-1")?.["stuckNudgeCount"]).toBeFalsy();
    // ...and the stuck human notify is NOT deferred (no nudge path to take over).
    expect(notifier.notify).toHaveBeenCalled();
  });

  // Round-2 finding: deferring off stale dispatch hashes must not lose the alert.
  // If the agent resolved every thread since the last fetch, the deferred stuck
  // notify must still fire (the transition suppressed it).
  it("fires the deferred stuck notify when the delivered backlog turns out resolved", async () => {
    const notifier = createMockNotifier();
    // Stale hash from a prior delivery, but every thread is now resolved (empty fetch).
    const getReviewThreads = vi.fn().mockResolvedValue({ threads: [], reviews: [] });
    const lm = setupStuckSession(getReviewThreads, {
      nudgeRetries: 3,
      notifier,
      metaOverrides: {
        lastAutomatedReviewFingerprint: "c1",
        lastAutomatedReviewDispatchHash: "c1",
      },
    });

    await lm.check("app-1");
    expect(lm.getStates().get("app-1")).toBe("stuck");
    // Nothing to nudge → no agent send, but the human IS alerted (alert not lost).
    expect(mockSessionManager.send).not.toHaveBeenCalled();
    expect(notifier.notify).toHaveBeenCalled();
    expect(readMetadataRaw(env.sessionsDir, "app-1")?.["stuckNotifiedAt"]).toBeTruthy();
  });

  // Round-2 finding: an empty but TRUNCATED page is not trustworthy as clean —
  // an unresolved thread may lie off-page, so the escalation latch must hold.
  it("fails closed on a truncated empty page (preserves the needs_input latch)", async () => {
    const getReviewThreads = vi
      .fn()
      .mockResolvedValue({ threads: [], reviews: [], threadsTruncated: true });
    const lm = setupStuckSession(getReviewThreads, {
      nudgeRetries: 3,
      withNotifier: true,
      metaOverrides: {
        stuckNudgeEscalated: "true",
        stuckNudgeCount: "3",
        stuckNudgeFingerprint: "c1",
      },
    });
    // setupCheck's writeMetadata only persists an allowlist of known keys, so the
    // escalation latch must be injected into the metadata FILE that readMetadataRaw
    // reads (the mocked sessionManager.get already carries it for determineStatus).
    updateMetadata(env.sessionsDir, "app-1", {
      stuckNudgeEscalated: "true",
      stuckNudgeCount: "3",
      stuckNudgeFingerprint: "c1",
    });

    await lm.check("app-1");
    expect(lm.getStates().get("app-1")).toBe("needs_input");
    // Truncated → do NOT declare clean; the escalation latch is preserved.
    expect(readMetadataRaw(env.sessionsDir, "app-1")?.["stuckNudgeEscalated"]).toBe("true");
  });

  // Round-3 finding (id 3516001443): when the review fetch itself fails, the nudge
  // path never runs — but the transition handler deferred the stuck notify to it.
  // The fetch-catch must still alert a human, or a stuck agent stays silent for as
  // long as review-thread fetching is broken.
  it("fires the deferred stuck notify when the review fetch fails", async () => {
    const notifier = createMockNotifier();
    const getReviewThreads = vi.fn().mockRejectedValue(new Error("rate limited"));
    const lm = setupStuckSession(getReviewThreads, { nudgeRetries: 3, notifier });

    await lm.check("app-1");
    expect(lm.getStates().get("app-1")).toBe("stuck");
    // Fetch failed → nothing to nudge, but the human IS alerted (not left silent).
    expect(mockSessionManager.send).not.toHaveBeenCalled();
    expect(notifier.notify).toHaveBeenCalled();
    expect(readMetadataRaw(env.sessionsDir, "app-1")?.["stuckNotifiedAt"]).toBeTruthy();
  });

  // Round-3 finding (id 3516001448): a truncated empty page cannot be trusted as
  // clean (fail closed on latch clearing) but a stuck agent must still be alerted —
  // otherwise a stuck agent on a large PR gets neither a nudge nor a human alert.
  it("notifies a stuck agent on a truncated empty page (cannot nudge, must alert)", async () => {
    const notifier = createMockNotifier();
    const getReviewThreads = vi
      .fn()
      .mockResolvedValue({ threads: [], reviews: [], threadsTruncated: true });
    const lm = setupStuckSession(getReviewThreads, { nudgeRetries: 3, notifier });

    await lm.check("app-1");
    expect(lm.getStates().get("app-1")).toBe("stuck");
    // Page too incomplete to nudge or declare clean → still alert the human.
    expect(mockSessionManager.send).not.toHaveBeenCalled();
    expect(notifier.notify).toHaveBeenCalled();
    expect(readMetadataRaw(env.sessionsDir, "app-1")?.["stuckNotifiedAt"]).toBeTruthy();
  });

  // Round-3 finding (id 3516001451): when the nudge send keeps failing (broken
  // runtime send path), the budget isn't consumed and escalation is never reached.
  // The send-catch must alert a human so the session isn't retried silently forever.
  it("alerts a human when the nudge send keeps failing", async () => {
    const notifier = createMockNotifier();
    const getReviewThreads = vi.fn().mockResolvedValue({ threads: [botThread("c1")], reviews: [] });
    const lm = setupStuckSession(getReviewThreads, {
      nudgeRetries: 3,
      notifier,
      // Comment already delivered so the nudge path (not bugbot dispatch) runs it.
      metaOverrides: {
        lastAutomatedReviewFingerprint: "c1",
        lastAutomatedReviewDispatchHash: "c1",
      },
    });
    // The runtime send path is broken.
    vi.mocked(mockSessionManager.send).mockRejectedValue(new Error("pane is dead"));

    await lm.check("app-1");
    expect(lm.getStates().get("app-1")).toBe("stuck");
    // Send failed → agent not engaged; a human is alerted and the budget is NOT spent.
    expect(mockSessionManager.send).toHaveBeenCalledTimes(1);
    expect(notifier.notify).toHaveBeenCalled();
    expect(readMetadataRaw(env.sessionsDir, "app-1")?.["stuckNotifiedAt"]).toBeTruthy();
    expect(readMetadataRaw(env.sessionsDir, "app-1")?.["stuckNudgeCount"]).toBeFalsy();
  });

  // Round-3 finding (id 3516065043): honoring auto:false must not skip cleanup of a
  // latch persisted from a prior auto-enabled config — otherwise determineStatus
  // keeps parking the resolved PR in needs_input forever.
  it("clears a persisted nudge latch under auto:false once the backlog is clean", async () => {
    const notifier = createMockNotifier();
    const getReviewThreads = vi.fn().mockResolvedValue({ threads: [], reviews: [] });
    const lm = setupStuckSession(getReviewThreads, {
      stuckAuto: false,
      notifier,
      metaOverrides: {
        stuckNudgeEscalated: "true",
        stuckNudgeCount: "3",
        stuckNudgeFingerprint: "c1",
      },
    });
    // setupCheck's writeMetadata only persists an allowlist, so inject the latch
    // directly into the metadata FILE that readMetadataRaw reads.
    updateMetadata(env.sessionsDir, "app-1", {
      stuckNudgeEscalated: "true",
      stuckNudgeCount: "3",
      stuckNudgeFingerprint: "c1",
    });

    await lm.check("app-1");
    // auto:false skips sends, but the stale latch must still clear now that the
    // backlog is confirmably clean (empty + not truncated).
    expect(mockSessionManager.send).not.toHaveBeenCalled();
    const meta = readMetadataRaw(env.sessionsDir, "app-1");
    expect(meta?.["stuckNudgeEscalated"]).toBeFalsy();
    expect(meta?.["stuckNudgeCount"]).toBeFalsy();
    expect(meta?.["stuckNudgeFingerprint"]).toBeFalsy();
  });
});

describe("summary pinning", () => {
  it("pins first quality summary when pinnedSummary not set", async () => {
    const session = makeSession({
      status: "working",
      agentInfo: {
        summary: "Implementing authentication flow",
        summaryIsFallback: false,
        agentSessionId: "abc",
      },
      metadata: {},
    });
    const lm = setupCheck("app-1", { session });

    await lm.check("app-1");

    const meta = readMetadataRaw(env.sessionsDir, "app-1");
    expect(meta!["pinnedSummary"]).toBe("Implementing authentication flow");
  });

  it("skips pinning when summaryIsFallback is true", async () => {
    const session = makeSession({
      status: "working",
      agentInfo: {
        summary: "You are working on issue #42...",
        summaryIsFallback: true,
        agentSessionId: "abc",
      },
      metadata: {},
    });
    const lm = setupCheck("app-1", { session });

    await lm.check("app-1");

    const meta = readMetadataRaw(env.sessionsDir, "app-1");
    expect(meta!["pinnedSummary"]).toBeUndefined();
  });

  it("skips pinning when pinnedSummary already exists", async () => {
    const session = makeSession({
      status: "working",
      agentInfo: {
        summary: "New summary that should not overwrite",
        summaryIsFallback: false,
        agentSessionId: "abc",
      },
      metadata: { pinnedSummary: "Original pinned summary" },
    });
    const lm = setupCheck("app-1", {
      session,
      metaOverrides: { pinnedSummary: "Original pinned summary" },
    });

    await lm.check("app-1");

    const meta = readMetadataRaw(env.sessionsDir, "app-1");
    expect(meta!["pinnedSummary"]).toBe("Original pinned summary");
  });

  it("skips pinning when trimmed summary is shorter than 5 chars", async () => {
    const session = makeSession({
      status: "working",
      agentInfo: {
        summary: "  Hi ",
        summaryIsFallback: false,
        agentSessionId: "abc",
      },
      metadata: {},
    });
    const lm = setupCheck("app-1", { session });

    await lm.check("app-1");

    const meta = readMetadataRaw(env.sessionsDir, "app-1");
    expect(meta!["pinnedSummary"]).toBeUndefined();
  });

  it("does not throw when metadata write fails", async () => {
    const session = makeSession({
      status: "working",
      agentInfo: {
        summary: "Valid summary for pinning",
        summaryIsFallback: false,
        agentSessionId: "abc",
      },
      metadata: {},
    });
    // Use a config with invalid path to trigger write failure
    const badConfig = {
      ...config,
      projects: {
        "my-app": {
          ...config.projects["my-app"],
          path: "/nonexistent/path/that/does/not/exist",
        },
      },
    };
    const lm = setupCheck("app-1", { session, configOverride: badConfig });

    // Should not throw — error is swallowed
    await expect(lm.check("app-1")).resolves.not.toThrow();
  });
});

describe("auto-cleanup on merge (#1309)", () => {
  function mergedScm() {
    return createMockSCM({
      getPRState: vi.fn().mockResolvedValue("merged"),
      enrichSessionsPRBatch: mockBatchEnrichment({ state: "merged", ciStatus: "none" }),
    });
  }

  function configWithLifecycle(
    overrides: Partial<{ autoCleanupOnMerge: boolean; mergeCleanupIdleGraceMs: number }>,
  ): OrchestratorConfig {
    return {
      ...config,
      lifecycle: {
        autoCleanupOnMerge: overrides.autoCleanupOnMerge ?? true,
        mergeCleanupIdleGraceMs: overrides.mergeCleanupIdleGraceMs ?? 300_000,
      },
    };
  }

  it("kills session with reason=pr_merged when PR merges and agent is idle", async () => {
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mergedScm(),
    });
    const lm = setupCheck("app-1", {
      session: makeSession({ status: "approved", pr: makePR(), activity: "idle" }),
      registry,
      configOverride: configWithLifecycle({}),
    });

    await lm.check("app-1");

    expect(mockSessionManager.kill).toHaveBeenCalledWith("app-1", {
      purgeOpenCode: true,
      reason: "pr_merged",
    });
  });

  it("defers cleanup when agent is still active and records pending marker", async () => {
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mergedScm(),
    });
    const lm = setupCheck("app-1", {
      session: makeSession({ status: "approved", pr: makePR(), activity: "active" }),
      registry,
      configOverride: configWithLifecycle({}),
    });

    await lm.check("app-1");

    expect(mockSessionManager.kill).not.toHaveBeenCalled();
    const meta = readMetadataRaw(env.sessionsDir, "app-1");
    expect(meta?.["mergedPendingCleanupSince"]).toMatch(/\d{4}-\d{2}-\d{2}T/);
    expect(meta?.["status"]).toBe("merged");
  });

  it("forces cleanup after grace window elapses even if agent is still active", async () => {
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mergedScm(),
    });
    const pendingSince = new Date(Date.now() - 10 * 60_000).toISOString(); // 10min ago
    const lm = setupCheck("app-1", {
      session: makeSession({
        status: "approved",
        pr: makePR(),
        activity: "active",
        metadata: { mergedPendingCleanupSince: pendingSince },
      }),
      registry,
      configOverride: configWithLifecycle({ mergeCleanupIdleGraceMs: 300_000 }),
      metaOverrides: { mergedPendingCleanupSince: pendingSince },
    });

    await lm.check("app-1");

    expect(mockSessionManager.kill).toHaveBeenCalledWith("app-1", {
      purgeOpenCode: true,
      reason: "pr_merged",
    });
  });

  it("does not trigger cleanup when autoCleanupOnMerge is disabled", async () => {
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mergedScm(),
    });
    const lm = setupCheck("app-1", {
      session: makeSession({ status: "approved", pr: makePR(), activity: "idle" }),
      registry,
      configOverride: configWithLifecycle({ autoCleanupOnMerge: false }),
    });

    await lm.check("app-1");

    expect(mockSessionManager.kill).not.toHaveBeenCalled();
    expect(lm.getStates().get("app-1")).toBe("merged");
  });

  it("merges a partial per-project lifecycle override over the top-level (keeps cleanup disabled)", async () => {
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mergedScm(),
    });
    // Top-level disables cleanup; the project overrides ONLY the grace window.
    // The field-merge must keep autoCleanupOnMerge:false rather than letting the
    // partial override re-enable cleanup.
    const base = configWithLifecycle({ autoCleanupOnMerge: false });
    const configOverride: OrchestratorConfig = {
      ...base,
      projects: {
        ...base.projects,
        "my-app": { ...base.projects["my-app"], lifecycle: { mergeCleanupIdleGraceMs: 600_000 } },
      },
    };
    const lm = setupCheck("app-1", {
      session: makeSession({ status: "approved", pr: makePR(), activity: "idle" }),
      registry,
      configOverride,
    });

    await lm.check("app-1");

    expect(mockSessionManager.kill).not.toHaveBeenCalled();
    expect(lm.getStates().get("app-1")).toBe("merged");
  });

  it("does not trigger cleanup for terminated/killed sessions (no self-recursion)", async () => {
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
    });
    const lm = setupCheck("app-1", {
      session: makeSession({ status: "killed", activity: "exited" }),
      registry,
      configOverride: configWithLifecycle({}),
    });

    await lm.check("app-1");

    expect(mockSessionManager.kill).not.toHaveBeenCalled();
  });

  it("retains merged status when kill() fails so the next poll retries", async () => {
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mergedScm(),
    });
    vi.mocked(mockSessionManager.kill).mockRejectedValueOnce(new Error("tmux busy"));
    const lm = setupCheck("app-1", {
      session: makeSession({ status: "approved", pr: makePR(), activity: "idle" }),
      registry,
      configOverride: configWithLifecycle({}),
    });

    await lm.check("app-1");

    expect(mockSessionManager.kill).toHaveBeenCalledTimes(1);
    const meta = readMetadataRaw(env.sessionsDir, "app-1");
    expect(meta?.["status"]).toBe("merged");
    expect(meta?.["mergedPendingCleanupSince"]).toMatch(/\d{4}-\d{2}-\d{2}T/);
  });
});

describe("event enrichment", () => {
  it("includes PR context in event data when session has PR", async () => {
    const notifier = createMockNotifier();
    const mockSCM = createMockSCM({ getPRState: vi.fn().mockResolvedValue("closed") });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
      notifier,
    });

    const session = makeSession({
      status: "pr_open",
      pr: makePR({ number: 42, url: "https://github.com/org/repo/pull/42" }),
      branch: "feat/test-123",
    });
    const lm = setupCheck("app-1", {
      session,
      registry,
      configOverride: {
        ...config,
        notificationRouting: {
          ...config.notificationRouting,
          info: ["desktop"],
        },
      },
    });

    await lm.check("app-1");

    expect(notifier.notify).toHaveBeenCalledWith(
      expect.objectContaining({
        type: "pr.closed",
        data: expect.objectContaining({
          schemaVersion: 3,
          subject: expect.objectContaining({
            pr: expect.objectContaining({
              url: "https://github.com/org/repo/pull/42",
              number: 42,
            }),
            branch: "feat/test-123",
          }),
          transition: expect.objectContaining({
            kind: "pr_state",
            from: "none",
            to: "closed",
          }),
        }),
      }),
    );
  });

  it("includes issue context in event data when session has issue", async () => {
    const notifier = createMockNotifier();
    const mockSCM = createMockSCM({ getPRState: vi.fn().mockResolvedValue("closed") });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
      notifier,
    });

    const session = makeSession({
      status: "pr_open",
      pr: makePR(),
      issueId: "INT-123",
      metadata: { issueTitle: "Fix login bug" },
    });
    const lm = setupCheck("app-1", {
      session,
      registry,
      configOverride: {
        ...config,
        notificationRouting: {
          ...config.notificationRouting,
          info: ["desktop"],
        },
      },
    });

    await lm.check("app-1");

    expect(notifier.notify).toHaveBeenCalledWith(
      expect.objectContaining({
        type: "pr.closed",
        data: expect.objectContaining({
          schemaVersion: 3,
          subject: expect.objectContaining({
            issue: {
              id: "INT-123",
              title: "Fix login bug",
            },
          }),
        }),
      }),
    );
  });

  it("gracefully omits PR context when session has no PR", async () => {
    const notifier = createMockNotifier();
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      notifier,
    });

    // Create a session without PR that will transition to needs_input
    const session = makeSession({
      status: "working",
      pr: null,
      issueId: "INT-456",
    });
    // Mock activity detection to return waiting_input
    vi.mocked(plugins.agent.getActivityState).mockResolvedValue({
      state: "waiting_input",
      timestamp: new Date(),
    });

    const lm = setupCheck("app-1", {
      session,
      registry,
      configOverride: {
        ...config,
        notificationRouting: {
          ...config.notificationRouting,
          urgent: ["desktop"],
        },
      },
    });

    await lm.check("app-1");

    expect(notifier.notify).toHaveBeenCalledWith(
      expect.objectContaining({
        type: "session.needs_input",
        data: expect.objectContaining({
          schemaVersion: 3,
          subject: expect.objectContaining({
            issue: { id: "INT-456" },
          }),
          transition: expect.objectContaining({
            kind: "session_status",
            from: "working",
            to: "needs_input",
          }),
        }),
      }),
    );
  });
});

// ---------------------------------------------------------------------------
// Multi-PR state machine aggregation (issue #1821)
// ---------------------------------------------------------------------------

describe("multi-PR state machine aggregation", () => {
  /** Batch enrichment mock returning different data per PR key. */
  function mockBatchEnrichmentPerPR(
    perPR: Record<
      string,
      { state?: string; ciStatus?: string; reviewDecision?: string; mergeable?: boolean }
    >,
  ) {
    return vi.fn().mockImplementation(async (prs: PRInfo[]) => {
      const result = new Map();
      for (const p of prs) {
        const key = `${p.owner}/${p.repo}#${p.number}`;
        const data = perPR[key] ?? {};
        result.set(key, {
          state: data.state ?? "open",
          ciStatus: data.ciStatus ?? "passing",
          reviewDecision: data.reviewDecision ?? "none",
          mergeable: data.mergeable ?? false,
        });
      }
      return result;
    });
  }

  it("2.1 — session stays open when only one of two PRs is merged", async () => {
    vi.useFakeTimers();
    try {
      const pr10 = makeMatchingPR({ number: 10, url: "https://github.com/org/my-app/pull/10" });
      const pr11 = makeMatchingPR({ number: 11, url: "https://github.com/org/my-app/pull/11" });
      const mockSCM = createMockSCM({
        enrichSessionsPRBatch: mockBatchEnrichmentPerPR({
          "org/my-app#10": { state: "merged" },
          "org/my-app#11": { state: "open", ciStatus: "passing", reviewDecision: "approved" },
        }),
      });
      const registry = createMockRegistry({
        runtime: plugins.runtime,
        agent: plugins.agent,
        scm: mockSCM,
      });
      const session = makeSession({ status: "pr_open", pr: pr10, prs: [pr10, pr11] });

      const lm = setupPollCheck("app-1", { session, registry });
      lm.start(60_000);
      await vi.advanceTimersByTimeAsync(0);
      lm.stop();

      expect(lm.getStates().get("app-1")).not.toBe("merged");
    } finally {
      vi.useRealTimers();
    }
  });

  it("2.1b — enrichment metadata uses unique PRs and deletes duplicate-index orphans", async () => {
    vi.useFakeTimers();
    try {
      const pr10 = makeMatchingPR({ number: 10, url: "https://github.com/org/my-app/pull/10" });
      const mockSCM = createMockSCM({
        enrichSessionsPRBatch: mockBatchEnrichmentPerPR({
          "org/my-app#10": { state: "open", ciStatus: "passing", reviewDecision: "none" },
        }),
      });
      const registry = createMockRegistry({
        runtime: plugins.runtime,
        agent: plugins.agent,
        scm: mockSCM,
      });
      const session = makeSession({
        id: "app-1",
        status: "pr_open",
        pr: pr10,
        prs: [pr10, { ...pr10 }],
        metadata: {
          prEnrichment_1: "{\"state\":\"open\"}",
          prReviewComments_1: "{\"unresolvedThreads\":0}",
        },
      });

      const lm = setupPollCheck("app-1", {
        session,
        registry,
        metaOverrides: {
          pr: pr10.url,
          prs: `${pr10.url},${pr10.url}`,
          prEnrichment_1: "{\"state\":\"open\"}",
          prReviewComments_1: "{\"unresolvedThreads\":0}",
        },
      });
      updateMetadata(env.sessionsDir, "app-1", {
        prEnrichment_1: "{\"state\":\"open\"}",
        prReviewComments_1: "{\"unresolvedThreads\":0}",
      });

      lm.start(60_000);
      await vi.advanceTimersByTimeAsync(0);
      lm.stop();

      const metadata = readMetadataRaw(env.sessionsDir, "app-1");
      expect(metadata?.["prEnrichment"]).toBeDefined();
      expect(metadata?.["prEnrichment_1"]).toBeUndefined();
      expect(metadata?.["prReviewComments_1"]).toBeUndefined();
    } finally {
      vi.useRealTimers();
    }
  });

  it("2.2 — session merges when ALL PRs are merged", async () => {
    vi.useFakeTimers();
    try {
      const pr10 = makeMatchingPR({ number: 10, url: "https://github.com/org/my-app/pull/10" });
      const pr11 = makeMatchingPR({ number: 11, url: "https://github.com/org/my-app/pull/11" });
      const mockSCM = createMockSCM({
        enrichSessionsPRBatch: mockBatchEnrichmentPerPR({
          "org/my-app#10": { state: "merged" },
          "org/my-app#11": { state: "merged" },
        }),
      });
      const registry = createMockRegistry({
        runtime: plugins.runtime,
        agent: plugins.agent,
        scm: mockSCM,
      });
      const session = makeSession({ status: "pr_open", pr: pr10, prs: [pr10, pr11] });

      const lm = setupPollCheck("app-1", { session, registry });
      lm.start(60_000);
      await vi.advanceTimersByTimeAsync(0);
      lm.stop();

      expect(lm.getStates().get("app-1")).toBe("merged");
    } finally {
      vi.useRealTimers();
    }
  });

  it("2.3 — ci_failed if ANY PR has failing CI", async () => {
    vi.useFakeTimers();
    try {
      const pr10 = makeMatchingPR({ number: 10, url: "https://github.com/org/my-app/pull/10" });
      const pr11 = makeMatchingPR({ number: 11, url: "https://github.com/org/my-app/pull/11" });
      const mockSCM = createMockSCM({
        enrichSessionsPRBatch: mockBatchEnrichmentPerPR({
          "org/my-app#10": { state: "open", ciStatus: "passing", reviewDecision: "approved" },
          "org/my-app#11": { state: "open", ciStatus: "failing" },
        }),
      });
      const registry = createMockRegistry({
        runtime: plugins.runtime,
        agent: plugins.agent,
        scm: mockSCM,
      });
      const session = makeSession({ status: "pr_open", pr: pr10, prs: [pr10, pr11] });

      const lm = setupPollCheck("app-1", { session, registry });
      lm.start(60_000);
      await vi.advanceTimersByTimeAsync(0);
      lm.stop();

      expect(lm.getStates().get("app-1")).toBe("ci_failed");
    } finally {
      vi.useRealTimers();
    }
  });

  it("2.4 — review_pending when not all PRs are approved", async () => {
    vi.useFakeTimers();
    try {
      const pr10 = makeMatchingPR({ number: 10, url: "https://github.com/org/my-app/pull/10" });
      const pr11 = makeMatchingPR({ number: 11, url: "https://github.com/org/my-app/pull/11" });
      const mockSCM = createMockSCM({
        enrichSessionsPRBatch: mockBatchEnrichmentPerPR({
          "org/my-app#10": { state: "open", ciStatus: "passing", reviewDecision: "approved" },
          "org/my-app#11": { state: "open", ciStatus: "passing", reviewDecision: "pending" },
        }),
      });
      const registry = createMockRegistry({
        runtime: plugins.runtime,
        agent: plugins.agent,
        scm: mockSCM,
      });
      const session = makeSession({ status: "pr_open", pr: pr10, prs: [pr10, pr11] });

      const lm = setupPollCheck("app-1", { session, registry });
      lm.start(60_000);
      await vi.advanceTimersByTimeAsync(0);
      lm.stop();

      const state = lm.getStates().get("app-1");
      expect(state).not.toBe("merged");
      expect(state).toBe("review_pending");
    } finally {
      vi.useRealTimers();
    }
  });

  it("2.5 — single PR session still merges correctly (backwards compat)", async () => {
    vi.useFakeTimers();
    try {
      const pr10 = makeMatchingPR({ number: 10, url: "https://github.com/org/my-app/pull/10" });
      const mockSCM = createMockSCM({
        enrichSessionsPRBatch: mockBatchEnrichment({ state: "merged", ciStatus: "none" }),
      });
      const registry = createMockRegistry({
        runtime: plugins.runtime,
        agent: plugins.agent,
        scm: mockSCM,
      });
      const session = makeSession({ status: "pr_open", pr: pr10 });

      const lm = setupPollCheck("app-1", { session, registry });
      lm.start(60_000);
      await vi.advanceTimersByTimeAsync(0);
      lm.stop();

      expect(lm.getStates().get("app-1")).toBe("merged");
    } finally {
      vi.useRealTimers();
    }
  });
});

describe("dependency scheduler (#10)", () => {
  function makeHeld(id: string, blockedBy: string[], issueId: string) {
    const held = makeSession({ id, status: "spawning", issueId, branch: `feat/${id}` });
    held.lifecycle.session.reason = "blocked_by_dependency";
    held.blockedBy = blockedBy;
    held.workspacePath = null;
    held.runtimeHandle = null;
    return held;
  }

  it("launches a held session once its prerequisite PR has merged", async () => {
    vi.useFakeTimers();
    try {
      const merged = makeSession({ id: "app-1", status: "merged", issueId: "back" });
      const held = makeHeld("app-2", ["back"], "front");

      vi.mocked(mockSessionManager.list).mockResolvedValue([merged, held]);
      const unblockSpy = vi.mocked(mockSessionManager.unblock);
      unblockSpy.mockResolvedValue(makeSession({ id: "app-2", status: "working" }));

      const lm = createLifecycleManager({
        config,
        registry: mockRegistry,
        sessionManager: mockSessionManager,
      });
      lm.start(60_000);
      await vi.advanceTimersByTimeAsync(0);
      lm.stop();

      expect(unblockSpy).toHaveBeenCalledWith("app-2");
    } finally {
      vi.useRealTimers();
    }
  });

  it("keeps a multi-prerequisite dependent blocked until all prerequisites merge", async () => {
    vi.useFakeTimers();
    try {
      const merged = makeSession({ id: "app-1", status: "merged", issueId: "back-a" });
      const stillOpen = makeSession({ id: "app-3", status: "working", issueId: "back-b" });
      const held = makeHeld("app-2", ["back-a", "back-b"], "front");

      vi.mocked(mockSessionManager.list).mockResolvedValue([merged, stillOpen, held]);
      const unblockSpy = vi.mocked(mockSessionManager.unblock);

      const lm = createLifecycleManager({
        config,
        registry: mockRegistry,
        sessionManager: mockSessionManager,
      });
      lm.start(60_000);
      await vi.advanceTimersByTimeAsync(0);
      lm.stop();

      expect(unblockSpy).not.toHaveBeenCalled();
    } finally {
      vi.useRealTimers();
    }
  });

  it("defers launch when the project concurrency cap is reached", async () => {
    vi.useFakeTimers();
    try {
      const baseProject = config.projects["my-app"];
      if (!baseProject) throw new Error("expected my-app project");
      const capConfig: OrchestratorConfig = {
        ...config,
        projects: { "my-app": { ...baseProject, maxConcurrent: 1 } },
      };
      const merged = makeSession({ id: "app-1", status: "merged", issueId: "back" });
      const active = makeSession({ id: "app-3", status: "working" });
      const held = makeHeld("app-2", ["back"], "front");

      vi.mocked(mockSessionManager.list).mockResolvedValue([merged, active, held]);
      const unblockSpy = vi.mocked(mockSessionManager.unblock);

      const lm = createLifecycleManager({
        config: capConfig,
        registry: mockRegistry,
        sessionManager: mockSessionManager,
      });
      lm.start(60_000);
      await vi.advanceTimersByTimeAsync(0);
      lm.stop();

      expect(unblockSpy).not.toHaveBeenCalled();
    } finally {
      vi.useRealTimers();
    }
  });

  /** A config with the extra `front`/`back` projects the cross-project tests use. */
  function multiProjectConfig(): OrchestratorConfig {
    const baseProject = config.projects["my-app"];
    if (!baseProject) throw new Error("expected my-app project");
    return {
      ...config,
      projects: {
        "my-app": baseProject,
        front: { ...baseProject },
        back: { ...baseProject },
      },
    };
  }

  it("unblocks a dependent using a merged prerequisite in another project (unscoped scheduling)", async () => {
    vi.useFakeTimers();
    try {
      // backend merged in project `back`; frontend held in project `front`,
      // blocked by the backend *session id* (the cross-project handle).
      const mergedBack = makeSession({ id: "back-1", status: "merged", issueId: "b" });
      mergedBack.projectId = "back";
      const heldFront = makeHeld("front-1", ["back-1"], "f");
      heldFront.projectId = "front";

      vi.mocked(mockSessionManager.list).mockImplementation(async (projectId?: string) => {
        // A project-scoped worker's own list only sees its project; the
        // scheduler must consult the UNSCOPED list to see the merge.
        if (projectId === "front") return [heldFront];
        return [mergedBack, heldFront];
      });
      const unblockSpy = vi.mocked(mockSessionManager.unblock);
      unblockSpy.mockResolvedValue(makeSession({ id: "front-1", status: "working" }));

      const lm = createLifecycleManager({
        config: multiProjectConfig(),
        registry: mockRegistry,
        sessionManager: mockSessionManager,
        projectId: "front",
      });
      lm.start(60_000);
      await vi.advanceTimersByTimeAsync(0);
      lm.stop();

      expect(unblockSpy).toHaveBeenCalledWith("front-1");
    } finally {
      vi.useRealTimers();
    }
  });

  it("does not unblock a held dependent owned by another project (scope isolation)", async () => {
    vi.useFakeTimers();
    try {
      // The held dependent belongs to `back`; this worker is scoped to `front`,
      // so it must leave the `back` dependent to the `back` worker — preventing
      // two project workers from racing to launch the same session.
      const mergedBack = makeSession({ id: "back-1", status: "merged", issueId: "b" });
      mergedBack.projectId = "back";
      const heldBack = makeHeld("back-2", ["back-1"], "f2");
      heldBack.projectId = "back";

      vi.mocked(mockSessionManager.list).mockImplementation(async (projectId?: string) => {
        if (projectId === "front") return [];
        return [mergedBack, heldBack];
      });
      const unblockSpy = vi.mocked(mockSessionManager.unblock);

      const lm = createLifecycleManager({
        config: multiProjectConfig(),
        registry: mockRegistry,
        sessionManager: mockSessionManager,
        projectId: "front",
      });
      lm.start(60_000);
      await vi.advanceTimersByTimeAsync(0);
      lm.stop();

      expect(unblockSpy).not.toHaveBeenCalled();
    } finally {
      vi.useRealTimers();
    }
  });
});
