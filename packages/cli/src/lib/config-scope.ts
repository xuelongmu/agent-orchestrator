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
import { pathsEqual } from "./path-equality.js";

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
 * Startup-WIDE policy (`reactions`, `notificationRouting`, `lifecycle`, `budget`)
 * is also baked onto the project as per-project overrides. The merged config
 * otherwise spreads the GLOBAL top-level policy, so without this a startup-only
 * project's lifecycle worker would silently run the unrelated global reaction/
 * routing/merge-cleanup/budget policy instead of the one declared in the config
 * that launched `ao start`. The lifecycle manager honors these per-project fields.
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
    budget: project.budget ?? startupConfig.budget,
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

/**
 * Identity of a plugin *implementation* — what determines which module loads,
 * NOT the logical config name. Keyed by source + path/package so a startup local
 * plugin is never conflated with a same-named global npm/registry plugin (or a
 * global local plugin in a different directory).
 */
function pluginIdentityKey(plugin: InstalledPluginConfig): string {
  if (plugin.source === "local") return `local:${plugin.path ?? plugin.name}`;
  return `${plugin.source}:${plugin.package ?? plugin.name}`;
}

/** Append startup plugin declarations, deduped by implementation identity
 *  (source + path/package) against the ENABLED global plugins, and absolutizing
 *  any relative local `path` against the startup config dir. Deduping by name
 *  alone would drop a startup plugin whose global namesake is disabled or points
 *  at a different package/path, leaving a carried startup-only project unable to
 *  resolve its tracker/agent/workspace. */
function mergeInstalledPlugins(
  globalPlugins: InstalledPluginConfig[] | undefined,
  startupPlugins: InstalledPluginConfig[] | undefined,
  startupConfigPath: string,
): InstalledPluginConfig[] | undefined {
  if (!startupPlugins?.length) return globalPlugins;
  // Only an ENABLED global entry with the same identity makes a startup
  // declaration redundant — a disabled global entry would be skipped by the
  // registry, so the startup one must still be carried to actually load.
  const seen = new Set(
    (globalPlugins ?? []).filter((p) => p.enabled !== false).map(pluginIdentityKey),
  );
  const merged = [...(globalPlugins ?? [])];
  for (const plugin of startupPlugins) {
    const baked =
      plugin.source === "local" && plugin.path
        ? { ...plugin, path: absolutizeLocalPluginPath(plugin.path, startupConfigPath) }
        : plugin;
    const key = pluginIdentityKey(baked);
    if (seen.has(key)) continue;
    seen.add(key);
    merged.push(baked);
  }
  return merged;
}

/**
 * Select the startup external plugin entries that actually belong in the merged
 * scope, and absolutize their relative local `path` (so they match their
 * absolutized `plugins` entry and resolve against the startup config dir).
 *
 * The registry uses these entries to rewrite `config.projects[id].tracker/scm.plugin`
 * and `config.notifiers[id].plugin` during manifest resolution. An entry whose
 * target stayed the GLOBAL entry — a project skipped because its id is already
 * registered, or a notifier alias that collides with a global one — must be
 * dropped, otherwise the startup config's plugin would clobber the registered
 * project's tracker/scm or the global notifier.
 */
function scopeStartupExternalEntries(
  entries: ExternalPluginEntryRef[] | undefined,
  startupConfigPath: string,
  carriedProjectIds: Set<string>,
  startupOnlyNotifierIds: Set<string>,
): ExternalPluginEntryRef[] {
  if (!entries?.length) return [];
  const scoped: ExternalPluginEntryRef[] = [];
  for (const entry of entries) {
    const loc = entry.location;
    if (loc.kind === "project" && !carriedProjectIds.has(loc.projectId)) continue;
    if (loc.kind === "notifier" && !startupOnlyNotifierIds.has(loc.notifierId)) continue;
    scoped.push(
      entry.path && !entry.package
        ? { ...entry, path: absolutizeLocalPluginPath(entry.path, startupConfigPath) }
        : entry,
    );
  }
  return scoped;
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

  const carriedProjectIds = new Set<string>();
  for (const [id, project] of Object.entries(startupConfig.projects)) {
    if (id in projects) {
      // Same id in both configs. Only treat it as the SAME (registered) project
      // when they point at the same path. Otherwise the startup config defines a
      // DIFFERENT project under a colliding id — silently keeping the global
      // entry would make the supervisor/poller/shutdown manage the startup
      // project's already-spawned sessions with the wrong path/tracker/defaults.
      // Fail loudly rather than disambiguate silently.
      const globalProject = projects[id];
      if (!pathsEqual(globalProject.path, project.path)) {
        throw new Error(
          `Project id collision in merged scope: "${id}" is defined in both the global ` +
            `registry (path "${globalProject.path}") and the startup config ` +
            `(path "${project.path}"). Rename the startup project's id (or register it) ` +
            `before launching ao start.`,
        );
      }
      continue; // Genuinely the same registered project — keep the global entry + defaults.
    }
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
    carriedProjectIds.add(id);
  }

  if (carriedProjectIds.size === 0) {
    // Nothing startup-only — the global config already covers the full scope.
    return { ...globalConfig, projects };
  }

  // Startup notifier aliases not already defined globally. Their definitions are
  // merged below so a carried project's baked `notificationRouting` can resolve
  // them; on a name collision the global definition wins (it governs the global
  // projects), so colliding startup notifier external entries are NOT applied.
  const startupOnlyNotifierIds = new Set(
    Object.keys(startupConfig.notifiers ?? {}).filter(
      (id) => !(id in (globalConfig.notifiers ?? {})),
    ),
  );

  return {
    ...globalConfig,
    projects,
    // Add startup-only notifier definitions (global wins on alias collision) so a
    // carried project routing to a startup alias doesn't hit `target_missing`.
    notifiers: { ...(startupConfig.notifiers ?? {}), ...(globalConfig.notifiers ?? {}) },
    plugins: mergeInstalledPlugins(
      globalConfig.plugins,
      startupConfig.plugins,
      startupConfig.configPath,
    ),
    _externalPluginEntries: [
      ...(globalConfig._externalPluginEntries ?? []),
      ...scopeStartupExternalEntries(
        startupConfig._externalPluginEntries,
        startupConfig.configPath,
        carriedProjectIds,
        startupOnlyNotifierIds,
      ),
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
