/**
 * Session manager factory for the CLI.
 *
 * Creates a PluginRegistry with all available plugins loaded,
 * then creates a SessionManager instance backed by core's implementation.
 * This ensures the CLI uses the same hash-based naming, metadata format,
 * and plugin abstractions as the rest of the system.
 */

import {
  createPluginRegistry,
  createSessionManager,
  createLifecycleManager,
  type OrchestratorConfig,
  type OpenCodeSessionManager,
  type PluginRegistry,
  type LifecycleManager,
} from "@aoagents/ao-core";
import { importPluginModuleFromSource } from "./plugin-store.js";

const registryPromises = new Map<string, Promise<PluginRegistry>>();
// The config each cached registry was built from. `loadFromConfig` mutates it,
// rewriting inline-external plugin references (`project.tracker/scm.plugin`,
// `notifier.plugin`) from any inferred temporary name to the real manifest.name.
// A freshly loaded config (e.g. every backlog poll) carries the temp names again,
// so on a cache hit we reconcile it from this built config — otherwise
// `registry.get(tempName)` returns null and the project is skipped/fails.
const builtConfigs = new Map<string, OrchestratorConfig>();

function getRegistryCacheKey(config: OrchestratorConfig): string {
  // A merged scope (global ∪ startup-only config) sets `_registryScopeKey` so its
  // registry — built with the startup config's extra plugin declarations — is
  // cached apart from the plain global config's registry (which shares the same
  // `configPath`). See OrchestratorConfig._registryScopeKey.
  return config._registryScopeKey || config.configPath || "__default__";
}

/**
 * Copy resolved inline-external plugin names from a previously-built config onto
 * a fresh one, so the fresh config's plugin references match what the cached
 * registry actually registered under (the real manifest.name, not an inferred
 * temporary name). Keyed by the structured external-entry location.
 */
function reconcileExternalPluginNames(target: OrchestratorConfig, source: OrchestratorConfig): void {
  for (const entry of source._externalPluginEntries ?? []) {
    const loc = entry.location;
    if (loc.kind === "project") {
      const resolved = source.projects[loc.projectId]?.[loc.configType]?.plugin;
      const targetSlot = target.projects[loc.projectId]?.[loc.configType];
      if (resolved && targetSlot) targetSlot.plugin = resolved;
    } else if (loc.kind === "notifier") {
      const resolved = source.notifiers?.[loc.notifierId]?.plugin;
      const targetNotifier = target.notifiers?.[loc.notifierId];
      if (resolved && targetNotifier) targetNotifier.plugin = resolved;
    }
  }
}

/**
 * Get or create the plugin registry.
 * Caches the Promise (not the resolved value) so concurrent callers
 * await the same initialization rather than racing.
 */
export async function getPluginRegistry(config: OrchestratorConfig): Promise<PluginRegistry> {
  const cacheKey = getRegistryCacheKey(config);
  let registryPromise = registryPromises.get(cacheKey);

  if (!registryPromise) {
    registryPromise = (async () => {
      const registry = createPluginRegistry();
      // Prefer the AO-managed plugin store when a package is installed there,
      // but still fall back to the CLI/workspace dependency tree for built-ins.
      await registry.loadFromConfig(config, importPluginModuleFromSource);
      // `config` is now mutated with resolved manifest names — keep it so later
      // (cache-hit) callers with a fresh config can be reconciled against it.
      builtConfigs.set(cacheKey, config);
      return registry;
    })().catch((err) => {
      registryPromises.delete(cacheKey);
      builtConfigs.delete(cacheKey);
      throw err;
    });
    registryPromises.set(cacheKey, registryPromise);
  }

  const registry = await registryPromise;
  // Cache hit with a different (freshly loaded) config object: reconcile its
  // inline-external plugin references so they match the registered manifest names.
  const builtConfig = builtConfigs.get(cacheKey);
  if (builtConfig && builtConfig !== config) {
    reconcileExternalPluginNames(config, builtConfig);
  }
  return registry;
}

/**
 * Create a SessionManager backed by core's implementation.
 * Initializes the plugin registry from config and wires everything up.
 */
export async function getSessionManager(
  config: OrchestratorConfig,
): Promise<OpenCodeSessionManager> {
  const registry = await getPluginRegistry(config);
  return createSessionManager({ config, registry });
}

/**
 * Create a LifecycleManager backed by core's implementation.
 * Shares the same plugin registry initialization path as SessionManager.
 */
export async function getLifecycleManager(
  config: OrchestratorConfig,
  projectId?: string,
): Promise<LifecycleManager> {
  const registry = await getPluginRegistry(config);
  const sessionManager = createSessionManager({ config, registry });
  return createLifecycleManager({ config, registry, sessionManager, projectId });
}
