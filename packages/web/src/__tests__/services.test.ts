import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const {
  mockLoadConfig,
  mockGetGlobalConfigPath,
  MockConfigNotFoundError,
  mockRegister,
  mockCreateSessionManager,
  mockCreateBacklogPoller,
  mockRegistry,
  tmuxPlugin,
  aiderPlugin,
  claudePlugin,
  codexPlugin,
  grokPlugin,
  opencodePlugin,
  worktreePlugin,
  scmPlugin,
  trackerGithubPlugin,
  trackerLinearPlugin,
} = vi.hoisted(() => {
  const mockLoadConfig = vi.fn();
  const mockGetGlobalConfigPath = vi.fn();
  const mockCreateBacklogPoller = vi.fn();
  class MockConfigNotFoundError extends Error {
    constructor(message?: string) {
      super(message ?? "Config not found");
      this.name = "ConfigNotFoundError";
    }
  }
  const mockRegister = vi.fn();
  const mockCreateSessionManager = vi.fn();
  const mockRegistry = {
    register: mockRegister,
    get: vi.fn(),
    list: vi.fn(),
    loadBuiltins: vi.fn(),
    loadFromConfig: vi.fn(),
  };

  return {
    mockLoadConfig,
    mockGetGlobalConfigPath,
    MockConfigNotFoundError,
    mockRegister,
    mockCreateSessionManager,
    mockCreateBacklogPoller,
    mockRegistry,
    tmuxPlugin: { manifest: { name: "tmux" } },
    aiderPlugin: { manifest: { name: "aider" } },
    claudePlugin: { manifest: { name: "claude-code" } },
    codexPlugin: { manifest: { name: "codex" } },
    grokPlugin: { manifest: { name: "grok" } },
    opencodePlugin: { manifest: { name: "opencode" } },
    worktreePlugin: { manifest: { name: "worktree" } },
    scmPlugin: { manifest: { name: "github" } },
    trackerGithubPlugin: { manifest: { name: "github" } },
    trackerLinearPlugin: { manifest: { name: "linear" } },
  };
});

vi.mock("@aoagents/ao-core", () => ({
  loadConfig: mockLoadConfig,
  getGlobalConfigPath: mockGetGlobalConfigPath,
  ConfigNotFoundError: MockConfigNotFoundError,
  createPluginRegistry: () => mockRegistry,
  createSessionManager: mockCreateSessionManager,
  createLifecycleManager: () => ({
    start: vi.fn(),
    stop: vi.fn(),
    getStates: vi.fn(),
    check: vi.fn(),
  }),
  createBacklogPoller: mockCreateBacklogPoller,
  BACKLOG_LABEL: "agent:backlog",
  TERMINAL_STATUSES: new Set(["merged", "killed"]) as ReadonlySet<string>,
}));

vi.mock("@aoagents/ao-plugin-runtime-tmux", () => ({ default: tmuxPlugin }));
vi.mock("@aoagents/ao-plugin-agent-aider", () => ({ default: aiderPlugin }));
vi.mock("@aoagents/ao-plugin-agent-claude-code", () => ({ default: claudePlugin }));
vi.mock("@aoagents/ao-plugin-agent-codex", () => ({ default: codexPlugin }));
vi.mock("@aoagents/ao-plugin-agent-grok", () => ({ default: grokPlugin }));
vi.mock("@aoagents/ao-plugin-agent-opencode", () => ({ default: opencodePlugin }));
vi.mock("@aoagents/ao-plugin-workspace-worktree", () => ({ default: worktreePlugin }));
vi.mock("@aoagents/ao-plugin-scm-github", () => ({ default: scmPlugin }));
vi.mock("@aoagents/ao-plugin-tracker-github", () => ({ default: trackerGithubPlugin }));
vi.mock("@aoagents/ao-plugin-tracker-linear", () => ({ default: trackerLinearPlugin }));

