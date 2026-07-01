/**
 * Tests for loadMergedScopeConfig — the union of the global registry and the
 * startup config used by both the backlog poller and shutdown.
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { dirname, isAbsolute, resolve } from "node:path";

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

import { loadMergedScopeConfig, __resetMergedScopeCache } from "../../src/lib/config-scope.js";

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
    __resetMergedScopeCache();
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

  it("keeps the canonical global entry for a project present in both configs (same path)", () => {
    // Same id AND same path → genuinely the same registered project: keep the
    // global entry. (A same-id/different-path collision is rejected — see below.)
    const startup = {
      configPath: STARTUP,
      defaults: startupDefaults,
      projects: { shared: project("startup-shared", "s", { path: "/repos/shared" }) },
    };
    const global = {
      configPath: GLOBAL,
      defaults: globalDefaults,
      projects: { shared: project("global-shared", "g", { path: "/repos/shared" }) },
    };
    mockLoadConfig.mockImplementation((p: string) => (p === GLOBAL ? global : startup));

    const merged = loadMergedScopeConfig(STARTUP);

    expect(merged.projects.shared.name).toBe("global-shared");
  });

  it("rejects a project-id collision when the startup project points at a different path", () => {
    const startup = {
      configPath: STARTUP,
      defaults: startupDefaults,
      projects: { shared: project("shared", "s", { path: "/repos/startup-shared" }) },
    };
    const global = {
      configPath: GLOBAL,
      defaults: globalDefaults,
      projects: { shared: project("shared", "g", { path: "/repos/global-shared" }) },
    };
    mockLoadConfig.mockImplementation((p: string) => (p === GLOBAL ? global : startup));

    expect(() => loadMergedScopeConfig(STARTUP)).toThrow(/project id collision/i);
  });

  it("absolutizes a startup-only project's relative local plugin path against the startup dir", () => {
    // The merged scope keeps the global configPath, so a relative local plugin
    // path would otherwise resolve under the global dir. It must be absolutized
    // against the startup config's directory before merging.
    const startup = {
      configPath: STARTUP,
      defaults: startupDefaults,
      projects: { local: project("local", "l") },
      plugins: [{ name: "custom-tracker", source: "local", path: "./plugins/tracker" }],
      _externalPluginEntries: [
        {
          source: "project local tracker",
          location: { kind: "project", projectId: "local", configType: "tracker" },
          slot: "tracker",
          path: "./plugins/tracker",
        },
      ],
    };
    const global = {
      configPath: GLOBAL,
      defaults: globalDefaults,
      projects: { reg: project("reg", "r") },
    };
    mockLoadConfig.mockImplementation((p: string) => (p === GLOBAL ? global : startup));

    const merged = loadMergedScopeConfig(STARTUP);

    const expectedPath = resolve(dirname(STARTUP), "./plugins/tracker");
    const custom = merged.plugins?.find((p) => p.name === "custom-tracker");
    expect(custom?.path).toBe(expectedPath);
    expect(isAbsolute(custom?.path ?? "")).toBe(true);
    // The matching external entry is absolutized identically so registry lookup
    // (keyed by path) still resolves it.
    expect(merged._externalPluginEntries?.[0]?.path).toBe(expectedPath);
  });

  it("gives the merged scope a distinct registry cache key when carrying startup plugins", () => {
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
    };
    mockLoadConfig.mockImplementation((p: string) => (p === GLOBAL ? global : startup));

    const merged = loadMergedScopeConfig(STARTUP);

    expect(merged._registryScopeKey).toBe(`${GLOBAL}::+startup:${STARTUP}`);
    // configPath stays the global path so registered projects' AO_CONFIG_PATH is
    // still correct; only the registry cache key changes.
    expect(merged.configPath).toBe(GLOBAL);
  });

  it("does not set a distinct cache key when nothing is startup-only", () => {
    const startup = {
      configPath: STARTUP,
      defaults: startupDefaults,
      projects: { shared: project("startup-shared", "s", { path: "/repos/shared" }) },
    };
    const global = {
      configPath: GLOBAL,
      defaults: globalDefaults,
      projects: { shared: project("global-shared", "s", { path: "/repos/shared" }) },
    };
    mockLoadConfig.mockImplementation((p: string) => (p === GLOBAL ? global : startup));

    const merged = loadMergedScopeConfig(STARTUP);

    expect(merged._registryScopeKey).toBeUndefined();
  });

  it("rejects a startup-only project whose session prefix collides with a registered one", () => {
    // loadConfig validates each config in isolation; merging can still introduce
    // a cross-config prefix collision. Rather than silently drop the startup
    // project (whose dashboard/orchestrator may already be running), abort loudly
    // so the collision is fixed before sessions go unsupervised.
    const startup = {
      configPath: STARTUP,
      defaults: startupDefaults,
      projects: { localdup: project("localdup", "dup") },
    };
    const global = {
      configPath: GLOBAL,
      defaults: globalDefaults,
      projects: { reg: project("reg", "dup") },
    };
    mockLoadConfig.mockImplementation((p: string) => (p === GLOBAL ? global : startup));

    expect(() => loadMergedScopeConfig(STARTUP)).toThrow(/prefix collision/i);
  });

  it("bakes startup-wide reactions / notificationRouting / lifecycle onto a carried project", () => {
    const startup = {
      configPath: STARTUP,
      defaults: startupDefaults,
      projects: { local: project("local", "l") },
      reactions: { "pr-closed": { auto: false } },
      notificationRouting: { action: ["slack"] },
      lifecycle: { autoCleanupOnMerge: false },
    };
    const global = {
      configPath: GLOBAL,
      defaults: globalDefaults,
      projects: { reg: project("reg", "r") },
      reactions: { "pr-closed": { auto: true } },
      notificationRouting: { action: ["desktop"] },
      lifecycle: { autoCleanupOnMerge: true },
    };
    mockLoadConfig.mockImplementation((p: string) => (p === GLOBAL ? global : startup));

    const merged = loadMergedScopeConfig(STARTUP);

    // The carried startup-only project keeps its own startup-wide policy as
    // per-project overrides, not the global top-level policy.
    expect(merged.projects.local.reactions?.["pr-closed"]).toEqual({ auto: false });
    // notificationRouting is baked COMPLETE: the explicit startup route is kept,
    // and unrouted priorities fall back to the startup defaults (empty here) — not
    // the global top-level routing.
    expect(merged.projects.local.notificationRouting?.action).toEqual(["slack"]);
    expect(merged.projects.local.notificationRouting?.warning).toEqual([]);
    expect(merged.projects.local.lifecycle).toEqual({ autoCleanupOnMerge: false });
    // The merged config's top-level policy stays global (it governs global projects).
    expect(merged.notificationRouting).toEqual({ action: ["desktop"] });
  });

  it("fills every priority from startup defaults.notifiers when no explicit routing", () => {
    // A startup-only project that relies on defaults.notifiers (no notificationRouting)
    // must route ALL priorities to those defaults, not fall through to global defaults.
    const startup = {
      configPath: STARTUP,
      defaults: { ...startupDefaults, notifiers: ["startup-slack"] },
      projects: { local: project("local", "l") },
    };
    const global = {
      configPath: GLOBAL,
      defaults: { ...globalDefaults, notifiers: ["global-desktop"] },
      projects: { reg: project("reg", "r") },
    };
    mockLoadConfig.mockImplementation((p: string) => (p === GLOBAL ? global : startup));

    const merged = loadMergedScopeConfig(STARTUP);

    const routing = merged.projects.local.notificationRouting;
    expect(routing?.urgent).toEqual(["startup-slack"]);
    expect(routing?.action).toEqual(["startup-slack"]);
    expect(routing?.warning).toEqual(["startup-slack"]);
    expect(routing?.info).toEqual(["startup-slack"]);
  });

  it("bakes the startup top-level budget onto a carried project", () => {
    const startup = {
      configPath: STARTUP,
      defaults: startupDefaults,
      projects: { local: project("local", "l") },
      budget: { perSessionUsd: 5 },
    };
    const global = {
      configPath: GLOBAL,
      defaults: globalDefaults,
      projects: { reg: project("reg", "r") },
      budget: { perSessionUsd: 99 },
    };
    mockLoadConfig.mockImplementation((p: string) => (p === GLOBAL ? global : startup));

    const merged = loadMergedScopeConfig(STARTUP);

    expect(merged.projects.local.budget).toEqual({ perSessionUsd: 5 });
  });

  it("merges budget fields when a carried project has a partial project budget", () => {
    // project sets only perSessionUsd; the startup top-level supplies perProjectUsd.
    // A whole-object fallback would drop perProjectUsd (and resolveBudget won't
    // inherit it from global for a carried project) — field-merge preserves both.
    const startup = {
      configPath: STARTUP,
      defaults: startupDefaults,
      projects: { local: project("local", "l", { budget: { perSessionUsd: 2 } }) },
      budget: { perProjectUsd: 50 },
    };
    const global = {
      configPath: GLOBAL,
      defaults: globalDefaults,
      projects: { reg: project("reg", "r") },
      budget: { perSessionUsd: 99, perProjectUsd: 99 },
    };
    mockLoadConfig.mockImplementation((p: string) => (p === GLOBAL ? global : startup));

    const merged = loadMergedScopeConfig(STARTUP);

    expect(merged.projects.local.budget).toEqual({ perSessionUsd: 2, perProjectUsd: 50 });
  });

  it("carries the startup readyThresholdMs onto a carried project", () => {
    const startup = {
      configPath: STARTUP,
      defaults: startupDefaults,
      projects: { local: project("local", "l") },
      readyThresholdMs: 120_000,
    };
    const global = {
      configPath: GLOBAL,
      defaults: globalDefaults,
      projects: { reg: project("reg", "r") },
      readyThresholdMs: 600_000,
    };
    mockLoadConfig.mockImplementation((p: string) => (p === GLOBAL ? global : startup));

    const merged = loadMergedScopeConfig(STARTUP);

    // The carried project keeps the startup threshold, not the global one.
    expect(merged.projects.local.readyThresholdMs).toBe(120_000);
  });

  it("merges startup-only notifier definitions (global wins on alias collision)", () => {
    const startup = {
      configPath: STARTUP,
      defaults: startupDefaults,
      projects: { local: project("local", "l") },
      notifiers: {
        slack: { plugin: "slack", webhookUrl: "startup-hook" },
        shared: { plugin: "slack", webhookUrl: "startup-shared" },
      },
    };
    const global = {
      configPath: GLOBAL,
      defaults: globalDefaults,
      projects: { reg: project("reg", "r") },
      notifiers: { shared: { plugin: "slack", webhookUrl: "global-shared" } },
    };
    mockLoadConfig.mockImplementation((p: string) => (p === GLOBAL ? global : startup));

    const merged = loadMergedScopeConfig(STARTUP);

    // Startup-only alias is added; the colliding alias keeps the global value.
    expect(merged.notifiers.slack).toEqual({ plugin: "slack", webhookUrl: "startup-hook" });
    expect(merged.notifiers.shared).toEqual({ plugin: "slack", webhookUrl: "global-shared" });
  });

  it("rejects a notifier alias collision a carried project routes to", () => {
    // "shared" is defined in both configs; the merge keeps the global definition,
    // so the carried project would notify the wrong target — abort instead.
    const startup = {
      configPath: STARTUP,
      defaults: startupDefaults,
      projects: { local: project("local", "l") },
      notificationRouting: { action: ["shared"] },
      notifiers: { shared: { plugin: "slack", webhookUrl: "startup-shared" } },
    };
    const global = {
      configPath: GLOBAL,
      defaults: globalDefaults,
      projects: { reg: project("reg", "r") },
      notifiers: { shared: { plugin: "slack", webhookUrl: "global-shared" } },
    };
    mockLoadConfig.mockImplementation((p: string) => (p === GLOBAL ? global : startup));

    expect(() => loadMergedScopeConfig(STARTUP)).toThrow(/notifier alias collision/i);
  });

  it("allows a carried project to route to a startup-only notifier alias", () => {
    const startup = {
      configPath: STARTUP,
      defaults: startupDefaults,
      projects: { local: project("local", "l") },
      notificationRouting: { action: ["startup-slack"] },
      notifiers: { "startup-slack": { plugin: "slack", webhookUrl: "hook" } },
    };
    const global = {
      configPath: GLOBAL,
      defaults: globalDefaults,
      projects: { reg: project("reg", "r") },
      notifiers: {},
    };
    mockLoadConfig.mockImplementation((p: string) => (p === GLOBAL ? global : startup));

    const merged = loadMergedScopeConfig(STARTUP);

    expect(merged.projects.local.notificationRouting?.action).toEqual(["startup-slack"]);
    expect(merged.notifiers["startup-slack"]).toEqual({ plugin: "slack", webhookUrl: "hook" });
  });

  it("rejects a startup plugin whose name matches an ENABLED global plugin of a different identity", () => {
    // Same logical name, different implementation, global enabled: the registry
    // keys instances by slot:name, so loading both would silently overwrite the
    // global implementation. The merge must abort instead.
    const startup = {
      configPath: STARTUP,
      defaults: startupDefaults,
      projects: { local: project("local", "l") },
      plugins: [{ name: "tracker", source: "local", path: "./plugins/tracker" }],
    };
    const global = {
      configPath: GLOBAL,
      defaults: globalDefaults,
      projects: { reg: project("reg", "r") },
      plugins: [{ name: "tracker", source: "npm", package: "@acme/tracker" }],
    };
    mockLoadConfig.mockImplementation((p: string) => (p === GLOBAL ? global : startup));

    expect(() => loadMergedScopeConfig(STARTUP)).toThrow(/plugin name collision/i);
  });

  it("dedupes a startup plugin that is identical to an enabled global plugin", () => {
    const startup = {
      configPath: STARTUP,
      defaults: startupDefaults,
      projects: { local: project("local", "l") },
      plugins: [{ name: "tracker", source: "npm", package: "@acme/tracker" }],
    };
    const global = {
      configPath: GLOBAL,
      defaults: globalDefaults,
      projects: { reg: project("reg", "r") },
      plugins: [{ name: "tracker", source: "npm", package: "@acme/tracker" }],
    };
    mockLoadConfig.mockImplementation((p: string) => (p === GLOBAL ? global : startup));

    const merged = loadMergedScopeConfig(STARTUP);

    // Same identity → not duplicated, not a collision.
    expect(merged.plugins?.filter((p) => p.name === "tracker")).toHaveLength(1);
  });

  it("does not reject an inferred-name (temp basename) plugin clash", () => {
    // Both configs declare inline external plugins WITHOUT an explicit `plugin`,
    // so their config.plugins names are temp basenames the registry replaces with
    // distinct manifest names at load time. A basename clash must NOT abort.
    const startup = {
      configPath: STARTUP,
      defaults: startupDefaults,
      projects: { local: project("local", "l") },
      plugins: [{ name: "tracker", source: "local", path: "./plugins/tracker" }],
      _externalPluginEntries: [
        {
          source: "project local tracker",
          location: { kind: "project", projectId: "local", configType: "tracker" },
          slot: "tracker",
          path: "./plugins/tracker",
          // expectedPluginName omitted → inferred temp name
        },
      ],
    };
    const global = {
      configPath: GLOBAL,
      defaults: globalDefaults,
      projects: { reg: project("reg", "r") },
      plugins: [{ name: "tracker", source: "npm", package: "@acme/tracker" }],
      _externalPluginEntries: [
        {
          source: "project reg tracker",
          location: { kind: "project", projectId: "reg", configType: "tracker" },
          slot: "tracker",
          package: "@acme/tracker",
        },
      ],
    };
    mockLoadConfig.mockImplementation((p: string) => (p === GLOBAL ? global : startup));

    const merged = loadMergedScopeConfig(STARTUP);

    // Both survive — they load under distinct slot:manifest.name keys.
    expect(merged.plugins?.filter((p) => p.name === "tracker")).toHaveLength(2);
  });

  it("carries a startup plugin whose name matches a DISABLED global plugin", () => {
    const startup = {
      configPath: STARTUP,
      defaults: startupDefaults,
      projects: { local: project("local", "l") },
      plugins: [{ name: "tracker", source: "npm", package: "@acme/tracker" }],
    };
    const global = {
      configPath: GLOBAL,
      defaults: globalDefaults,
      projects: { reg: project("reg", "r") },
      plugins: [{ name: "tracker", source: "npm", package: "@acme/tracker", enabled: false }],
    };
    mockLoadConfig.mockImplementation((p: string) => (p === GLOBAL ? global : startup));

    const merged = loadMergedScopeConfig(STARTUP);

    // The disabled global entry must not suppress the (enabled) startup one.
    expect(merged.plugins?.some((p) => p.name === "tracker" && p.enabled !== false)).toBe(true);
  });

  it("does not apply a startup external entry to a skipped (registered) project", () => {
    const startup = {
      configPath: STARTUP,
      defaults: startupDefaults,
      // `reg` collides with a registered project (same path → skipped); `local`
      // is carried. Only the carried project's external entry should survive.
      projects: {
        reg: project("reg", "r", { path: "/repos/reg" }),
        local: project("local", "l"),
      },
      _externalPluginEntries: [
        {
          source: "project reg tracker",
          location: { kind: "project", projectId: "reg", configType: "tracker" },
          slot: "tracker",
          package: "@startup/reg-tracker",
        },
        {
          source: "project local tracker",
          location: { kind: "project", projectId: "local", configType: "tracker" },
          slot: "tracker",
          package: "@startup/local-tracker",
        },
      ],
    };
    const global = {
      configPath: GLOBAL,
      defaults: globalDefaults,
      projects: { reg: project("reg", "r", { path: "/repos/reg" }) },
    };
    mockLoadConfig.mockImplementation((p: string) => (p === GLOBAL ? global : startup));

    const merged = loadMergedScopeConfig(STARTUP);

    const entryProjects = (merged._externalPluginEntries ?? []).map(
      (e) => (e.location as { projectId?: string }).projectId,
    );
    expect(entryProjects).toContain("local");
    expect(entryProjects).not.toContain("reg");
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

  it("falls back to the global config when the startup config disappears at runtime", () => {
    // The startup (local/URL) config was removed/became unreadable while the
    // daemon runs. The global registry still exists, so the merged scope must
    // return it (keep polling/shutting down registered projects) rather than throw.
    const global = {
      configPath: GLOBAL,
      defaults: globalDefaults,
      projects: { reg: project("reg", "r") },
      notifiers: {},
    };
    mockLoadConfig.mockImplementation((p: string) => {
      if (p === GLOBAL) return global;
      const err = new Error("ENOENT") as NodeJS.ErrnoException & { path?: string };
      err.code = "ENOENT";
      err.path = STARTUP;
      throw err;
    });

    const merged = loadMergedScopeConfig(STARTUP);

    expect(merged).toBe(global);
  });

  it("preserves carried startup projects when the startup config disappears after a good load", () => {
    // First load succeeds and caches the startup config (carrying `local`). When
    // the startup file later vanishes, the merged scope must still include `local`
    // (so shutdown kills its sessions and the supervisor doesn't detach it).
    const startup = {
      configPath: STARTUP,
      defaults: startupDefaults,
      projects: { local: project("local", "l") },
      notifiers: {},
    };
    const global = {
      configPath: GLOBAL,
      defaults: globalDefaults,
      projects: { reg: project("reg", "r") },
      notifiers: {},
    };
    let startupGone = false;
    mockLoadConfig.mockImplementation((p: string) => {
      if (p === GLOBAL) return global;
      if (startupGone) {
        const err = new Error("ENOENT") as NodeJS.ErrnoException & { path?: string };
        err.code = "ENOENT";
        err.path = STARTUP;
        throw err;
      }
      return startup;
    });

    const first = loadMergedScopeConfig(STARTUP);
    expect(first.projects.local).toBeDefined();
    // A fresh (non-cached) load leaves the carried project spawnable.
    expect(first.projects.local._spawnPaused).toBeUndefined();

    startupGone = true;
    const second = loadMergedScopeConfig(STARTUP);

    // Carried startup project survives the disappearance; global project stays too.
    expect(second.projects.local).toBeDefined();
    expect(second.projects.reg).toBeDefined();
    // But it's supervision/shutdown-only now — the poller must not spawn into it.
    expect(second.projects.local._spawnPaused).toBe(true);
  });

  it("uses the startup config's daemon port in the merged scope", () => {
    // The daemon binds the STARTUP config's port; workers spawn with AO_PORT from
    // config.port and must reach THIS daemon, not the global registry's port.
    const startup = {
      configPath: STARTUP,
      defaults: startupDefaults,
      projects: { local: project("local", "l") },
      notifiers: {},
      port: 4100,
    };
    const global = {
      configPath: GLOBAL,
      defaults: globalDefaults,
      projects: { reg: project("reg", "r") },
      notifiers: {},
      port: 3000,
    };
    mockLoadConfig.mockImplementation((p: string) => (p === GLOBAL ? global : startup));

    // With a carried startup-only project.
    expect(loadMergedScopeConfig(STARTUP).port).toBe(4100);

    // And when nothing is startup-only (registered project in both, same path).
    const startup2 = {
      configPath: STARTUP,
      defaults: startupDefaults,
      projects: { shared: project("shared", "s", { path: "/repos/shared" }) },
      notifiers: {},
      port: 4100,
    };
    const global2 = {
      configPath: GLOBAL,
      defaults: globalDefaults,
      projects: { shared: project("shared", "s", { path: "/repos/shared" }) },
      notifiers: {},
      port: 3000,
    };
    mockLoadConfig.mockImplementation((p: string) => (p === GLOBAL ? global2 : startup2));
    expect(loadMergedScopeConfig(STARTUP).port).toBe(4100);
  });

  it("pauses spawns for a cached startup config when no global registry exists", () => {
    // First-run scope (no global). After a good load, the startup file becomes
    // unreadable — the cached copy is served, but its projects must be
    // supervision/shutdown-only so the poller doesn't spawn with a stale path.
    const startup = {
      configPath: STARTUP,
      defaults: startupDefaults,
      projects: { local: project("local", "l") },
      notifiers: {},
    };
    const missingGlobal = (): never => {
      const err = new Error("ENOENT") as NodeJS.ErrnoException & { path?: string };
      err.code = "ENOENT";
      err.path = GLOBAL;
      throw err;
    };
    let startupGone = false;
    mockLoadConfig.mockImplementation((p: string) => {
      if (p === GLOBAL) return missingGlobal();
      if (startupGone) {
        const err = new Error("ENOENT") as NodeJS.ErrnoException & { path?: string };
        err.code = "ENOENT";
        err.path = STARTUP;
        throw err;
      }
      return startup;
    });

    // Fresh load (no global): startup returned as-is, spawnable.
    const first = loadMergedScopeConfig(STARTUP);
    expect(first.projects.local._spawnPaused).toBeUndefined();

    // Startup file vanishes: cached copy served with projects spawn-paused.
    startupGone = true;
    const second = loadMergedScopeConfig(STARTUP);
    expect(second.projects.local).toBeDefined();
    expect(second.projects.local._spawnPaused).toBe(true);
  });
});
