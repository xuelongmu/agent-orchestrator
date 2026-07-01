/**
 * Tests for the CLI backlog-service wiring — specifically that the poller's
 * resolveServices returns a config carrying the RESOLVED daemon port, so
 * backlog-spawned sessions inherit AO_PORT for the port the dashboard actually
 * bound (not the stale requested value the reloaded config file carries).
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import type { BacklogServices, OrchestratorConfig } from "@aoagents/ao-core";

const {
  mockCreateBacklogPoller,
  mockLoadMergedScopeConfig,
  mockGetPluginRegistry,
  mockGetSessionManager,
} = vi.hoisted(() => ({
  mockCreateBacklogPoller: vi.fn(),
  mockLoadMergedScopeConfig: vi.fn(),
  mockGetPluginRegistry: vi.fn(),
  mockGetSessionManager: vi.fn(),
}));

vi.mock("@aoagents/ao-core", async (importOriginal) => {
  // eslint-disable-next-line @typescript-eslint/consistent-type-imports
  const actual = await importOriginal<typeof import("@aoagents/ao-core")>();
  return {
    ...actual,
    createBacklogPoller: (...args: unknown[]) => mockCreateBacklogPoller(...args),
  };
});

vi.mock("../../src/lib/config-scope.js", () => ({
  loadMergedScopeConfig: (...args: unknown[]) => mockLoadMergedScopeConfig(...args),
}));

vi.mock("../../src/lib/create-session-manager.js", () => ({
  getPluginRegistry: (...args: unknown[]) => mockGetPluginRegistry(...args),
  getSessionManager: (...args: unknown[]) => mockGetSessionManager(...args),
}));

import { startBacklogPoller, stopBacklogPoller } from "../../src/lib/backlog-service.js";

type ResolveServices = () => Promise<BacklogServices>;

describe("startBacklogPoller — resolved port", () => {
  let capturedResolveServices: ResolveServices | undefined;

  beforeEach(async () => {
    await stopBacklogPoller();
    mockCreateBacklogPoller.mockReset();
    mockLoadMergedScopeConfig.mockReset();
    mockGetPluginRegistry.mockReset();
    mockGetSessionManager.mockReset();

    capturedResolveServices = undefined;
    mockCreateBacklogPoller.mockImplementation((opts: { resolveServices: ResolveServices }) => {
      capturedResolveServices = opts.resolveServices;
      return { start: vi.fn(), stop: vi.fn().mockResolvedValue(undefined), pollOnce: vi.fn() };
    });
    mockGetPluginRegistry.mockResolvedValue({});
    mockGetSessionManager.mockResolvedValue({});
  });

  it("overrides the reloaded config's port with the resolved daemon port", async () => {
    // Config file requests 3000, but the daemon fell back and bound 3005.
    mockLoadMergedScopeConfig.mockReturnValue({
      configPath: "/repo/agent-orchestrator.yaml",
      port: 3000,
      projects: {},
    } as unknown as OrchestratorConfig);

    startBacklogPoller("/repo/agent-orchestrator.yaml", 3005);

    expect(capturedResolveServices).toBeDefined();
    const services = await capturedResolveServices!();
    expect(services.config.port).toBe(3005);
  });

  it("leaves the config port untouched when no resolved port is passed", async () => {
    mockLoadMergedScopeConfig.mockReturnValue({
      configPath: "/repo/agent-orchestrator.yaml",
      port: 3000,
      projects: {},
    } as unknown as OrchestratorConfig);

    startBacklogPoller("/repo/agent-orchestrator.yaml");

    const services = await capturedResolveServices!();
    expect(services.config.port).toBe(3000);
  });
});
