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
  EVENT_PRIORITIES,
  generateSessionPrefix,
  getGlobalConfigPath,
  loadConfig,
  type EventPriority,
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
 * Build the COMPLETE per-priority notification routing for a carried startup-only
 * project. Precedence per priority: explicit per-project route → startup
 * top-level route → startup `defaults.notifiers`. Filling every priority means
 * the lifecycle manager's per-project lookup always resolves to the startup
 * project's own notifiers instead of falling through to the merged/global
 * `defaults.notifiers` — the case where a startup project relies on default
 * notifiers rather than an explicit `notificationRouting`.
 */
function bakeStartupNotificationRouting(
  project: ProjectConfig,
  startupConfig: OrchestratorConfig,
): Record<EventPriority, string[]> {
  const projectRouting: Partial<Record<EventPriority, string[]>> = project.notificationRouting ?? {};
  const topRouting: Partial<Record<EventPriority, string[]>> =
    startupConfig.notificationRouting ?? {};
  const defaultNotifiers = startupConfig.defaults?.notifiers ?? [];
  const routing = {} as Record<EventPriority, string[]>;
  for (const priority of EVENT_PRIORITIES) {
    routing[priority] = projectRouting[priority] ?? topRouting[priority] ?? defaultNotifiers;
  }
  return routing;
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
  // Merge budget caps FIELD-BY-FIELD (per-project cap wins, else startup
  // top-level). A whole-object fallback would drop a startup top-level cap when
  // the project sets only the other field — and since `resolveBudget` does not
  // inherit the global budget for carried (sourceConfigPath) projects, that
  // missing cap would silently disable enforcement.
  const budget: ProjectConfig["budget"] =
    project.budget || startupConfig.budget
      ? {
          perSessionUsd: project.budget?.perSessionUsd ?? startupConfig.budget?.perSessionUsd,
          perProjectUsd: project.budget?.perProjectUsd ?? startupConfig.budget?.perProjectUsd,
        }
      : undefined;
  return {
    ...project,
    runtime: project.runtime ?? defaults.runtime,
    agent: project.agent ?? defaults.agent,
    workspace: project.workspace ?? defaults.workspace,
    worker: { ...project.worker, agent: workerAgent },
    orchestrator: { ...project.orchestrator, agent: orchestratorAgent },
    ...(reactions ? { reactions } : {}),
    notificationRouting: bakeStartupNotificationRouting(project, startupConfig),
    // Field-merge the project lifecycle over the startup top-level (so a partial
    // project override keeps the startup config's other fields, not the global).
    lifecycle: {
      autoCleanupOnMerge:
        project.lifecycle?.autoCleanupOnMerge ?? startupConfig.lifecycle?.autoCleanupOnMerge,
      mergeCleanupIdleGraceMs:
        project.lifecycle?.mergeCleanupIdleGraceMs ?? startupConfig.lifecycle?.mergeCleanupIdleGraceMs,
    },
    // Carry the startup config's idle/ready threshold so the scoped lifecycle
    // worker supervises this project with its own threshold, not the global one.
    readyThresholdMs: project.readyThresholdMs ?? startupConfig.readyThresholdMs,
    ...(budget ? { budget } : {}),
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

/** True when a plugin's `name` is an inferred temporary basename (an inline
 *  external plugin declared without an explicit `plugin`), rather than its final
 *  registry name. `loadConfig` fills such names from the package/path basename
 *  and the registry replaces them with the real manifest.name at load time, so
 *  they must NOT be treated as the final `slot:name` for collision detection. */
function hasInferredName(
  plugin: InstalledPluginConfig,
  inferredIdentities: Set<string>,
): boolean {
  if (plugin.package && inferredIdentities.has(`package:${plugin.package}`)) return true;
  if (plugin.path && inferredIdentities.has(`path:${plugin.path}`)) return true;
  return false;
}

/** Append startup plugin declarations, absolutizing any relative local `path`
 *  against the startup config dir, with two guards against the registry's
 *  `slot:name` instance keying:
 *   - dedupe by implementation identity (source + path/package) against ENABLED
 *     global plugins, so a redundant re-declaration is skipped (but a startup
 *     plugin whose global namesake is DISABLED is still carried so it loads);
 *   - reject a same-NAME / different-identity collision against an enabled global
 *     plugin — keeping both would have the startup plugin silently overwrite the
 *     global implementation for registered projects, so fail loudly instead. The
 *     name check is skipped when EITHER side's name is an inferred temporary
 *     basename: those are replaced with distinct `slot:manifest.name` keys at
 *     load time, so a basename clash is not a real registry collision. */
function mergeInstalledPlugins(
  globalPlugins: InstalledPluginConfig[] | undefined,
  startupPlugins: InstalledPluginConfig[] | undefined,
  startupConfigPath: string,
  inferredIdentities: Set<string>,
): InstalledPluginConfig[] | undefined {
  if (!startupPlugins?.length) return globalPlugins;
  const enabledGlobal = (globalPlugins ?? []).filter((p) => p.enabled !== false);
  const seenIdentity = new Set(enabledGlobal.map(pluginIdentityKey));
  const enabledGlobalByName = new Map(enabledGlobal.map((p) => [p.name, p] as const));
  const merged = [...(globalPlugins ?? [])];
  for (const plugin of startupPlugins) {
    const baked =
      plugin.source === "local" && plugin.path
        ? { ...plugin, path: absolutizeLocalPluginPath(plugin.path, startupConfigPath) }
        : plugin;
    const key = pluginIdentityKey(baked);
    if (seenIdentity.has(key)) continue; // Same implementation already present (enabled).
    const collidingGlobal = enabledGlobalByName.get(baked.name);
    // Only a clash of FINAL (non-inferred) names is a real registry collision —
    // use the raw `plugin` (pre-absolutization) so its package/path matches the
    // external-entry identities collected from the startup config.
    if (
      collidingGlobal &&
      !hasInferredName(plugin, inferredIdentities) &&
      !hasInferredName(collidingGlobal, inferredIdentities)
    ) {
      throw new Error(
        `Plugin name collision in merged scope: plugin "${baked.name}" is declared by both ` +
          `the global config (${pluginIdentityKey(collidingGlobal)}) and the startup config ` +
          `(${key}). They map to the same registry slot and would overwrite each other. ` +
          `Rename one of the plugins before launching ao start.`,
      );
    }
    seenIdentity.add(key);
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
  // Set when `startupConfig` is a cached (last-known) copy because the source
  // file is currently unreadable. Carried projects are then supervision/shutdown
  // -only: they must not be SPAWNED into, since a new worker would get an
  // `AO_CONFIG_PATH` pointing at the missing file and fail to resolve its config.
  startupFromCache = false,
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
    const baked = bakeStartupOnlyProject(project, startupConfig);
    projects[id] = startupFromCache ? { ...baked, _spawnPaused: true } : baked;
    carriedProjectIds.add(id);
  }

  if (carriedProjectIds.size === 0) {
    // Nothing startup-only — the global config already covers the full scope.
    // The daemon still runs on the STARTUP config's port, so workers it spawns
    // must talk to it (not the global registry's port).
    return { ...globalConfig, projects, port: startupConfig.port };
  }

  // Startup notifier aliases not already defined globally. Their definitions are
  // merged below so a carried project's baked `notificationRouting` can resolve
  // them; on a name collision the global definition wins (it governs the global
  // projects), so colliding startup notifier external entries are NOT applied.
  const globalNotifiers = globalConfig.notifiers ?? {};
  const startupNotifiers = startupConfig.notifiers ?? {};
  const startupOnlyNotifierIds = new Set(
    Object.keys(startupNotifiers).filter((id) => !(id in globalNotifiers)),
  );

  // A notifier alias defined in BOTH configs is a collision: the merged top-level
  // notifiers keep the GLOBAL definition, so a carried project routing to that
  // alias would silently notify the global target instead of its startup one.
  // Reject when a carried project actually routes to such an alias.
  const collidingNotifierAliases = new Set(
    Object.keys(startupNotifiers).filter((id) => id in globalNotifiers),
  );
  if (collidingNotifierAliases.size > 0) {
    for (const id of carriedProjectIds) {
      const routing = projects[id].notificationRouting;
      if (!routing) continue;
      for (const aliases of Object.values(routing)) {
        for (const alias of aliases) {
          if (collidingNotifierAliases.has(alias)) {
            throw new Error(
              `Notifier alias collision in merged scope: carried project "${id}" routes ` +
                `notifications to "${alias}", which is defined in BOTH the startup and global ` +
                `configs. The merged scope keeps the global definition, so "${id}" would notify ` +
                `the wrong target. Rename the startup notifier alias before launching ao start.`,
            );
          }
        }
      }
    }
  }

  // External plugin entries that omit an explicit `plugin` carry an inferred
  // temporary name (basename), which the registry rewrites to the real manifest
  // name at load time. Track their identities so the plugin-name collision check
  // ignores basename clashes that aren't real `slot:manifest.name` collisions.
  const inferredPluginIdentities = new Set<string>();
  for (const entry of [
    ...(globalConfig._externalPluginEntries ?? []),
    ...(startupConfig._externalPluginEntries ?? []),
  ]) {
    if (entry.expectedPluginName !== undefined) continue; // explicit `plugin` → final name
    if (entry.package) inferredPluginIdentities.add(`package:${entry.package}`);
    else if (entry.path) inferredPluginIdentities.add(`path:${entry.path}`);
  }

  return {
    ...globalConfig,
    projects,
    // The daemon was started from `startupConfig` and binds its port, so the
    // merged scope must carry that port — workers (global or startup-only) spawn
    // with `AO_PORT` from `config.port` and must reach THIS daemon, not the port
    // recorded in the global registry.
    port: startupConfig.port,
    // Add startup-only notifier definitions (global wins on alias collision) so a
    // carried project routing to a startup alias doesn't hit `target_missing`.
    notifiers: { ...startupNotifiers, ...globalNotifiers },
    plugins: mergeInstalledPlugins(
      globalConfig.plugins,
      startupConfig.plugins,
      startupConfig.configPath,
      inferredPluginIdentities,
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
 * Last successfully-loaded startup config, keyed by its path. Lets a transient
 * disappearance of the startup file preserve its carried startup-only projects in
 * the merged scope (see {@link loadMergedScopeConfig}).
 */
const lastStartupConfig = new Map<string, OrchestratorConfig>();

/** Test-only: clear the cached last-known startup configs. */
export function __resetMergedScopeCache(): void {
  lastStartupConfig.clear();
}

/**
 * Load the config spanning both the global registry and the config that started
 * this `ao start` process. Falls back to the startup config alone when no global
 * config exists (first-run `ao start <url>` / `<path>`).
 *
 * The global config is loaded FIRST so a startup config removed/unreadable at
 * runtime can't break the poller or the SIGINT shutdown path. When the startup
 * config can't be read, the LAST-KNOWN startup config (if any) is reused so its
 * carried startup-only projects stay in scope — otherwise shutdown wouldn't
 * enumerate/kill their sessions and the supervisor would detach their lifecycle
 * workers as "removed". Only when no startup config has ever loaded does the
 * scope shrink to the plain global registry.
 */
export function loadMergedScopeConfig(startupConfigPath: string): OrchestratorConfig {
  const globalConfigPath = getGlobalConfigPath();
  let globalConfig: OrchestratorConfig | undefined;
  try {
    globalConfig = loadConfig(globalConfigPath);
  } catch (error) {
    // A missing global config is fine (first-run startup-only). Any other error
    // (corrupt global) is fatal — the global registry is the merge base.
    if (!isMissingGlobalConfig(error, globalConfigPath)) throw error;
  }

  let startupConfig = lastStartupConfig.get(startupConfigPath);
  let startupFromCache = false;
  let startupError: unknown;
  try {
    startupConfig = loadConfig(startupConfigPath);
    lastStartupConfig.set(startupConfigPath, startupConfig); // remember the last good load
  } catch (error) {
    // Startup config removed/unreadable at runtime: fall through with the cached
    // last-known startup config (if any) so its carried projects survive. Mark it
    // cache-sourced so carried projects are supervision/shutdown-only (not spawned
    // into with a stale AO_CONFIG_PATH).
    startupError = error;
    startupFromCache = startupConfig !== undefined;
  }

  if (!startupConfig) {
    // Never loaded the startup config successfully. Keep the daemon working over
    // the registered projects if the global registry exists; otherwise fail.
    if (globalConfig) return globalConfig;
    throw startupError ?? new Error(`Unable to load startup config at ${startupConfigPath}`);
  }

  if (!globalConfig) return startupConfig;
  return mergeScopeProjects(globalConfig, startupConfig, startupFromCache);
}
