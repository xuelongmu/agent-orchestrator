/**
 * CLI-side backlog poller wiring.
 *
 * Runs the core backlog poller inside the `ao start` daemon so labeled backlog
 * issues are auto-spawned even with no dashboard open (headless autonomy). The
 * poller's cross-process lock keeps the CLI and dashboard from double-spawning
 * the same issue when both run.
 */

import {
  createBacklogPoller,
  loadConfig,
  getGlobalConfigPath,
  type BacklogPoller,
  type BacklogServices,
  type OrchestratorConfig,
} from "@aoagents/ao-core";
import { loadMergedScopeConfig } from "./config-scope.js";
import { getPluginRegistry, getSessionManager } from "./create-session-manager.js";

let activePoller: BacklogPoller | null = null;

/**
 * Resolve the freshest services for a poll cycle. Unions the global registry
 * (so all registered projects are visible) with the config that started this
 * daemon, so a project launched from a non-canonical local/wrapped/URL config —
 * one that isn't in the global registry yet — still has its `agent:backlog`
 * issues auto-claimed. Falls back to the global registry, then cwd, when no
 * startup config path is known.
 */
function loadBacklogConfig(configPath?: string): OrchestratorConfig {
  if (configPath) return loadMergedScopeConfig(configPath);
  const globalConfigPath = getGlobalConfigPath();
  try {
    return loadConfig(globalConfigPath);
  } catch (error) {
    if (
      error instanceof Error &&
      "code" in error &&
      (error as NodeJS.ErrnoException).code === "ENOENT" &&
      "path" in error &&
      (error as Error & { path?: string }).path === globalConfigPath
    ) {
      return loadConfig();
    }
    throw error;
  }
}

/**
 * Start the backlog poller. Idempotent — safe to call multiple times.
 *
 * @param configPath Resolved config path from the caller (used as the
 *   local fallback source when the global config is missing).
 * @param resolvedPort The port the daemon ACTUALLY bound. When the requested
 *   port is busy, `ao start` falls back to a free one but the config file still
 *   records the original value; spawned backlog sessions must inherit `AO_PORT`
 *   for the port the dashboard actually bound, not the stale requested value the
 *   reloaded config carries.
 */
export function startBacklogPoller(configPath?: string, resolvedPort?: number): void {
  if (activePoller) return;

  activePoller = createBacklogPoller({
    resolveServices: async (): Promise<BacklogServices> => {
      const config = loadBacklogConfig(configPath);
      // Override the reloaded config's port with the port the daemon actually
      // bound (applied every cycle since the config is re-read each poll).
      if (resolvedPort !== undefined) config.port = resolvedPort;
      const registry = await getPluginRegistry(config);
      const sessionManager = await getSessionManager(config);
      return { config, registry, sessionManager };
    },
    logger: {
      info: (message) => console.log(message),
      error: (message, err) => console.error(message, err),
    },
  });

  activePoller.start();
}

/**
 * Stop the backlog poller. Returns a promise that resolves once any in-flight
 * poll (including a spawn started just before shutdown) has settled, so the
 * graceful-stop path can await it before enumerating sessions to kill.
 */
export function stopBacklogPoller(): Promise<void> {
  const stopped = activePoller?.stop() ?? Promise.resolve();
  activePoller = null;
  return stopped;
}
