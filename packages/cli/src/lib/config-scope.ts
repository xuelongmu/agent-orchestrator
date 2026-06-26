/**
 * Config scope resolution shared by the headless backlog poller and the
 * graceful-shutdown handler.
 *
 * Both run inside the long-lived `ao start` daemon and must reason about the
 * full set of projects this process is responsible for. That set is the union
 * of two configs:
 *
 *  - the **global registry** (`~/.agent-orchestrator/config.yaml`), which lists
 *    every registered project — the scope the poller spawns into and `ao stop`
 *    kills across; and
 *  - the **startup config** (`ctx.configPath`) that launched this process, which
 *    may be a non-canonical local/wrapped/URL-generated config whose project is
 *    not (yet) in the global registry.
 *
 * Loading only the global config drops the started project (the poller never
 * claims its backlog; shutdown never kills its sessions). Loading only the
 * startup config drops the poller's global-scoped sessions. So we union them.
 */

import {
  getGlobalConfigPath,
  loadConfig,
  type OrchestratorConfig,
  type ProjectConfig,
} from "@aoagents/ao-core";

function isMissingGlobalConfig(error: unknown, globalConfigPath: string): boolean {
  return (
    error instanceof Error &&
    (error as NodeJS.ErrnoException).code === "ENOENT" &&
    (error as Error & { path?: string }).path === globalConfigPath
  );
}

/**
 * Merge the startup config's projects into the global config. Projects already
 * present in the global registry keep their canonical global entry. A project
 * that exists only in the startup config is carried over with the startup
 * config's defaults baked in — otherwise the merged config exposes the *global*
 * defaults, and a startup-only project that omitted `runtime`/`agent`/
 * `workspace` (relying on its local defaults) would resolve the wrong plugins.
 * `session-manager.kill()` and `spawn()` resolve plugins via
 * `project.workspace ?? config.defaults.workspace`, so baking the startup
 * defaults onto the project entry keeps that resolution correct regardless of
 * which config's defaults the merged object carries.
 */
function mergeScopeProjects(
  globalConfig: OrchestratorConfig,
  startupConfig: OrchestratorConfig,
): OrchestratorConfig {
  const projects: Record<string, ProjectConfig> = { ...globalConfig.projects };
  for (const [id, project] of Object.entries(startupConfig.projects)) {
    if (id in projects) continue; // Registered project — keep the global entry + defaults.
    projects[id] = {
      ...project,
      runtime: project.runtime ?? startupConfig.defaults.runtime,
      agent: project.agent ?? startupConfig.defaults.agent,
      workspace: project.workspace ?? startupConfig.defaults.workspace,
    };
  }
  return { ...globalConfig, projects };
}

/**
 * Load the config spanning both the global registry and the config that started
 * this `ao start` process. Falls back to the startup config alone when no global
 * config exists (first-run `ao start <url>` / `<path>`).
 */
export function loadMergedScopeConfig(startupConfigPath: string): OrchestratorConfig {
  const startupConfig = loadConfig(startupConfigPath);
  const globalConfigPath = getGlobalConfigPath();
  let globalConfig: OrchestratorConfig;
  try {
    globalConfig = loadConfig(globalConfigPath);
  } catch (error) {
    if (isMissingGlobalConfig(error, globalConfigPath)) return startupConfig;
    throw error;
  }
  return mergeScopeProjects(globalConfig, startupConfig);
}
