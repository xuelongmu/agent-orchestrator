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

import { dirname, isAbsolute, resolve } from "node:path";
import {
  generateSessionPrefix,
  getGlobalConfigPath,
  loadConfig,
  type ExternalPluginEntryRef,
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
 *
 * Startup-WIDE policy (`reactions`, `notificationRouting`, `lifecycle`) is also
 * baked onto the project as per-project overrides. The merged config otherwise
 * spreads the GLOBAL top-level policy, so without this a startup-only project's
 * lifecycle worker would silently run the unrelated global reaction/routing/
 * merge-cleanup policy instead of the one declared in the config that launched
 * `ao start`. The lifecycle manager honors these per-project fields.
 */
function bakeStartupOnlyProject(
  project: ProjectConfig,
  startupConfig: OrchestratorConfig,
): ProjectConfig {
  const defaults = startupConfig.defaults;
  const workerAgent =
    project.worker?.agent ?? project.agent ?? defaults.worker?.agent ?? defaults.agent;
  const orchestratorAgent =
    project.orchestrator?.agent ?? project.agent ?? defaults.orchestrator?.agent ?? defaults.agent;
  // Startup top-level reactions become per-project overrides (an explicit
  // `project.reactions` still wins). The lifecycle manager merges per-project
  // reactions over global, so this scopes the startup policy to this project.
  const reactions: ProjectConfig["reactions"] =
    startupConfig.reactions || project.reactions
      ? { ...(startupConfig.reactions ?? {}), ...(project.reactions ?? {}) }
      : undefined;
  return {
    ...project,
    runtime: project.runtime ?? defaults.runtime,
    agent: project.agent ?? defaults.agent,
    workspace: project.workspace ?? defaults.workspace,
    worker: { ...project.worker, agent: workerAgent },
    orchestrator: { ...project.orchestrator, agent: orchestratorAgent },
    ...(reactions ? { reactions } : {}),
    notificationRouting: project.notificationRouting ?? startupConfig.notificationRouting,
    lifecycle: project.lifecycle ?? startupConfig.lifecycle,
    sourceConfigPath: project.sourceConfigPath ?? startupConfig.configPath,
  };
}

/**
 * Bake a startup config's relative local plugin `path` to an absolute path
 * anchored at the startup config's directory.
 *
 * The merged scope keeps the *global* `configPath`, and the plugin registry
 * resolves local plugin paths relative to `config.configPath`. A startup-only
 * `path: ./plugins/tracker` would therefore be looked up under the global config
 * directory and fail to load. Absolutizing here makes resolution independent of
 * which config path the merged object carries.
 */
function absolutizeLocalPluginPath(path: string, startupConfigPath: string): string {
  return isAbsolute(path) ? path : resolve(dirname(startupConfigPath), path);
}

/** Append startup plugin declarations not already present (dedup by name),
 *  absolutizing any relative local `path` against the startup config dir. */
function mergeInstalledPlugins(
  globalPlugins: InstalledPluginConfig[] | undefined,
  startupPlugins: InstalledPluginConfig[] | undefined,
  startupConfigPath: string,
): InstalledPluginConfig[] | undefined {
  if (!startupPlugins?.length) return globalPlugins;
  const seen = new Set((globalPlugins ?? []).map((p) => p.name));
  const merged = [...(globalPlugins ?? [])];
  for (const plugin of startupPlugins) {
    if (seen.has(plugin.name)) continue;
    seen.add(plugin.name);
    merged.push(
      plugin.source === "local" && plugin.path
        ? { ...plugin, path: absolutizeLocalPluginPath(plugin.path, startupConfigPath) }
        : plugin,
    );
  }
  return merged;
}

/** Absolutize relative local `path` on startup external plugin entries so they
 *  match their (absolutized) `plugins` entry and resolve against the startup
 *  config dir rather than the merged scope's global configPath. */
function absolutizeExternalEntries(
  entries: ExternalPluginEntryRef[] | undefined,
  startupConfigPath: string,
): ExternalPluginEntryRef[] {
  if (!entries?.length) return [];
  return entries.map((entry) =>
    entry.path && !entry.package
      ? { ...entry, path: absolutizeLocalPluginPath(entry.path, startupConfigPath) }
      : entry,
  );
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

  // Session prefixes already in use by the global (registered) projects. The
  // session manager derives session ids, tmux names, and `session/<prefix>-N`
  // branches from this prefix, so a startup-only project that collides with a
  // registered one would clobber the registered project's sessions/branches.
  // loadConfig only validates each config in isolation, so re-run the uniqueness
  // check across the merged set.
  const usedPrefixes = new Set<string>();
  for (const [id, project] of Object.entries(globalConfig.projects)) {
    usedPrefixes.add(project.sessionPrefix || generateSessionPrefix(id));
  }

  let carriedStartupOnly = false;
  for (const [id, project] of Object.entries(startupConfig.projects)) {
    if (id in projects) continue; // Registered project — keep the global entry + defaults.
    const prefix = project.sessionPrefix || generateSessionPrefix(id);
    if (usedPrefixes.has(prefix)) {
      // Abort rather than silently drop: `runStartup` may already have spawned
      // the dashboard/orchestrator (and soon workers) for this startup config
      // before the supervisor/poller/shutdown call this. Omitting the project
      // would leave those just-created sessions unenumerated — unsupervised, and
      // not killed/restored on Ctrl+C. Fail loudly so the collision is fixed.
      throw new Error(
        `Session prefix collision in merged scope: startup-only project "${id}" uses ` +
          `sessionPrefix "${prefix}", which is already used by a registered project. ` +
          `Session ids, tmux names, and "session/${prefix}-N" branches derive from this prefix, ` +
          `so carrying "${id}" would clobber the registered project's work. ` +
          `Set a unique sessionPrefix for "${id}" before launching ao start.`,
      );
    }
    usedPrefixes.add(prefix);
    projects[id] = bakeStartupOnlyProject(project, startupConfig);
    carriedStartupOnly = true;
  }

  if (!carriedStartupOnly) {
    // Nothing startup-only — the global config already covers the full scope.
    return { ...globalConfig, projects };
  }

  return {
    ...globalConfig,
    projects,
    plugins: mergeInstalledPlugins(
      globalConfig.plugins,
      startupConfig.plugins,
      startupConfig.configPath,
    ),
    _externalPluginEntries: [
      ...(globalConfig._externalPluginEntries ?? []),
      ...absolutizeExternalEntries(startupConfig._externalPluginEntries, startupConfig.configPath),
    ],
    // Distinct registry cache identity: this scope carries startup-only plugin
    // declarations the plain global config lacks, but keeps the global
    // `configPath`. Without a separate key the CLI registry cache (keyed by
    // configPath) would serve a global-only registry the project supervisor
    // built earlier, and startup-only external plugins would never resolve.
    _registryScopeKey: `${globalConfig.configPath}::+startup:${startupConfig.configPath}`,
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
