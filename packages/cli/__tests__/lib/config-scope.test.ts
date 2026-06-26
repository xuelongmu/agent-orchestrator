/**
 * Tests for loadMergedScopeConfig — the union of the global registry and the
 * startup config used by both the backlog poller and shutdown.
 */

import { describe, it, expect, vi, beforeEach } from "vitest";

const { mockLoadConfig, mockGetGlobalConfigPath } = vi.hoisted(() => ({
  mockLoadConfig: vi.fn(),
  mockGetGlobalConfigPath: vi.fn(),
}));

vi.mock("@aoagents/ao-core", async (importOriginal) => {
  // eslint-disable-next-line @typescript-eslint/consistent-type-imports
  const actual = await importOriginal<typeof import("@aoagents/ao-core")>();
  return {
    ...actual,
    loadConfig: (...args: unknown[]) => mockLoadConfig(...args),
    getGlobalConfigPath: () => mockGetGlobalConfigPath(),
  };
});

import { loadMergedScopeConfig } from "../../src/lib/config-scope.js";

const GLOBAL = "/home/.agent-orchestrator/config.yaml";
const STARTUP = "/repo/agent-orchestrator.yaml";

const globalDefaults = {
  runtime: "tmux",
  agent: "claude-code",
  workspace: "worktree",
  notifiers: [],
};
const startupDefaults = {
  runtime: "process",
  agent: "codex",
  workspace: "clone",
  notifiers: [],
};

const project = (name: string, prefix: string, extra: Record<string, unknown> = {}) => ({
  name,
  path: `/p/${name}`,
  defaultBranch: "main",
  sessionPrefix: prefix,
  ...extra,
});

describe("loadMergedScopeConfig", () => {
  beforeEach(() => {
    mockLoadConfig.mockReset();
    mockGetGlobalConfigPath.mockReset();
    mockGetGlobalConfigPath.mockReturnValue(GLOBAL);
  });

  it("bakes the startup defaults into a startup-only project", () => {
    const startup = {
      configPath: STARTUP,
      defaults: startupDefaults,
      projects: { local: project("local", "l") },
    };
    const global = {
      configPath: GLOBAL,
      defaults: globalDefaults,
      projects: { reg: project("reg", "r") },
    };
    mockLoadConfig.mockImplementation((p: string) => (p === GLOBAL ? global : startup));

    const merged = loadMergedScopeConfig(STARTUP);

    // The startup-only project resolves its plugins from the STARTUP defaults,
    // not the merged config's (global) defaults.
    expect(merged.projects.local.runtime).toBe("process");
    expect(merged.projects.local.agent).toBe("codex");
    expect(merged.projects.local.workspace).toBe("clone");
    // The registered project is still present.
    expect(merged.projects.reg).toBeDefined();
    // Top-level defaults remain the global ones.
    expect(merged.defaults.workspace).toBe("worktree");
    // Carries the startup config path so the agent's `ao` commands resolve it.
    expect(merged.projects.local.sourceConfigPath).toBe(STARTUP);
  });

  it("preserves a startup role-specific worker default", () => {
    // A generic baked agent must NOT shadow the startup config's
    // `defaults.worker.agent` (resolution: project.worker.agent wins).
    const startup = {
      configPath: STARTUP,
      defaults: { ...startupDefaults, worker: { agent: "codex" }, agent: "claude-code" },
      projects: { local: project("local", "l") },
    };
    const global = { configPath: GLOBAL, defaults: globalDefaults, projects: {} };
    mockLoadConfig.mockImplementation((p: string) => (p === GLOBAL ? global : startup));

    const merged = loadMergedScopeConfig(STARTUP);

    expect(merged.projects.local.worker?.agent).toBe("codex");
  });

  it("merges startup plugin declarations when carrying a startup-only project", () => {
    const startup = {
      configPath: STARTUP,
      defaults: startupDefaults,
      projects: { local: project("local", "l") },
      plugins: [{ name: "jira", source: "npm", package: "@acme/ao-plugin-tracker-jira" }],
    };
    const global = {
      configPath: GLOBAL,
      defaults: globalDefaults,
      projects: { reg: project("reg", "r") },
      plugins: [{ name: "github", source: "registry" }],
    };
    mockLoadConfig.mockImplementation((p: string) => (p === GLOBAL ? global : startup));

    const merged = loadMergedScopeConfig(STARTUP);

    expect(merged.plugins?.map((p) => p.name)).toEqual(["github", "jira"]);
  });

  it("does not override a startup project's explicit plugin selections", () => {
    const startup = {
      configPath: STARTUP,
      defaults: startupDefaults,
      projects: { local: project("local", "l", { workspace: "worktree" }) },
    };
    const global = { configPath: GLOBAL, defaults: globalDefaults, projects: {} };
    mockLoadConfig.mockImplementation((p: string) => (p === GLOBAL ? global : startup));

    const merged = loadMergedScopeConfig(STARTUP);

    expect(merged.projects.local.workspace).toBe("worktree");
    expect(merged.projects.local.runtime).toBe("process"); // filled from startup defaults
  });

  it("keeps the canonical global entry for a project present in both configs", () => {
    const startup = {
      configPath: STARTUP,
      defaults: startupDefaults,
      projects: { shared: project("startup-shared", "s") },
    };
    const global = {
      configPath: GLOBAL,
      defaults: globalDefaults,
      projects: { shared: project("global-shared", "g") },
    };
    mockLoadConfig.mockImplementation((p: string) => (p === GLOBAL ? global : startup));

    const merged = loadMergedScopeConfig(STARTUP);

    expect(merged.projects.shared.name).toBe("global-shared");
  });

  it("falls back to the startup config when no global config exists", () => {
    const startup = {
      configPath: STARTUP,
      defaults: startupDefaults,
      projects: { local: project("local", "l") },
    };
    mockLoadConfig.mockImplementation((p: string) => {
      if (p === GLOBAL) {
        const err = new Error("ENOENT") as NodeJS.ErrnoException & { path?: string };
        err.code = "ENOENT";
        err.path = GLOBAL;
        throw err;
      }
      return startup;
    });

    const merged = loadMergedScopeConfig(STARTUP);

    expect(merged).toBe(startup);
  });
});
