/**
 * Tests for getPluginRegistry's cache reconciliation: a cached registry must
 * still reconcile a freshly loaded config's inline-external plugin references
 * (which carry an inferred temporary name each load) with the real manifest name
 * the registry registered under — otherwise registry.get() misses on later polls.
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import type { OrchestratorConfig } from "@aoagents/ao-core";

const { mockCreatePluginRegistry } = vi.hoisted(() => ({
  mockCreatePluginRegistry: vi.fn(),
}));

vi.mock("@aoagents/ao-core", async (importOriginal) => {
  // eslint-disable-next-line @typescript-eslint/consistent-type-imports
  const actual = await importOriginal<typeof import("@aoagents/ao-core")>();
  return {
    ...actual,
    createPluginRegistry: (...args: unknown[]) => mockCreatePluginRegistry(...args),
  };
});

vi.mock("../../src/lib/plugin-store.js", () => ({
  importPluginModuleFromSource: vi.fn(),
}));

import { getPluginRegistry } from "../../src/lib/create-session-manager.js";

// Fake registry whose loadFromConfig simulates the real one: it rewrites the
// inline-external tracker plugin reference from the inferred temp name to the
// manifest name on the config it is given.
const loadFromConfig = vi.fn(async (config: OrchestratorConfig) => {
  for (const entry of config._externalPluginEntries ?? []) {
    const loc = entry.location;
    if (loc.kind === "project") {
      const slot = config.projects[loc.projectId]?.[loc.configType];
      if (slot) slot.plugin = "real-tracker";
    }
  }
});

function makeConfig(configPath: string): OrchestratorConfig {
  return {
    configPath,
    projects: {
      app: { name: "app", path: "/p/app", tracker: { plugin: "tmp-inferred-name" } },
    },
    notifiers: {},
    _externalPluginEntries: [
      {
        source: "project app tracker",
        location: { kind: "project", projectId: "app", configType: "tracker" },
        slot: "tracker",
        package: "@acme/tracker",
      },
    ],
  } as unknown as OrchestratorConfig;
}

describe("getPluginRegistry — cache reconciliation", () => {
  beforeEach(() => {
    loadFromConfig.mockClear();
    mockCreatePluginRegistry.mockReset();
    mockCreatePluginRegistry.mockReturnValue({
      loadFromConfig,
      get: vi.fn(),
      register: vi.fn(),
      list: vi.fn(),
      loadBuiltins: vi.fn(),
    });
  });

  it("reconciles a fresh config's inline-external plugin name on a cache hit", async () => {
    // First call builds the registry; loadFromConfig rewrites the temp name.
    const built = makeConfig("/scope-reconcile");
    await getPluginRegistry(built);
    expect(built.projects.app.tracker?.plugin).toBe("real-tracker");

    // Second call with a freshly loaded config (temp name again) hits the cache.
    const fresh = makeConfig("/scope-reconcile");
    expect(fresh.projects.app.tracker?.plugin).toBe("tmp-inferred-name");
    await getPluginRegistry(fresh);

    // The cached registry was reused (no rebuild) but the fresh config was
    // reconciled to the registered manifest name.
    expect(loadFromConfig).toHaveBeenCalledTimes(1);
    expect(fresh.projects.app.tracker?.plugin).toBe("real-tracker");
  });
});
