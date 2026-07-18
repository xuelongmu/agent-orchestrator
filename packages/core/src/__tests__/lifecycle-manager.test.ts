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
import {
  SessionInputPendingError,
  type OrchestratorConfig,
  type PluginRegistry,
  type OpenCodeSessionManager,
  type Agent,
  type ActivityState,
  type SessionStatus,
  type SessionMetadata,
  type PRInfo,
  type ReviewComment,
  type ReviewReaction,
  type ReviewThreadsResult,
  type SCM,
  type Notifier,
  type NotifyAction,
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
    expect(plugins.runtime.interrupt).toHaveBeenCalledWith(expect.objectContaining({ id: "rt-1" }));
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

    expect(plugins.runtime.interrupt).toHaveBeenCalledWith(expect.objectContaining({ id: "rt-1" }));
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
    expect(plugins.runtime.interrupt).toHaveBeenCalledWith(expect.objectContaining({ id: "rt-1" }));
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
    expect(plugins.runtime.interrupt).toHaveBeenCalledWith(expect.objectContaining({ id: "rt-1" }));
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

  it("still delivers the needs_input alert when callback action lookup fails", async () => {
    // Buttons enhance the alert; they are never a precondition for it. The action
    // lookup re-reads the session for its decision identity and runs OUTSIDE the
    // per-notifier delivery try blocks, so an unguarded rejection there would lose
    // the human alert entirely instead of just its buttons (#13 review).
    const previousSecret = process.env.AO_NOTIFY_CALLBACK_SECRET;
    process.env.AO_NOTIFY_CALLBACK_SECRET = "lifecycle-fallback-secret";
    vi.mocked(plugins.agent.getActivityState).mockResolvedValue({ state: "waiting_input" });

    const notifier = createMockNotifier();
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      notifier,
    });

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "working" }),
      registry,
      configOverride: {
        ...config,
        // session.needs_input infers "urgent" (inferPriority), so route that.
        notificationRouting: { ...config.notificationRouting, urgent: ["desktop"] },
      },
    });

    // Read back the session setupCheck configured by INVOKING the configured mock
    // (setupCheck only arms it, so its results list is empty and must not be read).
    // Then re-arm: check() resolves the session first, and the NEXT lookup is the
    // action mint — the one that must degrade rather than throw.
    const loaded = await mockSessionManager.get("app-1");
    expect(loaded).toBeTruthy();
    vi.mocked(mockSessionManager.get).mockReset();
    vi.mocked(mockSessionManager.get)
      .mockResolvedValueOnce(loaded)
      .mockRejectedValue(new Error("enrichment failed"));

    try {
      // Without the guard the rejection escapes notifyHuman and no notifier is
      // ever reached, so "notify was called at all" is the regression.
      await expect(lm.check("app-1")).resolves.toBeUndefined();
      expect(notifier.notify).toHaveBeenCalledWith(
        expect.objectContaining({ type: "session.needs_input" }),
      );
    } finally {
      // Assigning undefined would store the STRING "undefined", which
      // getNotifyCallbackSecret reads as a perfectly valid secret — silently
      // enabling callbacks for every later test in this file.
      if (previousSecret === undefined) delete process.env.AO_NOTIFY_CALLBACK_SECRET;
      else process.env.AO_NOTIFY_CALLBACK_SECRET = previousSecret;
    }
  });

  it("keeps the View PR link on a PR-only notification when the decision lookup would reject", async () => {
    // review.changes_requested / merge.ready carry only the unsigned View PR link,
    // so notifyHuman must NOT run the needs-input nonce lookup for them: a get()
    // rejection (e.g. an OpenCode session still awaiting discovery) would otherwise
    // strip their read-only link purely because callbacks are enabled. (#13 review)
    const previousSecret = process.env.AO_NOTIFY_CALLBACK_SECRET;
    process.env.AO_NOTIFY_CALLBACK_SECRET = "pr-only-secret";

    const notify = vi.fn().mockResolvedValue(undefined);
    const notifyWithActions = vi.fn().mockResolvedValue(undefined);
    const notifier = { name: "desktop", notify, notifyWithActions } as unknown as Notifier;
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
      notifier,
    });

    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makeMatchingPR() }),
      registry,
      configOverride: {
        ...config,
        reactions: {},
        // merge.ready infers the "action" priority (inferPriority).
        notificationRouting: { ...config.notificationRouting, action: ["desktop"] },
      },
    });

    // check() loads the session first; the needs-input nonce lookup would be the
    // NEXT get(). Arm it to reject so a regression (running the lookup for a PR-only
    // event) is caught: it would drop the View PR action.
    const loaded = await mockSessionManager.get("app-1");
    vi.mocked(mockSessionManager.get).mockReset();
    vi.mocked(mockSessionManager.get)
      .mockResolvedValueOnce(loaded)
      .mockRejectedValue(new Error("enrichment failed"));

    try {
      await expect(lm.check("app-1")).resolves.toBeUndefined();
      expect(lm.getStates().get("app-1")).toBe("mergeable");
      const delivered = notifyWithActions.mock.calls.flatMap(
        (c) => (c[1] as NotifyAction[] | undefined) ?? [],
      );
      expect(delivered.some((a) => a.label === "View PR" && !!a.url)).toBe(true);
    } finally {
      if (previousSecret === undefined) delete process.env.AO_NOTIFY_CALLBACK_SECRET;
      else process.env.AO_NOTIFY_CALLBACK_SECRET = previousSecret;
    }
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
    expect(meta?.["prBaseBranch"]).toBe(makePR().baseBranch);
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
      getPRState: vi
        .fn()
        .mockImplementation((pr: PRInfo) =>
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

  describe("decision re-notify when a new report changes the nonce (no reaction configured)", () => {
    const runAudit = async (
      reportState: "needs_input" | "needs_decision",
      newReportAt: string,
      priorTriggerAt: string,
      extraMeta: Record<string, string> = {},
    ) => {
      const notifier = createMockNotifier();
      const registryWithNotifier = createMockRegistry({
        runtime: plugins.runtime,
        agent: plugins.agent,
        notifier,
      });
      vi.mocked(plugins.agent.getActivityState).mockResolvedValue({ state: "waiting_input" });

      const priorState = reportState === "needs_decision" ? "needs_decision" : "needs_input";
      // A single lm.check() runs one poll → one auditAndReactToReports, the
      // deterministic path (no timer interplay). The session is already parked in
      // needs_input, so there is NO status transition — only the report-watcher
      // fallback can produce a notification here.
      const lm = setupCheck("app-1", {
        session: makeSession({
          status: "needs_input",
          activity: "waiting_input",
          createdAt: new Date("2025-01-01T11:40:00.000Z"),
          metadata: {
            agent: "mock-agent",
            createdAt: "2025-01-01T11:40:00.000Z",
            agentReportedState: reportState,
            agentReportedAt: newReportAt,
            agentAcknowledgedAt: priorTriggerAt,
            reportWatcherActiveTrigger: `agent_needs_input:${priorState}:${priorTriggerAt}:answerable`,
            reportWatcherTriggerActivatedAt: priorTriggerAt,
            reportWatcherTriggerCount: "1",
            notifyDecisionEpisodeAt: "2025-01-01T11:50:05.000Z",
            ...extraMeta,
          },
        }),
        registry: registryWithNotifier,
        configOverride: {
          ...config,
          reactions: {}, // no report-needs-input reaction: the fallback owns the ping
          notificationRouting: { ...config.notificationRouting, urgent: ["desktop"] },
        },
      });

      await lm.check("app-1");

      return vi.mocked(notifier.notify).mock.calls.filter((call) => {
        const event = call[0] as { type?: string; data?: { semanticType?: string } };
        return (
          event?.type === "reaction.triggered" && event?.data?.semanticType === "report.needs_input"
        );
      });
    };

    let previousSecret: string | undefined;
    beforeEach(() => {
      previousSecret = process.env.AO_NOTIFY_CALLBACK_SECRET;
      process.env.AO_NOTIFY_CALLBACK_SECRET = "re-notify-secret";
    });
    afterEach(() => {
      if (previousSecret === undefined) delete process.env.AO_NOTIFY_CALLBACK_SECRET;
      else process.env.AO_NOTIFY_CALLBACK_SECRET = previousSecret;
    });

    it("re-notifies for a SECOND needs_input report (new nonce)", async () => {
      const notifies = await runAudit(
        "needs_input",
        "2025-01-01T11:55:00.000Z", // new report
        "2025-01-01T11:50:00.000Z", // prior activation
      );
      expect(notifies.length).toBeGreaterThanOrEqual(1);
    });

    it("does not re-fire for the SAME needs_input report", async () => {
      const sameTs = "2025-01-01T11:50:00.000Z";
      const notifies = await runAudit("needs_input", sameTs, sameTs);
      expect(notifies).toHaveLength(0);
    });

    it("emits ONE actionable replacement when the SAME report becomes answerable, then no dup (3 polls)", async () => {
      // Finding 3 (#13 review) — answerability edge. A needs_input report maps the
      // canonical lifecycle to needs_input while the CURRENT native signal is still
      // active, so the first poll's alert correctly carries no controls
      // (activeDecisionId reads the live activitySignal, not the stale persisted
      // session.activity). When the SAME report first reads waiting_input the
      // activation identity flips unanswerable → answerable and exactly one
      // actionable replacement is emitted; a third poll at the same answerable
      // identity must NOT duplicate it.
      const reportAt = "2025-01-01T11:55:00.000Z";
      const cap = capturingNotifier();
      const registry = createMockRegistry({
        runtime: plugins.runtime,
        agent: plugins.agent,
        notifier: cap.plugin,
      });

      const lm = setupCheck("app-1", {
        session: makeSession({
          status: "needs_input",
          activity: "waiting_input",
          metadata: {
            agent: "mock-agent",
            agentReportedState: "needs_input",
            agentReportedAt: reportAt,
            // Prior activation was already recorded as UNANSWERABLE, so poll 1
            // (native still active) is not a new trigger and stays silent.
            reportWatcherActiveTrigger: `agent_needs_input:needs_input:${reportAt}:unanswerable`,
            reportWatcherTriggerActivatedAt: reportAt,
            reportWatcherTriggerCount: "1",
            notifyDecisionEpisodeAt: "2025-01-01T11:50:05.000Z",
          },
        }),
        registry,
        configOverride: {
          ...config,
          reactions: {}, // no report-needs-input reaction: the fallback owns the ping
          notificationRouting: { ...config.notificationRouting, urgent: ["desktop"] },
        },
      });

      // Poll 1: native still active → unanswerable, identity unchanged → silent.
      vi.mocked(plugins.agent.getActivityState).mockResolvedValue({ state: "active" });
      await lm.check("app-1");
      expect(cap.mutatingButtons()).toHaveLength(0);

      // Poll 2: same report, native now waiting_input → answerable → ONE replacement.
      vi.mocked(plugins.agent.getActivityState).mockResolvedValue({ state: "waiting_input" });
      await lm.check("app-1");
      expect(cap.mutatingButtons().map((a) => a.label)).toEqual([
        "Approve",
        "Deny",
        "Nudge",
        "Kill",
      ]);

      // Poll 3: same answerable identity → no duplicate (still exactly one set).
      await lm.check("app-1");
      expect(cap.mutatingButtons().map((a) => a.label)).toEqual([
        "Approve",
        "Deny",
        "Nudge",
        "Kill",
      ]);
    });

    it("still re-notifies for a second needs_decision report (behavior intact)", async () => {
      const notifies = await runAudit(
        "needs_decision",
        "2025-01-01T11:55:00.000Z",
        "2025-01-01T11:50:00.000Z",
        { agentReportedQuestion: "Ship it?", agentReportedConfidence: "0.6" },
      );
      expect(notifies.length).toBeGreaterThanOrEqual(1);
    });

    it("retries a same-state needs_decision notification that no notifier accepted", async () => {
      const notifier = createMockNotifier();
      vi.mocked(notifier.notify)
        .mockRejectedValueOnce(new Error("desktop unavailable"))
        .mockResolvedValue(undefined);
      const registry = createMockRegistry({
        runtime: plugins.runtime,
        agent: plugins.agent,
        notifier,
      });
      vi.mocked(plugins.agent.getActivityState).mockResolvedValue({ state: "waiting_input" });

      const reportAt = "2025-01-01T11:55:00.000Z";
      const priorAt = "2025-01-01T11:50:00.000Z";
      const lm = setupCheck("app-1", {
        session: makeSession({
          status: "needs_input",
          activity: "waiting_input",
          metadata: {
            agent: "mock-agent",
            agentReportedState: "needs_decision",
            agentReportedAt: reportAt,
            agentReportedQuestion: "Ship it?",
            agentReportedConfidence: "0.6",
            reportWatcherActiveTrigger: `agent_needs_input:needs_decision:${priorAt}:answerable`,
            reportWatcherTriggerActivatedAt: priorAt,
            reportWatcherTriggerCount: "1",
            notifyDecisionEpisodeAt: "2025-01-01T11:50:05.000Z",
          },
        }),
        registry,
        configOverride: {
          ...config,
          reactions: {},
          notificationRouting: { ...config.notificationRouting, urgent: ["desktop"] },
        },
      });

      await lm.check("app-1");
      await lm.check("app-1");

      const decisionNotifications = vi.mocked(notifier.notify).mock.calls.filter((call) => {
        const event = call[0] as { type?: string; data?: { semanticType?: string } };
        return (
          event?.type === "reaction.triggered" && event?.data?.semanticType === "report.needs_input"
        );
      });
      expect(decisionNotifications).toHaveLength(2);
      expect(
        readMetadataRaw(env.sessionsDir, "app-1")?.["decisionNotificationPending"],
      ).toBeFalsy();
    });

    it("re-notifies for the FIRST report that lands after a bare-prompt transition", async () => {
      // The session is already parked (a prior poll's bare-prompt transition) with
      // NO active trigger yet; the first report arrives with no transition this
      // poll, so the fallback is the only path that can deliver its controls.
      const notifies = await runAudit("needs_input", "2025-01-01T11:55:00.000Z", "", {
        reportWatcherActiveTrigger: "",
        reportWatcherTriggerActivatedAt: "",
        reportWatcherTriggerCount: "",
      });
      expect(notifies.length).toBeGreaterThanOrEqual(1);
    });

    const capturingNotifier = () => {
      const notify = vi.fn().mockResolvedValue(undefined);
      const notifyWithActions = vi.fn().mockResolvedValue(undefined);
      return {
        notify,
        notifyWithActions,
        plugin: {
          name: "desktop",
          resolvesActionCallbacks: true,
          notify,
          notifyWithActions,
        } as unknown as Notifier,
        mutatingButtons: () =>
          notifyWithActions.mock.calls
            .flatMap((c) => (c[1] as NotifyAction[] | undefined) ?? [])
            .filter((a) => a.callbackEndpoint),
      };
    };

    const decisionSession = (reportAt: string, priorAt: string) =>
      makeSession({
        status: "needs_input",
        activity: "waiting_input",
        metadata: {
          agent: "mock-agent",
          agentReportedState: "needs_input",
          agentReportedAt: reportAt,
          reportWatcherActiveTrigger: `agent_needs_input:needs_input:${priorAt}`,
          reportWatcherTriggerActivatedAt: priorAt,
          reportWatcherTriggerCount: "1",
          notifyDecisionEpisodeAt: "2025-01-01T11:50:05.000Z",
        },
      });

    it("omits mutating buttons when a superseding report changes the identity during delivery (B→C)", async () => {
      // The event content is built from report B, but a superseding report C lands
      // before the notify guard's async get(). The buttons must NOT be minted with
      // C's identity — a human must not resolve C from B's alert. (#13 review)
      const cap = capturingNotifier();
      const registry = createMockRegistry({
        runtime: plugins.runtime,
        agent: plugins.agent,
        notifier: cap.plugin,
      });
      vi.mocked(plugins.agent.getActivityState).mockResolvedValue({ state: "waiting_input" });

      const lm = setupCheck("app-1", {
        session: decisionSession("2025-01-01T11:55:00.000Z", "2025-01-01T11:50:00.000Z"),
        registry,
        configOverride: {
          ...config,
          reactions: {},
          notificationRouting: { ...config.notificationRouting, urgent: ["desktop"] },
        },
      });

      // The poll session B carries the persisted fields setupCheck merges in; C is
      // the same session with a superseding report timestamp (a different
      // decision identity).
      const sessionB = (await mockSessionManager.get("app-1"))!;
      const sessionC = {
        ...sessionB,
        metadata: { ...sessionB.metadata, agentReportedAt: "2025-01-01T11:59:00.000Z" },
      };
      // check() reads B (builds the event + expectedDecisionId); the notify guard's
      // fresh get() sees the superseding C.
      vi.mocked(mockSessionManager.get).mockReset();
      vi.mocked(mockSessionManager.get).mockResolvedValueOnce(sessionB).mockResolvedValue(sessionC);

      await lm.check("app-1");

      expect(cap.mutatingButtons()).toHaveLength(0);
      // The alert itself still went out.
      expect(
        cap.notify.mock.calls.length + cap.notifyWithActions.mock.calls.length,
      ).toBeGreaterThanOrEqual(1);
    });

    it("mints mutating buttons when the identity still matches at delivery (B==B)", async () => {
      const cap = capturingNotifier();
      const registry = createMockRegistry({
        runtime: plugins.runtime,
        agent: plugins.agent,
        notifier: cap.plugin,
      });
      vi.mocked(plugins.agent.getActivityState).mockResolvedValue({ state: "waiting_input" });

      const sessionB = decisionSession("2025-01-01T11:55:00.000Z", "2025-01-01T11:50:00.000Z");
      const lm = setupCheck("app-1", {
        session: sessionB,
        registry,
        configOverride: {
          ...config,
          reactions: {},
          notificationRouting: { ...config.notificationRouting, urgent: ["desktop"] },
        },
      });
      // Both reads see B: identity matches, so buttons mint.
      vi.mocked(mockSessionManager.get).mockResolvedValue(sessionB);

      await lm.check("app-1");

      expect(cap.mutatingButtons().map((a) => a.label)).toEqual([
        "Approve",
        "Deny",
        "Nudge",
        "Kill",
      ]);
    });

    it("does not duplicate when THIS poll's transition carried the controls", async () => {
      // The report parks the session THIS poll (working → needs_input): the status
      // transition notification carries the buttons, so the report fallback must
      // stay silent (no report.needs_input reaction.triggered).
      const notifier = createMockNotifier();
      const registry = createMockRegistry({
        runtime: plugins.runtime,
        agent: plugins.agent,
        notifier,
      });
      vi.mocked(plugins.agent.getActivityState).mockResolvedValue({ state: "waiting_input" });

      const lm = setupCheck("app-1", {
        session: makeSession({
          status: "working", // transitions to needs_input this poll
          activity: "waiting_input",
          metadata: {
            agent: "mock-agent",
            agentReportedState: "needs_input",
            agentReportedAt: "2025-01-01T11:55:00.000Z",
            notifyDecisionEpisodeAt: "2025-01-01T11:50:05.000Z",
          },
        }),
        registry,
        configOverride: {
          ...config,
          reactions: {},
          notificationRouting: { ...config.notificationRouting, urgent: ["desktop"] },
        },
      });

      await lm.check("app-1");

      const reportReNotifies = vi.mocked(notifier.notify).mock.calls.filter((call) => {
        const event = call[0] as { type?: string; data?: { semanticType?: string } };
        return (
          event?.type === "reaction.triggered" && event?.data?.semanticType === "report.needs_input"
        );
      });
      expect(reportReNotifies).toHaveLength(0);
    });

    // Finding 1 (#13 review): when a `report-needs-input` reaction IS configured,
    // the report fallback stays silent and the default `executeReaction` notify
    // path owns the ping. That path must bind the mutating buttons to the SAME
    // snapshot identity, exactly like the transition and fallback callers — a
    // superseding report during notify's async get() must not repoint them.
    const reportNeedsInputReaction = (): OrchestratorConfig => ({
      ...config,
      reactions: {
        "report-needs-input": {
          action: "notify" as const,
          auto: true,
          priority: "urgent" as const,
        },
      },
      notificationRouting: { ...config.notificationRouting, urgent: ["desktop"] },
    });

    it("merges a project-only report reaction over notify defaults", async () => {
      const notifier = createMockNotifier();
      const registry = createMockRegistry({
        runtime: plugins.runtime,
        agent: plugins.agent,
        notifier,
      });
      vi.mocked(plugins.agent.getActivityState).mockResolvedValue({ state: "waiting_input" });

      const lm = setupCheck("app-1", {
        session: decisionSession("2025-01-01T11:55:00.000Z", "2025-01-01T11:50:00.000Z"),
        registry,
        configOverride: {
          ...config,
          reactions: {},
          projects: {
            ...config.projects,
            "my-app": {
              ...config.projects["my-app"],
              reactions: {
                // Runtime project reaction shape is partial, like root overrides.
                "report-needs-input": { priority: "urgent" },
              },
            },
          },
          notificationRouting: { ...config.notificationRouting, urgent: ["desktop"] },
        },
      });

      await lm.check("app-1");

      expect(notifier.notify).toHaveBeenCalledWith(
        expect.objectContaining({
          type: "reaction.triggered",
          data: expect.objectContaining({ semanticType: "report.needs_input" }),
        }),
      );
    });

    it("executeReaction omits buttons when a superseding report changes the identity during delivery (B→C)", async () => {
      const cap = capturingNotifier();
      const registry = createMockRegistry({
        runtime: plugins.runtime,
        agent: plugins.agent,
        notifier: cap.plugin,
      });
      vi.mocked(plugins.agent.getActivityState).mockResolvedValue({ state: "waiting_input" });

      const lm = setupCheck("app-1", {
        session: decisionSession("2025-01-01T11:55:00.000Z", "2025-01-01T11:50:00.000Z"),
        registry,
        configOverride: reportNeedsInputReaction(),
      });

      const sessionB = (await mockSessionManager.get("app-1"))!;
      const sessionC = {
        ...sessionB,
        metadata: { ...sessionB.metadata, agentReportedAt: "2025-01-01T11:59:00.000Z" },
      };
      // check() reads B (executeReaction builds the event + expectedDecisionId from
      // it); notify's fresh get() sees the superseding C.
      vi.mocked(mockSessionManager.get).mockReset();
      vi.mocked(mockSessionManager.get).mockResolvedValueOnce(sessionB).mockResolvedValue(sessionC);

      await lm.check("app-1");

      expect(cap.mutatingButtons()).toHaveLength(0);
      // The alert itself still went out via the reaction.
      expect(
        cap.notify.mock.calls.length + cap.notifyWithActions.mock.calls.length,
      ).toBeGreaterThanOrEqual(1);
    });

    it("executeReaction mints buttons when the identity still matches at delivery (B==B)", async () => {
      const cap = capturingNotifier();
      const registry = createMockRegistry({
        runtime: plugins.runtime,
        agent: plugins.agent,
        notifier: cap.plugin,
      });
      vi.mocked(plugins.agent.getActivityState).mockResolvedValue({ state: "waiting_input" });

      const sessionB = decisionSession("2025-01-01T11:55:00.000Z", "2025-01-01T11:50:00.000Z");
      const lm = setupCheck("app-1", {
        session: sessionB,
        registry,
        configOverride: reportNeedsInputReaction(),
      });
      vi.mocked(mockSessionManager.get).mockResolvedValue(sessionB);

      await lm.check("app-1");

      expect(cap.mutatingButtons().map((a) => a.label)).toEqual([
        "Approve",
        "Deny",
        "Nudge",
        "Kill",
      ]);
    });
  });

  describe("between-polls decision re-park (episode boundary)", () => {
    // Faithful parked-decision fixture: the session was created well before the
    // decision (a report predating createdAt reads as stale and would correctly be
    // retired), the report is FRESH but DISTINCT from the episode marker, and the
    // episode marker is the stamped boundary. The invariant under test: while the
    // lifecycle is needs_input and the native activity boundary equals the stamped
    // episode, the record survives regardless of the (distinct) report instant.
    const CREATED_AT = new Date(Date.now() - 10 * 60_000);
    const reportAt = new Date(Date.now() - 2 * 60_000).toISOString(); // fresh (<5min)
    const episodeAt = new Date(Date.now() - 90_000).toISOString(); // distinct from reportAt
    const decisionMeta = () => ({
      agent: "mock-agent",
      createdAt: CREATED_AT.toISOString(),
      agentReportedState: "needs_input",
      agentReportedAt: reportAt,
      notifyDecisionEpisodeAt: episodeAt,
    });
    const parkedSession = (activityTimestamp: Date) => {
      vi.mocked(plugins.agent.getActivityState).mockResolvedValue({
        state: "waiting_input",
        timestamp: activityTimestamp,
      });
      const lm = setupCheck("app-1", {
        session: makeSession({
          status: "needs_input",
          activity: "waiting_input",
          createdAt: CREATED_AT,
          metadata: decisionMeta(),
        }),
        metaOverrides: decisionMeta(),
      });
      // setupCheck's initial write uses the typed SessionMetadata serializer,
      // which intentionally ignores extension keys. Seed the durable decision
      // fields explicitly so this integration test exercises the locked retire
      // path instead of asserting against metadata that was never persisted.
      updateMetadata(env.sessionsDir, "app-1", decisionMeta());
      return lm;
    };

    it("retires the spent report+episode when the agent activity boundary advances while still parked", async () => {
      // The agent resolved prompt A and re-parked on an unrelated bare prompt B
      // entirely between polls: its activity boundary has advanced to now, past A's
      // marker, though both poll snapshots read waiting_input/needs_input. The spent
      // record must be retired so A's still-live token cannot resolve against B.
      const lm = parkedSession(new Date());

      await lm.check("app-1");

      const meta = readMetadataRaw(env.sessionsDir, "app-1");
      expect(meta?.["notifyDecisionEpisodeAt"] ?? "").toBe("");
      expect(meta?.["agentReportedState"] ?? "").toBe("");
    });

    it("keeps the decision live while the agent stays blocked at the prompt (boundary frozen)", async () => {
      // The agent is genuinely blocked: its activity boundary IS the stamped episode
      // instant and has not advanced, so the decision survives the poll unchanged —
      // even though the fresh report instant is distinct from the episode marker.
      const lm = parkedSession(new Date(episodeAt));

      await lm.check("app-1");

      const meta = readMetadataRaw(env.sessionsDir, "app-1");
      expect(meta?.["notifyDecisionEpisodeAt"]).toBe(episodeAt);
      expect(meta?.["agentReportedState"]).toBe("needs_input");
    });

    it("restamps the episode and KEEPS a fresh successor report B instead of retiring it", async () => {
      // Finding #4 (round 18): the agent resolved A, worked, and re-parked on a NEW
      // decision B — writing a fresh report — entirely between polls. The activity
      // boundary advanced past A's marker, but B's report (written after A's episode,
      // by the time the agent re-parked) is already in metadata. The poll must
      // restamp the episode to the advanced boundary and KEEP B, not retire it with A.
      const oldEpisode = new Date(Date.now() - 90_000).toISOString(); // A's stamp
      const boundaryB = new Date(); // agent's advanced activity boundary (past A)
      const reportBAt = new Date(Date.now() - 30_000).toISOString(); // after A's episode, before re-park
      const metaB = () => ({
        agent: "mock-agent",
        createdAt: CREATED_AT.toISOString(),
        agentReportedState: "needs_input",
        agentReportedAt: reportBAt,
        notifyDecisionEpisodeAt: oldEpisode,
      });
      vi.mocked(plugins.agent.getActivityState).mockResolvedValue({
        state: "waiting_input",
        timestamp: boundaryB,
      });
      const lm = setupCheck("app-1", {
        session: makeSession({
          status: "needs_input",
          activity: "waiting_input",
          createdAt: CREATED_AT,
          metadata: metaB(),
        }),
        metaOverrides: metaB(),
      });
      updateMetadata(env.sessionsDir, "app-1", metaB());

      await lm.check("app-1");

      const meta = readMetadataRaw(env.sessionsDir, "app-1");
      // Episode restamped to the advanced boundary — not cleared, not the old marker.
      expect(meta?.["notifyDecisionEpisodeAt"]).toBe(boundaryB.toISOString());
      // B's report survived (would have been cleared by a retire).
      expect(meta?.["agentReportedState"]).toBe("needs_input");
      expect(meta?.["agentReportedAt"]).toBe(reportBAt);
    });
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

  it("routes a pending confidence hold to notify-only when auto becomes false", async () => {
    const notifier = createMockNotifier();
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      notifier,
    });
    config.reactions = {
      "agent-idle": {
        auto: false,
        action: "send-to-agent",
        message: "Agent idle action was disabled.",
        confidenceThreshold: 0.9,
      },
    };
    vi.mocked(plugins.agent.getActivityState).mockResolvedValue({ state: "active" });

    const session = makeSession({
      status: "working",
      activity: "active",
      metadata: { agent: "mock-agent", confidenceHoldPending: "agent-idle" },
    });
    const lm = setupCheck("app-1", {
      session,
      registry,
      metaOverrides: { confidenceHoldPending: "agent-idle" },
    });
    updateMetadata(env.sessionsDir, "app-1", { confidenceHoldPending: "agent-idle" });

    await lm.check("app-1");

    expect(mockSessionManager.send).not.toHaveBeenCalled();
    expect(notifier.notify).toHaveBeenCalledWith(
      expect.objectContaining({
        type: "reaction.triggered",
        message: "Agent idle action was disabled.",
      }),
    );
    expect(readMetadataRaw(env.sessionsDir, "app-1")?.["confidenceHoldPending"]).toBeFalsy();
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

  it("does not retry an enriched review prompt while its editor input remains pending", async () => {
    config.reactions = {
      "bugbot-comments": {
        auto: true,
        action: "send-to-agent",
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
            url: "https://example.com/comment/pending",
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

    let editorPending = true;
    vi.mocked(plugins.agent.getActivityState).mockImplementation(async () => ({
      state: editorPending ? "waiting_input" : "active",
    }));
    vi.mocked(mockSessionManager.send)
      .mockRejectedValueOnce(new SessionInputPendingError("app-1", true))
      .mockResolvedValue(undefined);

    let now = Date.now();
    const nowSpy = vi.spyOn(Date, "now").mockImplementation(() => now);
    try {
      const lm = setupCheck("app-1", {
        session: makeSession({ status: "pr_open", pr: makePR() }),
        registry,
      });

      await lm.check("app-1");
      expect(mockSessionManager.send).toHaveBeenCalledTimes(1);
      let metadata = readMetadataRaw(env.sessionsDir, "app-1");
      expect(metadata?.["lifecycleInputPendingAt"]).toBeTruthy();
      expect(metadata?.["lastAutomatedReviewDispatchHash"]).toBeFalsy();

      now += 121_000;
      await lm.check("app-1");
      expect(mockSessionManager.send).toHaveBeenCalledTimes(1);

      // A verified non-waiting activity signal proves the editor was cleared;
      // the same enriched payload may now be attempted once and latched normally.
      editorPending = false;
      now += 121_000;
      await lm.check("app-1");
      expect(mockSessionManager.send).toHaveBeenCalledTimes(2);
      metadata = readMetadataRaw(env.sessionsDir, "app-1");
      expect(metadata?.["lifecycleInputPendingAt"]).toBeFalsy();
      expect(metadata?.["lastAutomatedReviewDispatchHash"]).toBe("bot-1");
    } finally {
      nowSpy.mockRestore();
    }
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

  it("suppresses direct enriched-review retries after the transition leaves input pending", async () => {
    config.reactions = {
      "changes-requested": {
        auto: true,
        action: "send-to-agent",
        message: "Handle requested changes.",
      },
    };

    const mockSCM = createMockSCM({
      getReviewDecision: vi.fn().mockResolvedValue("changes_requested"),
      enrichSessionsPRBatch: mockBatchEnrichment({ reviewDecision: "changes_requested" }),
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
            url: "https://example.com/comment/direct-pending",
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

    let editorPending = false;
    vi.mocked(plugins.agent.getActivityState).mockImplementation(async () => ({
      state: editorPending ? "waiting_input" : "active",
    }));
    let dispatch = 0;
    vi.mocked(mockSessionManager.send).mockImplementation(async () => {
      dispatch += 1;
      if (dispatch === 2) {
        editorPending = true;
        throw new SessionInputPendingError("app-1", true);
      }
    });

    let now = Date.now();
    const nowSpy = vi.spyOn(Date, "now").mockImplementation(() => now);
    try {
      const lm = setupCheck("app-1", {
        session: makeSession({ status: "pr_open", pr: makePR() }),
        registry,
      });

      await lm.check("app-1");
      expect(mockSessionManager.send).toHaveBeenCalledTimes(2);
      expect(readMetadataRaw(env.sessionsDir, "app-1")?.["lifecycleInputPendingAt"]).toBeTruthy();

      now += 121_000;
      await lm.check("app-1");
      expect(mockSessionManager.send).toHaveBeenCalledTimes(2);

      editorPending = false;
      now += 121_000;
      await lm.check("app-1");
      expect(mockSessionManager.send).toHaveBeenCalledTimes(3);
      expect(readMetadataRaw(env.sessionsDir, "app-1")?.["lastPendingReviewDispatchHash"]).toBe(
        "c1",
      );
    } finally {
      nowSpy.mockRestore();
    }
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
    expect(sentMessage).toContain(
      "Failure URL: https://github.com/org/repo/actions/runs/123/job/456",
    );
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
    let changesRequested = true;
    const mockSCM = createMockSCM({
      enrichSessionsPRBatch: batchMock,
      getReviewDecision: vi.fn().mockImplementation(async () =>
        changesRequested ? "changes_requested" : "none",
      ),
      getReviewThreads: vi.fn().mockImplementation(async () => ({
        threads: [],
        reviews: changesRequested
          ? [
              {
                author: "reviewer",
                state: "CHANGES_REQUESTED",
                body: "Required changes",
                submittedAt: new Date(),
                commitSha: "sha-head",
              },
            ]
          : [],
        headSha: "sha-head",
        threadsTruncated: false,
      })),
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
    changesRequested = false;
    vi.mocked(mockSCM.enrichSessionsPRBatch!).mockImplementation(
      mockBatchEnrichment({ ciStatus: "passing", reviewDecision: "none" }),
    );
    await lm.check("app-1");

    // Transition back — fresh tracker (attempt 1 again, NOT 2)
    changesRequested = true;
    vi.mocked(mockSCM.enrichSessionsPRBatch!).mockImplementation(
      mockBatchEnrichment({ reviewDecision: "changes_requested" }),
    );
    await lm.check("app-1");
    expect(mockSessionManager.send).toHaveBeenCalledTimes(1);

    // Transition away and back again — still attempt 1, not escalating
    changesRequested = false;
    vi.mocked(mockSCM.enrichSessionsPRBatch!).mockImplementation(
      mockBatchEnrichment({ ciStatus: "passing", reviewDecision: "none" }),
    );
    await lm.check("app-1");
    vi.mocked(mockSessionManager.send).mockClear();

    changesRequested = true;
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
    const getReviewThreads = vi
      .fn()
      .mockImplementation(async () =>
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
    const getReviewThreads = vi
      .fn()
      .mockImplementation(async () =>
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
    const getReviewThreads = vi
      .fn()
      .mockImplementation(async () =>
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
      enrichment: {
        state: "open",
        ciStatus: "passing",
        reviewDecision: "pending",
        mergeable: false,
      },
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

  // The nudge is ADDITIVE: the agent-stuck notify still fires at the stuck
  // transition (pre-#5 behavior) AND the nudge re-delivers already-delivered
  // comments — the nudge never suppresses or defers the notification.
  it("fires the agent-stuck notify AND nudges when comments were already delivered", async () => {
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
    // The transition notify fires (not deferred) AND the nudge re-delivers the comment.
    expect(notifier.notify).toHaveBeenCalled();
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

  // Round-7 finding (id 3521898190): when a review reaction ESCALATES,
  // executeReaction records the dispatch hash even though nothing reached the
  // agent. The nudge must NOT treat that as a delivered backlog and re-send it.
  it("does not nudge comments whose review reaction has escalated", async () => {
    // Fresh bot comment (no prior dispatch hash) so the dispatch block runs
    // executeReaction and — with retries:0 — escalates on the first attempt.
    const getReviewThreads = vi.fn().mockResolvedValue({ threads: [botThread("c1")], reviews: [] });
    const lm = setupStuckSession(getReviewThreads, { nudgeRetries: 3, withNotifier: true });
    config.reactions = {
      ...config.reactions,
      "bugbot-comments": {
        auto: true,
        action: "send-to-agent",
        retries: 0,
        message: DEFAULT_BUGBOT_COMMENTS_MESSAGE,
      },
    };

    // Poll 1: bugbot dispatch escalates (retries:0) — it records the dispatch hash
    // but escalates without sending anything to the agent.
    await lm.check("app-1");
    expect(lm.getStates().get("app-1")).toBe("stuck");
    expect(mockSessionManager.send).not.toHaveBeenCalled();
    expect(readMetadataRaw(env.sessionsDir, "app-1")?.["lastAutomatedReviewDispatchHash"]).toBe(
      "c1",
    );

    // Poll 2: the recorded hash looks "delivered", but the reaction is escalated, so
    // the nudge must NOT re-send it to the agent (would fight the human handoff).
    await vi.advanceTimersByTimeAsync(THROTTLE_STEP_MS);
    await lm.check("app-1");
    expect(mockSessionManager.send).not.toHaveBeenCalled();
    expect(readMetadataRaw(env.sessionsDir, "app-1")?.["stuckNudgeCount"]).toBeFalsy();
  });

  // #12: a CONFIDENCE-held review send (distinct from a retry escalation) reaches
  // the agent with nothing — so the enriched comments must be surfaced to the
  // HUMAN instead, never silently dropped.
  it("surfaces a confidence-HELD review send to the human instead of the agent", async () => {
    const notifier = createMockNotifier();
    const getReviewThreads = vi.fn().mockResolvedValue({ threads: [botThread("c1")], reviews: [] });
    // High cumulative churn drops computeConfidence below the 0.9 threshold, so the
    // bugbot send-to-agent reaction is HELD rather than delivered to the agent.
    const lm = setupStuckSession(getReviewThreads, {
      nudgeRetries: 3,
      notifier,
      metaOverrides: { reviewRoundCountTotal: "5" },
    });
    config.reactions = {
      ...config.reactions,
      "bugbot-comments": {
        auto: true,
        action: "send-to-agent",
        message: DEFAULT_BUGBOT_COMMENTS_MESSAGE,
        confidenceThreshold: 0.9,
      },
    };
    updateMetadata(env.sessionsDir, "app-1", { reviewRoundCountTotal: "5" });

    await lm.check("app-1");
    // Held from the agent, but surfaced to the human as ONE actionable escalation:
    // the confidence question plus the enriched review comments.
    expect(mockSessionManager.send).not.toHaveBeenCalled();
    const escalation = vi
      .mocked(notifier.notify)
      .mock.calls.find((call) =>
        String((call[0] as { message?: string }).message ?? "").includes(
          "Proceed manually or intervene",
        ),
      );
    expect(escalation).toBeDefined();
    // The enriched bot comment travels with the escalation.
    expect(String((escalation![0] as { message?: string }).message)).toContain("Potential issue");
    // A human-only confidence escalation is not an agent review→fix round.
    const meta = readMetadataRaw(env.sessionsDir, "app-1");
    expect(meta?.["reviewRoundCount"]).toBeFalsy();
    expect(meta?.["reviewRoundCountTotal"]).toBe("5");
  });

  // Round-7 finding (id 3521898192): the agent-stuck notify is no longer deferred,
  // so it fires immediately at the stuck transition — never delayed behind the
  // review-thread throttle.
  it("fires the agent-stuck notify immediately at the stuck transition (no throttle delay)", async () => {
    const notifier = createMockNotifier();
    // A recent successful fetch stamps the throttle so maybeDispatchReviewBacklog
    // would be throttled this cycle — the transition notify must fire regardless.
    const getReviewThreads = vi.fn().mockResolvedValue({ threads: [], reviews: [] });
    const lm = setupStuckSession(getReviewThreads, { nudgeRetries: 3, notifier });

    await lm.check("app-1");
    expect(lm.getStates().get("app-1")).toBe("stuck");
    // The agent-stuck reaction fired at the transition (pre-#5 behavior restored).
    expect(notifier.notify).toHaveBeenCalled();
  });

  // Round-7 finding (id 3521898193): a clean PR overlay state (review_pending) with
  // an EMPTY backlog must NOT fire an agent-stuck alert — there is no stuck
  // transition, and the deferred empty-backlog alert has been removed.
  it("does not alert stuck on a clean overlay state with an empty backlog", async () => {
    const notifier = createMockNotifier();
    const getReviewThreads = vi.fn().mockResolvedValue({ threads: [], reviews: [] });
    // reviewDecision "pending" → review_pending overlay; the agent is idle-beyond-
    // threshold but never surfaces as `stuck`, so no agent-stuck transition occurs.
    const lm = setupStuckSession(getReviewThreads, {
      nudgeRetries: 3,
      notifier,
      enrichment: {
        state: "open",
        ciStatus: "passing",
        reviewDecision: "pending",
        mergeable: false,
      },
    });

    // Poll 1 settles into review_pending (a review.pending transition notify may fire).
    await lm.check("app-1");
    expect(lm.getStates().get("app-1")).toBe("review_pending");

    // Poll 2 has no transition, so the ONLY notify that could fire is the (removed)
    // deferred empty-backlog stuck alert — which must not happen on a clean overlay.
    vi.mocked(notifier.notify).mockClear();
    await vi.advanceTimersByTimeAsync(THROTTLE_STEP_MS);
    await lm.check("app-1");
    expect(lm.getStates().get("app-1")).toBe("review_pending");
    expect(notifier.notify).not.toHaveBeenCalled();
    expect(mockSessionManager.send).not.toHaveBeenCalled();
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

describe("merge DoD + per-bot policy + flaky CI (#15)", () => {
  function qualityConfig(): OrchestratorConfig {
    return {
      ...config,
      reactions: {
        ...config.reactions,
        "bugbot-comments": {
          auto: true,
          action: "send-to-agent",
          reviewBots: {
            "chatgpt-codex-connector[bot]": {
              weight: 1,
              approvalPhrases: ["Didn't find any major issues"],
              approvalReactions: ["THUMBS_UP"],
              rebumpMessage: "@codex review",
              maxRebumps: 2,
              rebumpBackoff: "30m",
            },
            "*": { weight: 0.25 },
          },
        },
        "approved-and-green": {
          auto: true,
          action: "auto-merge",
          confidenceThreshold: 0,
        },
      },
    };
  }

  function codexApprovalResult(
    threads: ReviewComment[] = [],
    body = "Didn't find any major issues. Chef's kiss.",
    reactions: ReviewReaction[] = [],
    headPushedAt: Date | null = new Date("2026-07-18T00:00:00Z"),
  ): ReviewThreadsResult {
    return {
      threads,
      reviews: [
        {
          author: "chatgpt-codex-connector[bot]",
          botName: "chatgpt-codex-connector[bot]",
          state: "COMMENTED",
          body,
          submittedAt: new Date(),
          isBot: true,
          isReviewBot: true,
          commitSha: "sha-head",
        },
      ],
      reactions,
      headSha: "sha-head",
      ...(headPushedAt ? { headPushedAt } : {}),
      threadsTruncated: false,
    };
  }

  it("merges only after fresh CI, conflict, thread, approval, and confidence gates pass", async () => {
    const mergePR = vi.fn().mockResolvedValue(undefined);
    const mockSCM = createMockSCM({
      mergePR,
      getPRState: vi.fn().mockResolvedValue("open"),
      getCISummary: vi.fn().mockResolvedValue("passing"),
      getReviewDecision: vi.fn().mockResolvedValue("none"),
      getMergeability: vi.fn().mockResolvedValue({
        mergeable: true,
        ciPassing: true,
        approved: false,
        noConflicts: true,
        blockers: [],
      }),
      getReviewThreads: vi.fn().mockResolvedValue(codexApprovalResult()),
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });
    const pr = makeMatchingPR();
    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr }),
      registry,
      configOverride: qualityConfig(),
    });

    await lm.check("app-1");

    expect(mergePR).toHaveBeenCalledWith(pr, undefined, "sha-head");
    expect(readMetadataRaw(env.sessionsDir, "app-1")?.["autoMergeRequestedAt"]).toBeTruthy();
  });

  it("fails closed when the fresh review snapshot has no head SHA", async () => {
    const mergePR = vi.fn().mockResolvedValue(undefined);
    const reviewData = codexApprovalResult();
    delete reviewData.headSha;
    const mockSCM = createMockSCM({
      mergePR,
      getPRState: vi.fn().mockResolvedValue("open"),
      getCISummary: vi.fn().mockResolvedValue("passing"),
      getReviewDecision: vi.fn().mockResolvedValue("none"),
      getMergeability: vi.fn().mockResolvedValue({
        mergeable: true,
        ciPassing: true,
        approved: false,
        noConflicts: true,
        blockers: [],
      }),
      getReviewThreads: vi.fn().mockResolvedValue(reviewData),
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });
    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makeMatchingPR() }),
      registry,
      configOverride: qualityConfig(),
    });

    await lm.check("app-1");

    expect(mergePR).not.toHaveBeenCalled();
    expect(readMetadataRaw(env.sessionsDir, "app-1")?.["autoMergeBlockers"]).toContain(
      "review_data_incomplete",
    );
  });

  it("deduplicates merge-failure escalation per PR head and recovers on a later attempt", async () => {
    const notifier = createMockNotifier();
    let failFirstEscalationDelivery = true;
    vi.mocked(notifier.notify).mockImplementation(async (event) => {
      if (event.type === "reaction.escalated" && failFirstEscalationDelivery) {
        failFirstEscalationDelivery = false;
        throw new Error("notifier offline");
      }
    });
    let headSha = "sha-one";
    let rejectMerge = true;
    const getReviewThreads = vi.fn().mockImplementation(
      async (): Promise<ReviewThreadsResult> => ({
        threads: [],
        reviews: [
          {
            author: "chatgpt-codex-connector[bot]",
            botName: "chatgpt-codex-connector[bot]",
            state: "COMMENTED",
            body: "Didn't find any major issues. Nice work!",
            submittedAt: new Date(),
            isBot: true,
            isReviewBot: true,
            commitSha: headSha,
          },
        ],
        headSha,
        threadsTruncated: false,
      }),
    );
    const mergePR = vi.fn().mockImplementation(async () => {
      if (rejectMerge) throw new Error("head protection race");
    });
    const mockSCM = createMockSCM({
      mergePR,
      getPRState: vi.fn().mockResolvedValue("open"),
      getCISummary: vi.fn().mockResolvedValue("passing"),
      getReviewDecision: vi.fn().mockResolvedValue("none"),
      getMergeability: vi.fn().mockResolvedValue({
        mergeable: true,
        ciPassing: true,
        approved: false,
        noConflicts: true,
        blockers: [],
      }),
      getReviewThreads,
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
      notifier,
    });
    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makeMatchingPR() }),
      registry,
      configOverride: qualityConfig(),
    });

    await lm.check("app-1");
    await lm.check("app-1");
    await lm.check("app-1");
    let escalations = vi
      .mocked(notifier.notify)
      .mock.calls.filter(([event]) => event.type === "reaction.escalated");
    expect(mergePR).toHaveBeenCalledTimes(3);
    // The failed first delivery did not latch. The second poll delivered it;
    // the third poll retried the merge without spamming another notification.
    expect(escalations).toHaveLength(2);
    expect(escalations[1]?.[0].message).toContain("PR #42");
    expect(escalations[1]?.[0].message).toContain("sha-one");

    headSha = "sha-two";
    await lm.check("app-1");
    escalations = vi
      .mocked(notifier.notify)
      .mock.calls.filter(([event]) => event.type === "reaction.escalated");
    expect(escalations).toHaveLength(3);
    expect(escalations[2]?.[0].message).toContain("sha-two");

    rejectMerge = false;
    await lm.check("app-1");
    expect(mergePR).toHaveBeenLastCalledWith(makeMatchingPR(), undefined, "sha-two");
    const metadata = readMetadataRaw(env.sessionsDir, "app-1");
    expect(metadata?.["autoMergeRequestedAt"]).toBeTruthy();
    expect(metadata?.["autoMergeFailureHeadSha:org/my-app#42"]).toBeFalsy();
    expect(metadata?.["autoMergeFailureNotifiedAt:org/my-app#42"]).toBeFalsy();
  });

  it("treats fractional bot changes requests as context when Codex approved the head", async () => {
    const codexReview = codexApprovalResult().reviews[0]!;
    const getReviewThreads = vi.fn().mockResolvedValue({
      ...codexApprovalResult(),
      reviews: [
        codexReview,
        {
          author: "coderabbitai[bot]",
          botName: "coderabbitai[bot]",
          state: "CHANGES_REQUESTED",
          body: "Optional suggestion",
          submittedAt: new Date(),
          isBot: true,
          isReviewBot: false,
          commitSha: "sha-head",
        },
      ],
    });
    const mergePR = vi.fn().mockResolvedValue(undefined);
    const postPRComment = vi.fn().mockResolvedValue(undefined);
    const mockSCM = createMockSCM({
      mergePR,
      postPRComment,
      getPRState: vi.fn().mockResolvedValue("open"),
      getCISummary: vi.fn().mockResolvedValue("passing"),
      getReviewDecision: vi.fn().mockResolvedValue("changes_requested"),
      getMergeability: vi.fn().mockResolvedValue({
        mergeable: false,
        ciPassing: true,
        approved: false,
        noConflicts: true,
        blockers: ["Changes requested in review"],
      }),
      getReviewThreads,
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });
    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makeMatchingPR() }),
      registry,
      configOverride: qualityConfig(),
    });

    await lm.check("app-1");

    expect(mockSessionManager.send).not.toHaveBeenCalled();
    expect(postPRComment).not.toHaveBeenCalled();
    expect(mergePR).toHaveBeenCalledWith(makeMatchingPR(), undefined, "sha-head");
  });

  it.each([
    {
      label: "human",
      review: {
        author: "alice",
        state: "CHANGES_REQUESTED",
        body: "Please fix this",
        submittedAt: new Date(),
        commitSha: "sha-head",
      },
    },
    {
      label: "actionable Codex",
      review: {
        author: "chatgpt-codex-connector[bot]",
        botName: "chatgpt-codex-connector[bot]",
        state: "CHANGES_REQUESTED",
        body: "Please fix this",
        submittedAt: new Date(),
        isBot: true,
        isReviewBot: true,
        commitSha: "sha-head",
      },
    },
  ])("blocks auto-merge for a $label changes request", async ({ review }) => {
    const mergePR = vi.fn().mockResolvedValue(undefined);
    const postPRComment = vi.fn().mockResolvedValue(undefined);
    const codexApproval = codexApprovalResult().reviews[0]!;
    const mockSCM = createMockSCM({
      mergePR,
      postPRComment,
      getPRState: vi.fn().mockResolvedValue("open"),
      getCISummary: vi.fn().mockResolvedValue("passing"),
      getReviewDecision: vi.fn().mockResolvedValue("changes_requested"),
      getMergeability: vi.fn().mockResolvedValue({
        mergeable: false,
        ciPassing: true,
        approved: false,
        noConflicts: true,
        blockers: ["Changes requested in review"],
      }),
      getReviewThreads: vi.fn().mockResolvedValue({
        ...codexApprovalResult(),
        reviews:
          "isReviewBot" in review && review.isReviewBot ? [review] : [codexApproval, review],
      }),
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });
    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makeMatchingPR() }),
      registry,
      configOverride: qualityConfig(),
    });

    await lm.check("app-1");

    expect(mergePR).not.toHaveBeenCalled();
    expect(postPRComment).not.toHaveBeenCalled();
    expect(readMetadataRaw(env.sessionsDir, "app-1")?.["autoMergeBlockers"]).toContain(
      "review_approval_missing",
    );
  });

  it("aggregates context-only changes requests across every PR before suppressing transition work", async () => {
    const primary = makeMatchingPR({ number: 42 });
    const secondary = makeMatchingPR({
      number: 43,
      branch: "feat/secondary",
      url: "https://github.com/org/my-app/pull/43",
    });
    const notifier = createMockNotifier();
    const mergePR = vi.fn().mockResolvedValue(undefined);
    const postPRComment = vi.fn().mockResolvedValue(undefined);
    const getReviewThreads = vi.fn().mockImplementation(async (pr: PRInfo) => {
      const headSha = pr.number === primary.number ? "sha-primary" : "sha-secondary";
      const reviews = [
        {
          author: "chatgpt-codex-connector[bot]",
          botName: "chatgpt-codex-connector[bot]",
          state: "COMMENTED",
          body: "Didn't find any major issues. Nice work!",
          submittedAt: new Date(),
          isBot: true,
          isReviewBot: true,
          commitSha: headSha,
        },
      ];
      if (pr.number === secondary.number) {
        reviews.push({
          author: "coderabbitai[bot]",
          botName: "coderabbitai[bot]",
          state: "CHANGES_REQUESTED",
          body: "Optional suggestion",
          submittedAt: new Date(),
          isBot: true,
          isReviewBot: false,
          commitSha: headSha,
        });
      }
      return { threads: [], reviews, headSha, threadsTruncated: false };
    });
    const mockSCM = createMockSCM({
      mergePR,
      postPRComment,
      getPRState: vi.fn().mockResolvedValue("open"),
      getCISummary: vi.fn().mockResolvedValue("passing"),
      getReviewDecision: vi
        .fn()
        .mockImplementation(async (pr: PRInfo) =>
          pr.number === secondary.number ? "changes_requested" : "none",
        ),
      getMergeability: vi.fn().mockImplementation(async (pr: PRInfo) => ({
        mergeable: pr.number === primary.number,
        ciPassing: true,
        approved: false,
        noConflicts: true,
        blockers: pr.number === secondary.number ? ["Changes requested in review"] : [],
      })),
      getReviewThreads,
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
      notifier,
    });
    const configOverride = qualityConfig();
    configOverride.reactions["changes-requested"] = {
      auto: true,
      action: "send-to-agent",
      message: "Address required changes.",
    };
    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: primary, prs: [primary, secondary] }),
      registry,
      configOverride,
    });

    await lm.check("app-1");

    expect(mockSessionManager.send).not.toHaveBeenCalled();
    expect(
      vi
        .mocked(notifier.notify)
        .mock.calls.filter(([event]) => event.type === "review.changes_requested"),
    ).toHaveLength(0);
    expect(postPRComment).not.toHaveBeenCalled();
    expect(mergePR).toHaveBeenNthCalledWith(1, primary, undefined, "sha-primary");
    expect(mergePR).toHaveBeenNthCalledWith(2, secondary, undefined, "sha-secondary");
  });

  it("does not suppress a required changes request on a secondary PR", async () => {
    const primary = makeMatchingPR({ number: 42 });
    const secondary = makeMatchingPR({
      number: 43,
      branch: "feat/secondary",
      url: "https://github.com/org/my-app/pull/43",
    });
    const notifier = createMockNotifier();
    const mergePR = vi.fn().mockResolvedValue(undefined);
    const getReviewThreads = vi.fn().mockImplementation(async (pr: PRInfo) => {
      const headSha = pr.number === primary.number ? "sha-primary" : "sha-secondary";
      return {
        threads: [],
        reviews:
          pr.number === secondary.number
            ? [
                {
                  author: "alice",
                  state: "CHANGES_REQUESTED",
                  body: "Required fix",
                  submittedAt: new Date(),
                  commitSha: headSha,
                },
              ]
            : [
                {
                  author: "chatgpt-codex-connector[bot]",
                  botName: "chatgpt-codex-connector[bot]",
                  state: "COMMENTED",
                  body: "Didn't find any major issues. Nice work!",
                  submittedAt: new Date(),
                  isBot: true,
                  isReviewBot: true,
                  commitSha: headSha,
                },
              ],
        headSha,
        threadsTruncated: false,
      };
    });
    const mockSCM = createMockSCM({
      mergePR,
      postPRComment: vi.fn().mockResolvedValue(undefined),
      getPRState: vi.fn().mockResolvedValue("open"),
      getCISummary: vi.fn().mockResolvedValue("passing"),
      getReviewDecision: vi
        .fn()
        .mockImplementation(async (pr: PRInfo) =>
          pr.number === secondary.number ? "changes_requested" : "none",
        ),
      getMergeability: vi.fn().mockImplementation(async (pr: PRInfo) => ({
        mergeable: pr.number === primary.number,
        ciPassing: true,
        approved: false,
        noConflicts: true,
        blockers: pr.number === secondary.number ? ["Changes requested in review"] : [],
      })),
      getReviewThreads,
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
      notifier,
    });
    const configOverride = qualityConfig();
    configOverride.reactions["changes-requested"] = {
      auto: true,
      action: "send-to-agent",
      message: "Address required changes.",
    };
    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: primary, prs: [primary, secondary] }),
      registry,
      configOverride,
    });

    await lm.check("app-1");

    const changesNotifications = vi
      .mocked(notifier.notify)
      .mock.calls.filter(([event]) => event.type === "review.changes_requested");
    expect(
      vi.mocked(mockSessionManager.send).mock.calls.length + changesNotifications.length,
    ).toBeGreaterThan(0);
    expect(mergePR).not.toHaveBeenCalled();
  });

  it("evaluates reaction-only approval while GitHub still reports review pending", async () => {
    const mergePR = vi.fn().mockResolvedValue(undefined);
    let reactionCreatedAt = new Date("2026-07-18T00:05:00Z");
    const getReviewThreads = vi.fn().mockImplementation(
      async (): Promise<ReviewThreadsResult> => ({
        threads: [],
        reviews: [],
        reactions: [
          {
            author: "chatgpt-codex-connector[bot]",
            botName: "chatgpt-codex-connector[bot]",
            content: "THUMBS_UP",
            createdAt: reactionCreatedAt,
            isBot: true,
          },
        ],
        headSha: "sha-head",
        headPushedAt: new Date("2026-07-18T00:10:00Z"),
        threadsTruncated: false,
      }),
    );
    const mockSCM = createMockSCM({
      mergePR,
      postPRComment: vi.fn().mockResolvedValue(undefined),
      getPRState: vi.fn().mockResolvedValue("open"),
      getCISummary: vi.fn().mockResolvedValue("passing"),
      getReviewDecision: vi.fn().mockResolvedValue("pending"),
      getMergeability: vi.fn().mockResolvedValue({
        mergeable: false,
        ciPassing: true,
        approved: false,
        noConflicts: true,
        blockers: ["Review required"],
      }),
      getReviewThreads,
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });
    const lm = setupCheck("app-1", {
      session: makeSession({ status: "review_pending", pr: makeMatchingPR() }),
      registry,
      configOverride: qualityConfig(),
    });

    await lm.check("app-1");
    expect(mergePR).not.toHaveBeenCalled();

    reactionCreatedAt = new Date("2026-07-18T00:11:00Z");
    await lm.check("app-1");
    expect(mergePR).toHaveBeenCalledTimes(1);
  });

  it("accepts the Nice work Codex clean-verdict variant via the stable phrase", async () => {
    const mergePR = vi.fn().mockResolvedValue(undefined);
    const mockSCM = createMockSCM({
      mergePR,
      getPRState: vi.fn().mockResolvedValue("open"),
      getCISummary: vi.fn().mockResolvedValue("passing"),
      getReviewDecision: vi.fn().mockResolvedValue("none"),
      getMergeability: vi.fn().mockResolvedValue({
        mergeable: true,
        ciPassing: true,
        approved: false,
        noConflicts: true,
        blockers: [],
      }),
      getReviewThreads: vi
        .fn()
        .mockResolvedValue(codexApprovalResult([], "Didn't find any major issues. Nice work!")),
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });
    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makeMatchingPR() }),
      registry,
      configOverride: qualityConfig(),
    });

    await lm.check("app-1");

    expect(mergePR).toHaveBeenCalledTimes(1);
  });

  it("does not treat an eyes acknowledgement reaction as Codex approval", async () => {
    const mergePR = vi.fn().mockResolvedValue(undefined);
    const mockSCM = createMockSCM({
      mergePR,
      getPRState: vi.fn().mockResolvedValue("open"),
      getCISummary: vi.fn().mockResolvedValue("passing"),
      getReviewDecision: vi.fn().mockResolvedValue("none"),
      getMergeability: vi.fn().mockResolvedValue({
        mergeable: true,
        ciPassing: true,
        approved: false,
        noConflicts: true,
        blockers: [],
      }),
      getReviewThreads: vi.fn().mockResolvedValue(
        codexApprovalResult([], "Review acknowledged.", [
          {
            author: "chatgpt-codex-connector[bot]",
            botName: "chatgpt-codex-connector[bot]",
            content: "EYES",
            createdAt: new Date("2026-07-18T00:01:00Z"),
            isBot: true,
          },
        ]),
      ),
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });
    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makeMatchingPR() }),
      registry,
      configOverride: qualityConfig(),
    });

    await lm.check("app-1");

    expect(mergePR).not.toHaveBeenCalled();
    expect(readMetadataRaw(env.sessionsDir, "app-1")?.["autoMergeBlockers"]).toContain(
      "review_approval_missing",
    );
  });

  it("accepts bot reactions only when they were created after the current head was pushed", async () => {
    let reaction: ReviewReaction = {
      author: "chatgpt-codex-connector[bot]",
      botName: "chatgpt-codex-connector[bot]",
      content: "THUMBS_UP",
      createdAt: new Date("2026-07-18T00:05:00Z"),
      isBot: true,
    };
    const getReviewThreads = vi.fn().mockImplementation(async () =>
      // The replacement commit may carry an old commit timestamp. The push
      // happened after this reaction, so it must not approve the new head.
      codexApprovalResult([], "Review acknowledged.", [reaction], new Date("2026-07-18T00:10:00Z")),
    );
    const mergePR = vi.fn().mockResolvedValue(undefined);
    const mockSCM = createMockSCM({
      mergePR,
      getPRState: vi.fn().mockResolvedValue("open"),
      getCISummary: vi.fn().mockResolvedValue("passing"),
      getReviewDecision: vi.fn().mockResolvedValue("none"),
      getMergeability: vi.fn().mockResolvedValue({
        mergeable: true,
        ciPassing: true,
        approved: false,
        noConflicts: true,
        blockers: [],
      }),
      getReviewThreads,
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });
    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makeMatchingPR() }),
      registry,
      configOverride: qualityConfig(),
    });

    await lm.check("app-1");
    expect(mergePR).not.toHaveBeenCalled();

    reaction = { ...reaction, createdAt: new Date("2026-07-18T00:11:00Z") };
    await lm.check("app-1");
    expect(mergePR).toHaveBeenCalledTimes(1);
  });

  it("uses a durable observation boundary when GitHub omits the head push time", async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-07-18T00:10:00Z"));
    try {
      let reactions: ReviewReaction[] = [
        {
          author: "chatgpt-codex-connector[bot]",
          botName: "chatgpt-codex-connector[bot]",
          content: "THUMBS_UP",
          createdAt: new Date("2026-07-18T00:05:00Z"),
          isBot: true,
        },
      ];
      const getReviewThreads = vi
        .fn()
        .mockImplementation(async () =>
          codexApprovalResult([], "Review acknowledged.", reactions, null),
        );
      const mergePR = vi.fn().mockResolvedValue(undefined);
      const mockSCM = createMockSCM({
        mergePR,
        getPRState: vi.fn().mockResolvedValue("open"),
        getCISummary: vi.fn().mockResolvedValue("passing"),
        getReviewDecision: vi.fn().mockResolvedValue("none"),
        getMergeability: vi.fn().mockResolvedValue({
          mergeable: true,
          ciPassing: true,
          approved: false,
          noConflicts: true,
          blockers: [],
        }),
        getReviewThreads,
      });
      const registry = createMockRegistry({
        runtime: plugins.runtime,
        agent: plugins.agent,
        scm: mockSCM,
      });
      const lm = setupCheck("app-1", {
        session: makeSession({ status: "pr_open", pr: makeMatchingPR() }),
        registry,
        configOverride: qualityConfig(),
      });

      await lm.check("app-1");
      expect(mergePR).not.toHaveBeenCalled();
      expect(readMetadataRaw(env.sessionsDir, "app-1")).toMatchObject({
        "approvalReactionHeadSha:org/my-app#42": "sha-head",
        "approvalReactionObservedAt:org/my-app#42": "2026-07-18T00:10:00.000Z",
      });

      await lm.check("app-1");
      expect(mergePR).not.toHaveBeenCalled();

      reactions = [{ ...reactions[0]!, createdAt: new Date("2026-07-18T00:11:00Z") }];
      vi.setSystemTime(new Date("2026-07-18T00:12:00Z"));
      await lm.check("app-1");
      expect(mergePR).toHaveBeenCalledTimes(1);
    } finally {
      vi.useRealTimers();
    }
  });

  it("resets the fallback reaction boundary when the PR head changes", async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-07-18T00:10:00Z"));
    try {
      let headSha = "sha-one";
      let reactions: ReviewReaction[] = [];
      const getReviewThreads = vi.fn().mockImplementation(async () => {
        const result = codexApprovalResult([], "Review acknowledged.", reactions, null);
        result.headSha = headSha;
        result.reviews[0]!.commitSha = headSha;
        return result;
      });
      const mergePR = vi.fn().mockResolvedValue(undefined);
      const mockSCM = createMockSCM({
        mergePR,
        getPRState: vi.fn().mockResolvedValue("open"),
        getCISummary: vi.fn().mockResolvedValue("passing"),
        getReviewDecision: vi.fn().mockResolvedValue("none"),
        getMergeability: vi.fn().mockResolvedValue({
          mergeable: true,
          ciPassing: true,
          approved: false,
          noConflicts: true,
          blockers: [],
        }),
        getReviewThreads,
      });
      const registry = createMockRegistry({
        runtime: plugins.runtime,
        agent: plugins.agent,
        scm: mockSCM,
      });
      const lm = setupCheck("app-1", {
        session: makeSession({ status: "pr_open", pr: makeMatchingPR() }),
        registry,
        configOverride: qualityConfig(),
      });

      await lm.check("app-1");

      headSha = "sha-two";
      reactions = [
        {
          author: "chatgpt-codex-connector[bot]",
          botName: "chatgpt-codex-connector[bot]",
          content: "THUMBS_UP",
          createdAt: new Date("2026-07-18T00:15:00Z"),
          isBot: true,
        },
      ];
      vi.setSystemTime(new Date("2026-07-18T00:20:00Z"));
      await lm.check("app-1");
      expect(mergePR).not.toHaveBeenCalled();
      expect(readMetadataRaw(env.sessionsDir, "app-1")).toMatchObject({
        "approvalReactionHeadSha:org/my-app#42": "sha-two",
        "approvalReactionObservedAt:org/my-app#42": "2026-07-18T00:20:00.000Z",
      });

      reactions = [{ ...reactions[0]!, createdAt: new Date("2026-07-18T00:21:00Z") }];
      vi.setSystemTime(new Date("2026-07-18T00:22:00Z"));
      await lm.check("app-1");
      expect(mergePR).toHaveBeenCalledTimes(1);
    } finally {
      vi.useRealTimers();
    }
  });

  it("requires human approval to target the current head", async () => {
    const humanApproval = (commitSha: string): ReviewThreadsResult => ({
      threads: [],
      reviews: [
        {
          author: "alice",
          state: "APPROVED",
          body: "",
          submittedAt: new Date(),
          commitSha,
        },
      ],
      headSha: "sha-head",
      threadsTruncated: false,
    });
    const getReviewThreads = vi.fn().mockResolvedValue(humanApproval("sha-stale"));
    const mergePR = vi.fn().mockResolvedValue(undefined);
    const mockSCM = createMockSCM({
      mergePR,
      getPRState: vi.fn().mockResolvedValue("open"),
      getCISummary: vi.fn().mockResolvedValue("passing"),
      getReviewDecision: vi.fn().mockResolvedValue("approved"),
      getMergeability: vi.fn().mockResolvedValue({
        mergeable: true,
        ciPassing: true,
        approved: true,
        noConflicts: true,
        blockers: [],
      }),
      getReviewThreads,
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });
    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makeMatchingPR() }),
      registry,
      configOverride: qualityConfig(),
    });

    await lm.check("app-1");
    expect(mergePR).not.toHaveBeenCalled();

    getReviewThreads.mockResolvedValue(humanApproval("sha-head"));
    await lm.check("app-1");
    expect(mergePR).toHaveBeenCalledTimes(1);
  });

  it("blocks auto-merge on live branch-protection mergeability blockers", async () => {
    const mergePR = vi.fn().mockResolvedValue(undefined);
    const mockSCM = createMockSCM({
      mergePR,
      getPRState: vi.fn().mockResolvedValue("open"),
      getCISummary: vi.fn().mockResolvedValue("passing"),
      getReviewDecision: vi.fn().mockResolvedValue("none"),
      getMergeability: vi.fn().mockResolvedValue({
        mergeable: false,
        ciPassing: true,
        approved: false,
        noConflicts: true,
        blockers: ["Branch is behind base branch"],
      }),
      getReviewThreads: vi.fn().mockResolvedValue(codexApprovalResult()),
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });
    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makeMatchingPR() }),
      registry,
      configOverride: qualityConfig(),
    });

    await lm.check("app-1");

    expect(mergePR).not.toHaveBeenCalled();
    expect(readMetadataRaw(env.sessionsDir, "app-1")?.["autoMergeBlockers"]).toContain(
      "Branch is behind base branch",
    );
  });

  it("uses live-ready draft state instead of a stale recorded draft snapshot", async () => {
    const mergePR = vi.fn().mockResolvedValue(undefined);
    const mockSCM = createMockSCM({
      mergePR,
      getPRState: vi.fn().mockResolvedValue("open"),
      getCISummary: vi.fn().mockResolvedValue("passing"),
      getReviewDecision: vi.fn().mockResolvedValue("none"),
      getMergeability: vi.fn().mockResolvedValue({
        mergeable: true,
        ciPassing: true,
        approved: false,
        noConflicts: true,
        isDraft: false,
        blockers: [],
      }),
      getReviewThreads: vi.fn().mockResolvedValue(codexApprovalResult()),
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });
    const pr = makeMatchingPR({ isDraft: true });
    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr }),
      registry,
      configOverride: qualityConfig(),
    });

    await lm.check("app-1");

    expect(mergePR).toHaveBeenCalledWith(pr, undefined, "sha-head");
  });

  it("blocks auto-merge when fresh mergeability reports a live draft", async () => {
    const mergePR = vi.fn().mockResolvedValue(undefined);
    const mockSCM = createMockSCM({
      mergePR,
      getPRState: vi.fn().mockResolvedValue("open"),
      getCISummary: vi.fn().mockResolvedValue("passing"),
      getReviewDecision: vi.fn().mockResolvedValue("none"),
      getMergeability: vi.fn().mockResolvedValue({
        mergeable: false,
        ciPassing: true,
        approved: false,
        noConflicts: true,
        isDraft: true,
        blockers: ["PR is still a draft"],
      }),
      getReviewThreads: vi.fn().mockResolvedValue(codexApprovalResult()),
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });
    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makeMatchingPR({ isDraft: false }) }),
      registry,
      configOverride: qualityConfig(),
    });

    await lm.check("app-1");

    expect(mergePR).not.toHaveBeenCalled();
    expect(readMetadataRaw(env.sessionsDir, "app-1")?.["autoMergeBlockers"]).toContain("draft_pr");
  });

  it("re-bumps the specific secondary PR whose current head lacks approval", async () => {
    const primary = makeMatchingPR({ number: 42 });
    const secondary = makeMatchingPR({
      number: 43,
      branch: "feat/secondary",
      url: "https://github.com/org/my-app/pull/43",
    });
    const postPRComment = vi.fn().mockResolvedValue(undefined);
    const mockSCM = createMockSCM({
      postPRComment,
      getPRState: vi.fn().mockResolvedValue("open"),
      getCISummary: vi.fn().mockResolvedValue("passing"),
      getReviewDecision: vi.fn().mockResolvedValue("none"),
      getMergeability: vi.fn().mockResolvedValue({
        mergeable: true,
        ciPassing: true,
        approved: false,
        noConflicts: true,
        blockers: [],
      }),
      getReviewThreads: vi.fn().mockImplementation(async (pr: PRInfo) =>
        pr.number === primary.number
          ? codexApprovalResult()
          : {
              threads: [],
              reviews: [],
              headSha: "sha-secondary",
              threadsTruncated: false,
            },
      ),
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });
    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: primary, prs: [primary, secondary] }),
      registry,
      configOverride: qualityConfig(),
    });

    await lm.check("app-1");

    expect(postPRComment).toHaveBeenCalledTimes(1);
    expect(postPRComment).toHaveBeenCalledWith(secondary, "@codex review");
    const metadata = readMetadataRaw(env.sessionsDir, "app-1");
    expect(metadata?.["reviewRebumpCount:org/my-app#43"]).toBe("1");
    expect(metadata?.["reviewRebumpCount:org/my-app#42"]).toBeFalsy();
  });

  it("defers a missing-approval re-bump while required review threads remain", async () => {
    const postPRComment = vi.fn().mockResolvedValue(undefined);
    const mockSCM = createMockSCM({
      postPRComment,
      getPRState: vi.fn().mockResolvedValue("open"),
      getCISummary: vi.fn().mockResolvedValue("passing"),
      getReviewDecision: vi.fn().mockResolvedValue("pending"),
      getMergeability: vi.fn().mockResolvedValue({
        mergeable: false,
        ciPassing: true,
        approved: false,
        noConflicts: true,
        blockers: ["Review required"],
      }),
      getReviewThreads: vi.fn().mockResolvedValue({
        threads: [
          {
            id: "required-thread",
            author: "chatgpt-codex-connector[bot]",
            botName: "chatgpt-codex-connector[bot]",
            body: "Fix this first",
            isResolved: false,
            createdAt: new Date(),
            url: "https://example.test/required-thread",
            isBot: true,
            isReviewBot: true,
          },
        ],
        reviews: [],
        headSha: "sha-head",
        threadsTruncated: false,
      }),
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });
    const lm = setupCheck("app-1", {
      session: makeSession({ status: "review_pending", pr: makeMatchingPR() }),
      registry,
      configOverride: qualityConfig(),
    });

    await lm.check("app-1");

    expect(postPRComment).not.toHaveBeenCalled();
    expect(
      readMetadataRaw(env.sessionsDir, "app-1")?.["reviewRebumpCount:org/my-app#42"],
    ).toBeFalsy();
  });

  it("escalates once when the SCM cannot post a review re-bump", async () => {
    const notifier = createMockNotifier();
    const mockSCM = createMockSCM({
      getPRState: vi.fn().mockResolvedValue("open"),
      getCISummary: vi.fn().mockResolvedValue("passing"),
      getReviewDecision: vi.fn().mockResolvedValue("pending"),
      getMergeability: vi.fn().mockResolvedValue({
        mergeable: false,
        ciPassing: true,
        approved: false,
        noConflicts: true,
        blockers: ["Review required"],
      }),
      getReviewThreads: vi.fn().mockResolvedValue({
        threads: [],
        reviews: [],
        headSha: "sha-head",
        threadsTruncated: false,
      }),
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
      notifier,
    });
    const lm = setupCheck("app-1", {
      session: makeSession({ status: "review_pending", pr: makeMatchingPR() }),
      registry,
      configOverride: qualityConfig(),
    });

    await lm.check("app-1");
    await lm.check("app-1");

    const escalations = vi
      .mocked(notifier.notify)
      .mock.calls.filter(([event]) => event.type === "reaction.escalated");
    expect(escalations).toHaveLength(1);
    expect(escalations[0]?.[0].message).toContain("cannot post PR comments");
    expect(
      readMetadataRaw(env.sessionsDir, "app-1")?.["reviewApprovalEscalatedAt:org/my-app#42"],
    ).toBeTruthy();
  });

  it("escalates once when posting a review re-bump fails", async () => {
    const notifier = createMockNotifier();
    const postPRComment = vi.fn().mockRejectedValue(new Error("forbidden"));
    const mockSCM = createMockSCM({
      postPRComment,
      getPRState: vi.fn().mockResolvedValue("open"),
      getCISummary: vi.fn().mockResolvedValue("passing"),
      getReviewDecision: vi.fn().mockResolvedValue("pending"),
      getMergeability: vi.fn().mockResolvedValue({
        mergeable: false,
        ciPassing: true,
        approved: false,
        noConflicts: true,
        blockers: ["Review required"],
      }),
      getReviewThreads: vi.fn().mockResolvedValue({
        threads: [],
        reviews: [],
        headSha: "sha-head",
        threadsTruncated: false,
      }),
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
      notifier,
    });
    const lm = setupCheck("app-1", {
      session: makeSession({ status: "review_pending", pr: makeMatchingPR() }),
      registry,
      configOverride: qualityConfig(),
    });

    await lm.check("app-1");
    await lm.check("app-1");

    const escalations = vi
      .mocked(notifier.notify)
      .mock.calls.filter(([event]) => event.type === "reaction.escalated");
    expect(postPRComment).toHaveBeenCalledTimes(1);
    expect(escalations).toHaveLength(1);
    expect(escalations[0]?.[0].message).toContain("failed to post another review request");
    expect(
      readMetadataRaw(env.sessionsDir, "app-1")?.["reviewApprovalEscalatedAt:org/my-app#42"],
    ).toBeTruthy();
  });

  it("keeps fractional-weight bot threads out of required fixes and the merge gate", async () => {
    const codexThread = {
      id: "codex-1",
      author: "chatgpt-codex-connector[bot]",
      botName: "chatgpt-codex-connector[bot]",
      body: "Fix this",
      isResolved: false,
      createdAt: new Date(),
      url: "https://example.test/codex-1",
      isBot: true,
      isReviewBot: true,
    };
    const rabbitThread = {
      ...codexThread,
      id: "rabbit-1",
      author: "coderabbitai[bot]",
      botName: "coderabbitai[bot]",
      url: "https://example.test/rabbit-1",
      isReviewBot: false,
    };
    let threads = [codexThread, rabbitThread];
    const getReviewThreads = vi.fn().mockImplementation(async () => codexApprovalResult(threads));
    const mergePR = vi.fn().mockResolvedValue(undefined);
    const mockSCM = createMockSCM({
      mergePR,
      getPRState: vi.fn().mockResolvedValue("open"),
      getCISummary: vi.fn().mockResolvedValue("passing"),
      getReviewDecision: vi.fn().mockResolvedValue("none"),
      getMergeability: vi.fn().mockResolvedValue({
        mergeable: true,
        ciPassing: true,
        approved: false,
        noConflicts: true,
        blockers: [],
      }),
      getReviewThreads,
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });
    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makeMatchingPR() }),
      registry,
      configOverride: qualityConfig(),
    });

    await lm.check("app-1");
    expect(mergePR).not.toHaveBeenCalled();
    const sentMessage = vi.mocked(mockSessionManager.send).mock.calls[0]?.[1] as string;
    const [requiredSection, contextSection] = sentMessage.split(
      "### NON-BLOCKING CONTEXT (OPTIONAL)",
    );
    expect(requiredSection).toContain("@chatgpt-codex-connector[bot]");
    expect(requiredSection).not.toContain("coderabbitai");
    expect(contextSection).toContain("@coderabbitai[bot]");
    expect(contextSection).toContain("optional context only");
    expect(readMetadataRaw(env.sessionsDir, "app-1")?.["reviewRoundCount"]).toBe("1");

    threads = [rabbitThread];
    await lm.check("app-1");
    expect(mergePR).toHaveBeenCalledTimes(1);
    expect(mockSessionManager.send).toHaveBeenCalledTimes(1);
    expect(readMetadataRaw(env.sessionsDir, "app-1")?.["reviewRoundCountTotal"]).toBe("1");
  });

  it("ignores GitLab's native approval blocker after AO approval succeeds", async () => {
    const mergePR = vi.fn().mockResolvedValue(undefined);
    const mockSCM = createMockSCM({
      name: "gitlab",
      mergePR,
      getPRState: vi.fn().mockResolvedValue("open"),
      getCISummary: vi.fn().mockResolvedValue("passing"),
      getReviewDecision: vi.fn().mockResolvedValue("pending"),
      getMergeability: vi.fn().mockResolvedValue({
        mergeable: false,
        ciPassing: true,
        approved: false,
        noConflicts: true,
        blockers: ["Approval required"],
      }),
      getReviewThreads: vi.fn().mockResolvedValue(codexApprovalResult()),
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });
    const lm = setupCheck("app-1", {
      session: makeSession({ status: "review_pending", pr: makeMatchingPR() }),
      registry,
      configOverride: qualityConfig(),
    });

    await lm.check("app-1");

    expect(mergePR).toHaveBeenCalledWith(makeMatchingPR(), undefined, "sha-head");
  });

  it("retries classifier-confirmed flaky CI without consuming an agent fix round", async () => {
    const retryCI = vi.fn().mockResolvedValue(true);
    const failedChecks = [
      {
        name: "windows",
        status: "failed" as const,
        conclusion: "FAILURE",
        url: "https://github.com/org/my-app/actions/runs/10/job/20",
      },
    ];
    const mockSCM = createMockSCM({
      getCIChecks: vi.fn().mockResolvedValue(failedChecks),
      getCISummary: vi.fn().mockResolvedValue("failing"),
      getCIFailureSummary: vi.fn().mockResolvedValue({
        failedJobs: [
          {
            name: "windows",
            runUrl: failedChecks[0]!.url,
            logTail: "The hosted runner was lost (ECONNRESET)",
          },
        ],
      }),
      retryCI,
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });
    const configOverride: OrchestratorConfig = {
      ...config,
      reactions: {
        "ci-failed": {
          auto: true,
          action: "send-to-agent",
          retries: 2,
          flakyRetries: 1,
          flakyRetryBackoff: "10m",
        },
      },
    };
    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makeMatchingPR() }),
      registry,
      configOverride,
    });

    await lm.check("app-1");

    expect(retryCI).toHaveBeenCalledWith(makeMatchingPR(), failedChecks);
    expect(mockSessionManager.send).not.toHaveBeenCalled();
    const metadata = readMetadataRaw(env.sessionsDir, "app-1");
    expect(metadata?.["flakyCIRetryCount"]).toBe("1");
    expect(metadata?.["ciFailureCountTotal"]).toBeFalsy();
  });

  it("does not extend flaky retry grace to a different CI run", async () => {
    const retryCI = vi.fn().mockResolvedValue(true);
    let runId = 10;
    const getCIChecks = vi.fn().mockImplementation(async () => [
      {
        name: "windows",
        status: "failed" as const,
        conclusion: "FAILURE",
        url: `https://github.com/org/my-app/actions/runs/${runId}/job/20`,
      },
    ]);
    const mockSCM = createMockSCM({
      getCIChecks,
      getCISummary: vi.fn().mockResolvedValue("failing"),
      getCIFailureSummary: vi.fn().mockImplementation(async () => ({
        failedJobs: [
          {
            name: "windows",
            runUrl: `https://github.com/org/my-app/actions/runs/${runId}/job/20`,
            logTail: "The hosted runner was lost (ECONNRESET)",
          },
        ],
      })),
      retryCI,
    });
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: mockSCM,
    });
    const configOverride: OrchestratorConfig = {
      ...config,
      reactions: {
        "ci-failed": {
          auto: true,
          action: "send-to-agent",
          retries: 2,
          flakyRetries: 1,
          flakyRetryBackoff: "10m",
        },
      },
    };
    const lm = setupCheck("app-1", {
      session: makeSession({ status: "pr_open", pr: makeMatchingPR() }),
      registry,
      configOverride,
    });

    await lm.check("app-1");
    expect(mockSessionManager.send).not.toHaveBeenCalled();

    runId = 11;
    await lm.check("app-1");

    expect(retryCI).toHaveBeenCalledTimes(1);
    expect(mockSessionManager.send).toHaveBeenCalledTimes(1);
    expect(mockSessionManager.send).toHaveBeenCalledWith(
      "app-1",
      expect.stringContaining("CI is failing"),
    );
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

