import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { _testUtils, type GhTraceEntry } from "../gh-trace.js";
import { createLifecycleManager } from "../lifecycle-manager.js";
import type { OpenCodeSessionManager, OrchestratorConfig, PluginRegistry } from "../types.js";

describe("lifecycle GitHub rate-limit backoff", () => {
  const now = Date.parse("2026-01-01T00:00:00.000Z");

  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(now);
    _testUtils.resetGithubRateLimits();
  });

  afterEach(() => {
    _testUtils.resetGithubRateLimits();
    vi.useRealTimers();
  });

  it("widens the next poll instead of using the normal interval", async () => {
    _testUtils.observeGithubRateLimit(
      {
        timestamp: new Date(now).toISOString(),
        component: "test",
        operation: "gh.api.graphql",
        args: [],
        ok: true,
        durationMs: 1,
        stdoutBytes: 0,
        stderrBytes: 0,
        rateLimitLimit: 5_000,
        rateLimitRemaining: 250,
        rateLimitReset: now / 1_000 + 3_600,
        rateLimitResource: "graphql",
      } satisfies GhTraceEntry,
      now,
    );

    const list = vi.fn(async () => []);
    const sessionManager = { list } as unknown as OpenCodeSessionManager;
    const registry = {
      get: vi.fn(() => null),
    } as unknown as PluginRegistry;
    const config = {
      configPath: "/tmp/agent-orchestrator.yaml",
      port: 3000,
      readyThresholdMs: 300_000,
      defaults: {
        runtime: "tmux",
        agent: "claude-code",
        workspace: "worktree",
        notifiers: [],
      },
      projects: {
        "rate-limit-test": {
          name: "Rate limit test",
          path: "/tmp/rate-limit-test",
          defaultBranch: "main",
          sessionPrefix: "rate",
        },
      },
      notifiers: {},
      notificationRouting: { urgent: [], action: [], warning: [], info: [] },
      reactions: {},
    } satisfies OrchestratorConfig;
    const lifecycle = createLifecycleManager({ config, registry, sessionManager });

    lifecycle.start(1_000);
    await vi.advanceTimersByTimeAsync(0);
    expect(list).toHaveBeenCalledTimes(1);

    await vi.advanceTimersByTimeAsync(3_999);
    expect(list).toHaveBeenCalledTimes(1);

    await vi.advanceTimersByTimeAsync(1);
    expect(list).toHaveBeenCalledTimes(2);
    lifecycle.stop();
  });
});
