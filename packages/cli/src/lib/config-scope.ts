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
 *
 * A single `OrchestratorConfig` carries one set of top-level `defaults`,
 * `plugins`, and `configPath` — the global ones, since the global config is the
 * merge base. A project that exists ONLY in the startup config would otherwise
 * resolve its plugins/agents against the wrong (global) defaults and point its
 * workers at the wrong config path. To avoid that we resolve the startup-only
 * project's plugin selections against the startup config up front and bake the
 * concrete values onto the project entry, carry its source config path, and
 * merge the startup config's external plugin declarations into the result.
 */

import {
  getGlobalConfigPath,
  loadConfig,
  type DefaultPlugins,
  type InstalledPluginConfig,
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
 * Resolve a startup-only project against the startup config's defaults and bake
 * the concrete selections onto the project entry, so the merged config's
 * (global) defaults are never consulted for it.
 *
 * Worker/orchestrator agents are resolved with the same precedence
 * `agent-selection` uses (`role.agent -> project.agent -> defaults.role.agent ->
 * defaults.agent`) and pinned onto `project.worker`/`project.orchestrator`.
 * Pinning the *fully resolved* value (rather than just the role default) is
 * required: an explicit `project.agent` outranks a role default, so copying the
 * role default alone could shadow it.
 */
function bakeStartupOnlyProject(
  project: ProjectConfig,
  defaults: DefaultPlugins,
  startupConfigPath: string,
): ProjectConfig {
  const workerAgent =
    project.worker?.agent ?? project.agent ?? defaults.worker?.agent ?? defaults.agent;
  const orchestratorAgent =
    project.orchestrator?.agent ?? project.agent ?? defaults.orchestrator?.agent ?? defaults.agent;
  return {
    ...project,
    runtime: project.runtime ?? defaults.runtime,
    agent: project.agent ?? defaults.agent,
    workspace: project.workspace ?? defaults.workspace,
    worker: { ...project.worker, agent: workerAgent },
    orchestrator: { ...project.orchestrator, agent: orchestratorAgent },
    sourceConfigPath: project.sourceConfigPath ?? startupConfigPath,
  };
}

/** Append startup plugin declarations not already present (dedup by name). */
function mergeInstalledPlugins(
  globalPlugins: InstalledPluginConfig[] | undefined,
  startupPlugins: InstalledPluginConfig[] | undefined,
): InstalledPluginConfig[] | undefined {
  if (!startupPlugins?.length) return globalPlugins;
  const seen = new Set((globalPlugins ?? []).map((p) => p.name));
  const merged = [...(globalPlugins ?? [])];
  for (const plugin of startupPlugins) {
    if (seen.has(plugin.name)) continue;
    seen.add(plugin.name);
    merged.push(plugin);
  }
  return merged;
}

/**
 * Merge the startup config's projects into the global config. Projects already
 * present in the global registry keep their canonical global entry; a project
 * that exists only in the startup config is carried over via
 * {@link bakeStartupOnlyProject}. When such a project is carried over, the
 * startup config's plugin declarations (`plugins` + `_externalPluginEntries`)
 * are merged in too, so the registry built from the merged config can resolve a
 * startup-only project's external tracker/scm/agent plugins.
 */
function mergeScopeProjects(
  globalConfig: OrchestratorConfig,
  startupConfig: OrchestratorConfig,
): OrchestratorConfig {
  const projects: Record<string, ProjectConfig> = { ...globalConfig.projects };
  let carriedStartupOnly = false;
  for (const [id, project] of Object.entries(startupConfig.projects)) {
    if (id in projects) continue; // Registered project — keep the global entry + defaults.
    projects[id] = bakeStartupOnlyProject(
      project,
      startupConfig.defaults,
      startupConfig.configPath,
    );
    carriedStartupOnly = true;
  }

  if (!carriedStartupOnly) {
    // Nothing startup-only — the global config already covers the full scope.
    return { ...globalConfig, projects };
  }

  return {
    ...globalConfig,
    projects,
    plugins: mergeInstalledPlugins(globalConfig.plugins, startupConfig.plugins),
    _externalPluginEntries: [
      ...(globalConfig._externalPluginEntries ?? []),
      ...(startupConfig._externalPluginEntries ?? []),
    ],
  };
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