describe("stacked child retarget + rebase on parent merge (#11)", () => {
  const childPr = makeMatchingPR({
    number: 200,
    url: "https://github.com/org/my-app/pull/200",
    branch: "feat/child",
    baseBranch: "feat/parent",
  });

  function makeChild(metaExtra: Record<string, string> = {}) {
    return makeSession({
      id: "app-2",
      status: "working",
      branch: "feat/child",
      pr: childPr,
      parentSessionId: "app-1",
      metadata: { agent: "mock-agent", baseRef: "feat/parent", ...metaExtra },
    });
  }

  /** The rebase-handoff message sent to the child agent, if any. */
  function rebaseMessage(): string | undefined {
    const call = vi
      .mocked(mockSessionManager.send)
      .mock.calls.find((c) => String(c[1]).includes("git rebase --onto"));
    return call ? String(call[1]) : undefined;
  }

  function wire(opts: {
    parent: ReturnType<typeof makeSession> | null;
    scm: SCM;
    child?: ReturnType<typeof makeSession>;
  }) {
    const child = opts.child ?? makeChild();
    const registry = createMockRegistry({
      runtime: plugins.runtime,
      agent: plugins.agent,
      scm: opts.scm,
    });
    vi.mocked(mockSessionManager.get).mockImplementation(async (id: string) =>
      id === "app-1" ? opts.parent : id === "app-2" ? child : null,
    );
    vi.mocked(mockSessionManager.list).mockResolvedValue([child]);
    writeMetadata(env.sessionsDir, "app-2", {
      worktree: "/tmp",
      branch: "feat/child",
      status: "working",
      project: "my-app",
      agent: "mock-agent",
      parentSessionId: "app-1",
      ...(child.metadata["baseRef"] ? { baseRef: child.metadata["baseRef"] } : {}),
      ...(child.metadata["stackRetargetedAt"]
        ? { stackRetargetedAt: child.metadata["stackRetargetedAt"] }
        : {}),
    } as unknown as SessionMetadata);
    return createLifecycleManager({ config, registry, sessionManager: mockSessionManager });
  }

  it("retargets the child PR onto the parent's own base and hands the rebase to the agent", async () => {
    // Parent is a middle stack: pr.baseBranch is empty after metadata rebuild, so
    // the target must fall back to the parent's persisted baseRef (grandparent),
    // NOT the project default. And a base-edit alone is insufficient under the
    // default squash merge, so the agent must be told to rebase.
    const retargetPR = vi.fn().mockResolvedValue("retargeted");
    const parent = makeSession({
      id: "app-1",
      status: "merged",
      branch: "feat/parent",
      pr: makeMatchingPR({ number: 100, baseBranch: "" }),
      metadata: { agent: "mock-agent", baseRef: "feat/grandparent" },
    });
    const lm = wire({ parent, scm: createMockSCM({ retargetPR }) });

    await lm.check("app-2");

    expect(retargetPR).toHaveBeenCalledWith(childPr, "feat/grandparent", "feat/parent");
    const msg = rebaseMessage()!;
    // Resolvable upstream (local ref, then origin fallback) + rebase onto the new base.
    expect(msg).toContain('git rev-parse -q --verify "feat/parent"');
    expect(msg).toContain('git rebase --onto "origin/feat/grandparent" "$upstream" feat/child');
    const meta = readMetadataRaw(env.sessionsDir, "app-2");
    expect(meta?.["stackRetargetedAt"]).toBeTruthy();
    expect(meta?.["baseRef"]).toBe("feat/grandparent");
    expect(meta?.["prBaseBranch"]).toBe("feat/grandparent");
  });

  it("uses the parent's persisted actual PR base after metadata reload (#66)", async () => {
    const retargetPR = vi.fn().mockResolvedValue("retargeted");
    const parent = makeSession({
      id: "app-1",
      status: "merged",
      branch: "feat/parent",
      pr: makeMatchingPR({ number: 100, baseBranch: "" }),
      metadata: {
        agent: "mock-agent",
        baseRef: "feat/stale-grandparent",
        prBaseBranch: "release/actual-base",
      },
    });
    const lm = wire({ parent, scm: createMockSCM({ retargetPR }) });

    await lm.check("app-2");

    expect(retargetPR).toHaveBeenCalledWith(childPr, "release/actual-base", "feat/parent");
    expect(readMetadataRaw(env.sessionsDir, "app-2")?.["baseRef"]).toBe(
      "release/actual-base",
    );
  });

  it("uses fresh poll bases and does not overwrite a retarget with stale enrichment (#75)", async () => {
    vi.useFakeTimers();
    let lm: ReturnType<typeof createLifecycleManager> | undefined;
    try {
      const parentPr = makeMatchingPR({
        number: 100,
        url: "https://github.com/org/my-app/pull/100",
        branch: "feat/parent",
        baseBranch: "feat/stale-grandparent",
      });
      const parent = makeSession({
        id: "app-1",
        status: "approved",
        branch: "feat/parent",
        pr: parentPr,
        metadata: {
          agent: "mock-agent",
          baseRef: "feat/stale-grandparent",
          prBaseBranch: "feat/stale-grandparent",
        },
      });
      // Keep the parent eligible for this poll while representing a merge that
      // was already known before the fresh GraphQL enrichment arrived.
      parent.lifecycle.pr.state = "merged";
      parent.lifecycle.pr.reason = "merged";
      const child = makeChild();

      const retargetPR = vi.fn().mockResolvedValue("retargeted");
      const enrichSessionsPRBatch = vi.fn().mockResolvedValue(
        new Map([
          [
            "org/my-app#100",
            {
              state: "merged",
              ciStatus: "none",
              reviewDecision: "none",
              mergeable: false,
              baseBranch: "release/actual-base",
            },
          ],
          [
            "org/my-app#200",
            {
              state: "open",
              ciStatus: "passing",
              reviewDecision: "none",
              mergeable: false,
              baseBranch: "feat/parent",
            },
          ],
        ]),
      );
      const registry = createMockRegistry({
        runtime: plugins.runtime,
        agent: plugins.agent,
        scm: createMockSCM({ enrichSessionsPRBatch, retargetPR }),
      });
      vi.mocked(mockSessionManager.list).mockResolvedValue([parent, child]);
      vi.mocked(mockSessionManager.get).mockImplementation(async (id: string) =>
        id === parent.id ? parent : id === child.id ? child : null,
      );
      writeMetadata(env.sessionsDir, parent.id, {
        worktree: "/tmp/parent",
        branch: parent.branch ?? "feat/parent",
        status: parent.status,
        lifecycle: parent.lifecycle,
        project: "my-app",
        agent: "mock-agent",
        pr: parentPr.url,
        baseRef: "feat/stale-grandparent",
        prBaseBranch: "feat/stale-grandparent",
      });
      writeMetadata(env.sessionsDir, child.id, {
        worktree: "/tmp/child",
        branch: child.branch ?? "feat/child",
        status: child.status,
        lifecycle: child.lifecycle,
        project: "my-app",
        agent: "mock-agent",
        pr: childPr.url,
        parentSessionId: parent.id,
        baseRef: "feat/parent",
      });

      lm = createLifecycleManager({ config, registry, sessionManager: mockSessionManager });
      lm.start(60_000);
      await vi.advanceTimersByTimeAsync(0);
      lm.stop();

      expect(retargetPR).toHaveBeenCalledWith(
        childPr,
        "release/actual-base",
        "feat/parent",
      );
      // pollAll persists its pre-check cache after retargeting. The verified new
      // base must survive that write both durably and in the live session.
      expect(readMetadataRaw(env.sessionsDir, child.id)?.["prBaseBranch"]).toBe(
        "release/actual-base",
      );
      expect(child.pr?.baseBranch).toBe("release/actual-base");
    } finally {
      lm?.stop();
      vi.useRealTimers();
    }
  });

  it("waits (no-op) while the parent PR is still open", async () => {
    const retargetPR = vi.fn().mockResolvedValue(undefined);
    const parent = makeSession({ id: "app-1", status: "review_pending", branch: "feat/parent" });
    const lm = wire({ parent, scm: createMockSCM({ retargetPR }) });

    await lm.check("app-2");

    expect(retargetPR).not.toHaveBeenCalled();
    expect(rebaseMessage()).toBeUndefined();
    expect(readMetadataRaw(env.sessionsDir, "app-2")?.["stackRetargetedAt"]).toBeFalsy();
  });

  it("does not latch (retries) when the retarget lookup fails transiently", async () => {
    const retargetPR = vi.fn().mockRejectedValue(new Error("HTTP 503"));
    const parent = makeSession({
      id: "app-1",
      status: "merged",
      branch: "feat/parent",
      pr: makeMatchingPR({ number: 100, baseBranch: "" }),
      metadata: { agent: "mock-agent", baseRef: "feat/grandparent" },
    });
    const lm = wire({ parent, scm: createMockSCM({ retargetPR }) });

    await lm.check("app-2");

    expect(retargetPR).toHaveBeenCalled();
    // Rebase handoff not reached, and NOT latched — the next poll retries.
    expect(rebaseMessage()).toBeUndefined();
    expect(readMetadataRaw(env.sessionsDir, "app-2")?.["stackRetargetedAt"]).toBeFalsy();
  });

  it("does not fabricate an old base for a child spawned after the parent merged (#66)", async () => {
    const retargetPR = vi.fn().mockResolvedValue("retargeted");
    const parent = makeSession({
      id: "app-1",
      status: "merged",
      branch: "feat/parent",
      pr: makeMatchingPR({ number: 100, baseBranch: "main" }),
    });
    const child = makeSession({
      id: "app-2",
      status: "working",
      branch: "feat/child",
      pr: { ...childPr, baseBranch: "main" },
      parentSessionId: "app-1",
      metadata: { agent: "mock-agent" },
    });
    const lm = wire({ parent, child, scm: createMockSCM({ retargetPR }) });

    await lm.check("app-2");

    expect(retargetPR).not.toHaveBeenCalled();
    expect(rebaseMessage()).toBeUndefined();
    expect(readMetadataRaw(env.sessionsDir, "app-2")?.["stackRetargetedAt"]).toBeTruthy();
  });

  it("targets the default branch when the parent has been merged and cleaned up", async () => {
    const retargetPR = vi.fn().mockResolvedValue("retargeted");
    // parent: null → archived (merged + auto-cleaned).
    const lm = wire({ parent: null, scm: createMockSCM({ retargetPR }) });

    await lm.check("app-2");

    expect(retargetPR).toHaveBeenCalledWith(childPr, "main", "feat/parent");
    expect(rebaseMessage()).toContain('git rebase --onto "origin/main" "$upstream" feat/child');
    expect(readMetadataRaw(env.sessionsDir, "app-2")?.["baseRef"]).toBe("main");
  });

  it("does not clobber a diverged base (human moved it) and skips the rebase handoff", async () => {
    const retargetPR = vi.fn().mockResolvedValue("diverged");
    const parent = makeSession({
      id: "app-1",
      status: "merged",
      branch: "feat/parent",
      pr: makeMatchingPR({ number: 100, baseBranch: "" }),
      metadata: { agent: "mock-agent", baseRef: "feat/grandparent" },
    });
    const lm = wire({ parent, scm: createMockSCM({ retargetPR }) });

    await lm.check("app-2");

    expect(retargetPR).toHaveBeenCalled();
    // Human owns the base — no rebase handoff, and baseRef is NOT overwritten.
    expect(rebaseMessage()).toBeUndefined();
    const meta = readMetadataRaw(env.sessionsDir, "app-2");
    expect(meta?.["stackRetargetedAt"]).toBeTruthy(); // latched (surfaced once, no nag)
    expect(meta?.["baseRef"]).toBe("feat/parent"); // unchanged
  });

  it("does not latch or hand off a rebase when the child PR is not found (#66)", async () => {
    const retargetPR = vi.fn().mockResolvedValue("not_found");
    const parent = makeSession({
      id: "app-1",
      status: "merged",
      branch: "feat/parent",
      pr: makeMatchingPR({ number: 100, baseBranch: "feat/grandparent" }),
    });
    const lm = wire({ parent, scm: createMockSCM({ retargetPR }) });

    await lm.check("app-2");

    expect(retargetPR).toHaveBeenCalled();
    expect(rebaseMessage()).toBeUndefined();
    const meta = readMetadataRaw(env.sessionsDir, "app-2");
    expect(meta?.["stackRetargetedAt"]).toBeFalsy();
    expect(meta?.["baseRef"]).toBe("feat/parent");
  });

  it("notifies a stacked child that has not opened its PR yet", async () => {
    const retargetPR = vi.fn().mockResolvedValue("retargeted");
    const parent = makeSession({
      id: "app-1",
      status: "merged",
      branch: "feat/parent",
      pr: makeMatchingPR({ number: 100, baseBranch: "" }),
      metadata: { agent: "mock-agent", baseRef: "feat/grandparent" },
    });
    // Child with NO PR yet (prs === []).
    const child = makeSession({
      id: "app-2",
      status: "working",
      branch: "feat/child",
      parentSessionId: "app-1",
      metadata: { agent: "mock-agent", baseRef: "feat/parent" },
    });
    const lm = wire({ parent, scm: createMockSCM({ retargetPR }), child });

    await lm.check("app-2");

    // No PR to edit, but the agent is still told the base moved so it opens
    // against the new base instead of the deleted parent branch.
    expect(retargetPR).not.toHaveBeenCalled();
    expect(rebaseMessage()).toContain("gh pr create --base feat/grandparent");
    expect(readMetadataRaw(env.sessionsDir, "app-2")?.["baseRef"]).toBe("feat/grandparent");
  });

  it("does not latch (stays retryable) when the SCM cannot retarget a PR base", async () => {
    const parent = makeSession({
      id: "app-1",
      status: "merged",
      branch: "feat/parent",
      pr: makeMatchingPR({ number: 100, baseBranch: "" }),
      metadata: { agent: "mock-agent", baseRef: "feat/grandparent" },
    });
    // SCM without retargetPR — the child already has an open PR on the old base.
    const scm = createMockSCM(); // default mock omits the optional retargetPR
    const lm = wire({ parent, scm });

    await lm.check("app-2");

    // No false "already moved" handoff, and NOT latched as done.
    expect(rebaseMessage()).toBeUndefined();
    const meta = readMetadataRaw(env.sessionsDir, "app-2");
    expect(meta?.["stackRetargetedAt"]).toBeFalsy();
    expect(meta?.["stackRetargetUnsupportedAt"]).toBeTruthy(); // surfaced once
  });

  it("skips the handoff for a held (blocked-by-dependency) child", async () => {
    const retargetPR = vi.fn().mockResolvedValue("retargeted");
    const parent = makeSession({
      id: "app-1",
      status: "merged",
      branch: "feat/parent",
      pr: makeMatchingPR({ number: 100, baseBranch: "" }),
      metadata: { agent: "mock-agent", baseRef: "feat/grandparent" },
    });
    // Held child: blocked_by_dependency pre-state, no runtime — unblock() owns it.
    const heldChild = makeSession({
      id: "app-2",
      status: "spawning",
      branch: "feat/child",
      pr: childPr,
      parentSessionId: "app-1",
      metadata: { agent: "mock-agent", baseRef: "feat/parent" },
    });
    heldChild.lifecycle.session.state = "not_started";
    heldChild.lifecycle.session.reason = "blocked_by_dependency";
    const lm = wire({ parent, scm: createMockSCM({ retargetPR }), child: heldChild });

    await lm.check("app-2");

    expect(retargetPR).not.toHaveBeenCalled();
    expect(mockSessionManager.send).not.toHaveBeenCalled();
  });

  it("is idempotent — already-retargeted children are skipped", async () => {
    const retargetPR = vi.fn().mockResolvedValue("retargeted");
    const parent = makeSession({
      id: "app-1",
      status: "merged",
      branch: "feat/parent",
      pr: makeMatchingPR({ number: 100 }),
    });
    const lm = wire({
      parent,
      scm: createMockSCM({ retargetPR }),
      child: makeChild({ stackRetargetedAt: "2026-01-01T00:00:00.000Z" }),
    });

    await lm.check("app-2");

    expect(retargetPR).not.toHaveBeenCalled();
    expect(rebaseMessage()).toBeUndefined();
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
          prEnrichment_1: '{"state":"open"}',
          prReviewComments_1: '{"unresolvedThreads":0}',
        },
      });

      const lm = setupPollCheck("app-1", {
        session,
        registry,
        metaOverrides: {
          pr: pr10.url,
          prs: `${pr10.url},${pr10.url}`,
          prEnrichment_1: '{"state":"open"}',
          prReviewComments_1: '{"unresolvedThreads":0}',
        },
      });
      updateMetadata(env.sessionsDir, "app-1", {
        prEnrichment_1: '{"state":"open"}',
        prReviewComments_1: '{"unresolvedThreads":0}',
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