describe("services", () => {
  beforeEach(() => {
    vi.resetModules();
    mockRegister.mockClear();
    mockCreateSessionManager.mockReset();
    mockLoadConfig.mockReset();
    mockGetGlobalConfigPath.mockReset();
    mockGetGlobalConfigPath.mockReturnValue("/tmp/global-config.yaml");
    mockLoadConfig.mockReturnValue({
      configPath: "/tmp/agent-orchestrator.yaml",
      port: 3000,
      readyThresholdMs: 300_000,
      defaults: { runtime: "tmux", agent: "claude-code", workspace: "worktree", notifiers: [] },
      projects: {},
      notifiers: {},
      notificationRouting: { urgent: [], action: [], warning: [], info: [] },
      reactions: {},
    });
    mockCreateSessionManager.mockReturnValue({});
    delete (globalThis as typeof globalThis & { _aoServices?: unknown })._aoServices;
    delete (globalThis as typeof globalThis & { _aoServicesInit?: unknown })._aoServicesInit;
  });

  afterEach(() => {
    delete (globalThis as typeof globalThis & { _aoServices?: unknown })._aoServices;
    delete (globalThis as typeof globalThis & { _aoServicesInit?: unknown })._aoServicesInit;
  });

  it("registers the OpenCode agent plugin with web services", async () => {
    const { getServices } = await import("../lib/services");

    await getServices();

    expect(mockRegister).toHaveBeenCalledWith(opencodePlugin);
  });

  it("registers the Codex agent plugin with web services", async () => {
    const { getServices } = await import("../lib/services");

    await getServices();

    expect(mockRegister).toHaveBeenCalledWith(codexPlugin);
  });

  it("registers the Aider agent plugin with web services", async () => {
    // A built-in agent whose plugin the web service failed to register would make
    // every callback dispatch to an aider session throw 500 (#13 review).
    const { getServices } = await import("../lib/services");

    await getServices();

    expect(mockRegister).toHaveBeenCalledWith(aiderPlugin);
  });

  it("registers the Grok agent plugin with web services", async () => {
    const { getServices } = await import("../lib/services");

    await getServices();

    expect(mockRegister).toHaveBeenCalledWith(grokPlugin);
  });

  it("caches initialized services across repeated calls", async () => {
    const { getServices } = await import("../lib/services");

    const first = await getServices();
    const second = await getServices();

    expect(first).toBe(second);
    expect(mockCreateSessionManager).toHaveBeenCalledTimes(1);
  });

  it("loads config from the canonical global config path", async () => {
    const { getServices } = await import("../lib/services");

    await getServices();

    expect(mockGetGlobalConfigPath).toHaveBeenCalledTimes(1);
    expect(mockLoadConfig).toHaveBeenCalledWith("/tmp/global-config.yaml");
  });

  it("falls back to discovered config when the canonical global config is missing", async () => {
    mockLoadConfig
      .mockImplementationOnce(() => {
        const error = new Error("ENOENT: no such file or directory");
        (error as Error & { code?: string }).code = "ENOENT";
        throw error;
      })
      .mockReturnValueOnce({
        configPath: "/tmp/local/agent-orchestrator.yaml",
        port: 3000,
        readyThresholdMs: 300_000,
        defaults: { runtime: "tmux", agent: "claude-code", workspace: "worktree", notifiers: [] },
        projects: {},
        notifiers: {},
        notificationRouting: { urgent: [], action: [], warning: [], info: [] },
        reactions: {},
      });

    const { getServices } = await import("../lib/services");

    await getServices();

    expect(mockLoadConfig).toHaveBeenNthCalledWith(1, "/tmp/global-config.yaml");
    expect(mockLoadConfig).toHaveBeenNthCalledWith(2);
  });
});

describe("backlog poller wiring", () => {
  const mockPollOnce = vi.fn();
  const mockStart = vi.fn();

  beforeEach(() => {
    vi.resetModules();
    mockCreateBacklogPoller.mockReset();
    mockPollOnce.mockReset().mockResolvedValue(undefined);
    mockStart.mockReset();
    mockGetGlobalConfigPath.mockReturnValue("/tmp/global-config.yaml");
    mockLoadConfig.mockReturnValue({
      configPath: "/tmp/agent-orchestrator.yaml",
      port: 3000,
      readyThresholdMs: 300_000,
      defaults: { runtime: "tmux", agent: "claude-code", workspace: "worktree", notifiers: [] },
      projects: {},
      notifiers: {},
      notificationRouting: { urgent: [], action: [], warning: [], info: [] },
      reactions: {},
    });
    mockCreateSessionManager.mockReturnValue({});
    mockCreateBacklogPoller.mockReturnValue({
      start: mockStart,
      stop: vi.fn(),
      pollOnce: mockPollOnce,
    });

    delete (globalThis as typeof globalThis & { _aoBacklogPoller?: unknown })._aoBacklogPoller;
    delete (globalThis as typeof globalThis & { _aoServices?: unknown })._aoServices;
    delete (globalThis as typeof globalThis & { _aoServicesInit?: unknown })._aoServicesInit;
  });

  afterEach(() => {
    delete (globalThis as typeof globalThis & { _aoBacklogPoller?: unknown })._aoBacklogPoller;
    delete (globalThis as typeof globalThis & { _aoServices?: unknown })._aoServices;
    delete (globalThis as typeof globalThis & { _aoServicesInit?: unknown })._aoServicesInit;
  });

  it("starts the core backlog poller", async () => {
    const { startBacklogPoller } = await import("../lib/services");
    startBacklogPoller();
    startBacklogPoller(); // idempotent — poller is cached on globalThis

    expect(mockCreateBacklogPoller).toHaveBeenCalledTimes(1);
    expect(mockStart).toHaveBeenCalledTimes(2);
  });

  it("delegates pollBacklog to the core poller", async () => {
    const { pollBacklog } = await import("../lib/services");
    await pollBacklog();

    expect(mockPollOnce).toHaveBeenCalledTimes(1);
  });
});
