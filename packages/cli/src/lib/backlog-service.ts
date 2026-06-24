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
import { getPluginRegistry, getSessionManager } from "./create-session-manager.js";

let activePoller: BacklogPoller | null = null;

/**
 * Resolve the freshest services for a poll cycle. Prefers the global registry
 * (so all projects are visible); falls back to the caller-resolved config path
 * when the global config is absent (first-run `ao start <url>` / `<path>`).
 */
function loadBacklogConfig(configPath?: string): OrchestratorConfig {
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
      return configPath ? loadConfig(configPath) : loadConfig();
    }
    throw error;
  }
}

/**
 * Start the backlog poller. Idempotent — safe to call multiple times.
 *
 * @param configPath Resolved config path from the caller (used as the
 *   local fallback source when the global config is missing).
 */
export function startBacklogPoller(configPath?: string): void {
  if (activePoller) return;

  activePoller = createBacklogPoller({
    resolveServices: async (): Promise<BacklogServices> => {
      const config = loadBacklogConfig(configPath);
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

export function stopBacklogPoller(): void {
  activePoller?.stop();
  activePoller = null;
}
