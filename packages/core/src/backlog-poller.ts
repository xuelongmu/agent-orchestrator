/**
 * Backlog poller — autonomous spawn-from-issues.
 *
 * Scans each project's tracker for issues labeled `agent:backlog` and spawns a
 * worker session per issue, up to the project's configured concurrency cap.
 * Also relabels reopened issues back into the backlog and labels merged issues
 * for human verification.
 *
 * This logic lives in core (rather than the web process) so that both the
 * headless CLI daemon (`ao start`) and the dashboard can drive the backlog.
 * A cross-process file lock guards the spawn cycle so the two never
 * double-spawn the same issue when run simultaneously: whichever process
 * acquires the lock runs the cycle; the other skips until the next interval.
 */

import {
  closeSync,
  futimesSync,
  mkdirSync,
  openSync,
  rmSync,
  statSync,
  writeSync,
} from "node:fs";
import { dirname, join } from "node:path";
import { getGlobalConfigPath } from "./global-config.js";
import {
  isOrchestratorSession,
  TERMINAL_STATUSES,
  type Issue,
  type OpenCodeSessionManager,
  type OrchestratorConfig,
  type PluginRegistry,
  type ProjectConfig,
  type Session,
  type Tracker,
} from "./types.js";

/** Tracker label marking an issue as ready for autonomous pickup. */
export const BACKLOG_LABEL = "agent:backlog";

/** Tracker label marking a merged issue awaiting human verification. */
export const MERGED_UNVERIFIED_LABEL = "merged-unverified";

/** Default interval between backlog poll cycles (1 minute). */
export const BACKLOG_POLL_INTERVAL_MS = 60_000;

/** Default per-project concurrency cap when `maxConcurrentAgents` is unset. */
export const DEFAULT_MAX_CONCURRENT_AGENTS = 5;

/** Services the poller needs. Compatible with both the CLI and web wiring. */
export interface BacklogServices {
  config: OrchestratorConfig;
  registry: PluginRegistry;
  sessionManager: OpenCodeSessionManager;
}

/** Minimal logging surface — keeps core free of direct console usage. */
export interface BacklogLogger {
  info?(message: string): void;
  error?(message: string, err?: unknown): void;
}

export interface BacklogPollerOptions {
  /** Resolve the freshest config/registry/sessionManager for a poll cycle. */
  resolveServices: () => Promise<BacklogServices>;
  /** Poll interval. Defaults to {@link BACKLOG_POLL_INTERVAL_MS}. */
  intervalMs?: number;
  /**
   * Cross-process lock file path. Defaults to `<global-config-dir>/backlog-poll.lock`
   * so the CLI daemon and dashboard coordinate automatically. Pass `null` to
   * disable locking (single-process use, e.g. tests).
   */
  lockPath?: string | null;
  /** Optional logger. */
  logger?: BacklogLogger;
}

export interface BacklogPoller {
  /** Start the poll loop (immediate run, then on interval). Idempotent. */
  start(): void;
  /**
   * Stop the poll loop and resolve once any in-flight poll has settled, so a
   * spawn started just before shutdown is awaited (and its session enumerated
   * by the graceful-stop path) rather than racing past it.
   */
  stop(): Promise<void>;
  /** Run a single poll cycle. */
  pollOnce(): Promise<void>;
}

interface LockHandle {
  release(): void;
}

/** A held lock is considered stale (and reclaimable) after this long. */
const LOCK_STALE_MS = 5 * 60_000;

/**
 * How often the lock holder refreshes the lock's mtime while a poll runs.
 * Kept well under {@link LOCK_STALE_MS} so a legitimately long poll (slow
 * tracker calls or `sessionManager.spawn()`) is never reclaimed as stale by
 * the peer process mid-cycle.
 */
const LOCK_HEARTBEAT_MS = 60_000;

function defaultLockPath(): string {
  return join(dirname(getGlobalConfigPath()), "backlog-poll.lock");
}

/**
 * Try to acquire the cross-process backlog lock without blocking.
 * Returns a handle if acquired, or null if another process holds a fresh lock.
 */
function tryAcquireLock(lockPath: string): LockHandle | null {
  mkdirSync(dirname(lockPath), { recursive: true });
  try {
    const fd = openSync(lockPath, "wx");
    try {
      writeSync(fd, String(process.pid));
    } catch {
      // Best-effort — the lock's existence is what matters, not its contents.
    }
    // Refresh the lock's mtime periodically so a long-running poll isn't
    // reclaimed as stale by the peer process while we still hold it.
    const heartbeat = setInterval(() => {
      try {
        const now = new Date();
        futimesSync(fd, now, now);
      } catch {
        // Best-effort — a missed refresh just risks an earlier stale reclaim.
      }
    }, LOCK_HEARTBEAT_MS);
    heartbeat.unref?.();
    return {
      release() {
        clearInterval(heartbeat);
        try {
          closeSync(fd);
        } catch {
          // Ignore cleanup races.
        }
        try {
          rmSync(lockPath, { force: true });
        } catch {
          // Best-effort cleanup.
        }
      },
    };
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code !== "EEXIST") {
      // Unexpected FS error — treat as "could not acquire" rather than crash the loop.
      return null;
    }
    // Lock exists. Reclaim it if it is stale (holder likely crashed).
    try {
      const info = statSync(lockPath);
      if (Date.now() - info.mtimeMs > LOCK_STALE_MS) {
        rmSync(lockPath, { force: true });
        return tryAcquireLock(lockPath);
      }
    } catch {
      // The lock vanished between open and stat — let the next cycle retry.
    }
    return null;
  }
}

function getMaxConcurrentAgents(project: ProjectConfig): number {
  const configured = project.maxConcurrentAgents;
  return typeof configured === "number" && configured > 0
    ? configured
    : DEFAULT_MAX_CONCURRENT_AGENTS;
}

/**
 * Label GitHub issues for verification when their PRs have been merged.
 * Mutates `processedIssues` to avoid repeated API calls.
 */
async function labelIssuesForVerification(
  sessions: Session[],
  config: OrchestratorConfig,
  registry: PluginRegistry,
  processedIssues: Set<string>,
  logger?: BacklogLogger,
): Promise<void> {
  const mergedSessions = sessions.filter(
    (s) =>
      s.lifecycle.pr.state === "merged" &&
      s.issueId &&
      !processedIssues.has(`${s.projectId}:${s.issueId}`),
  );

  for (const session of mergedSessions) {
    const key = `${session.projectId}:${session.issueId}`;
    const project = config.projects[session.projectId];
    if (!project?.tracker?.plugin) {
      processedIssues.add(key);
      continue;
    }

    const tracker = registry.get<Tracker>("tracker", project.tracker.plugin);
    if (!tracker?.updateIssue) {
      processedIssues.add(key);
      continue;
    }

    const issueId = session.issueId;
    if (!issueId) {
      processedIssues.add(key);
      continue;
    }

    // Cross-process dedupe: another poller process (CLI daemon vs dashboard)
    // may have already labeled this issue — our in-memory `processedIssues`
    // set is per-process, so check the tracker's authoritative state before
    // posting the verification comment again.
    try {
      const current = await tracker.getIssue(issueId, project);
      if (current.labels.includes(MERGED_UNVERIFIED_LABEL)) {
        processedIssues.add(key);
        continue;
      }
    } catch {
      // getIssue failed — fall through and attempt the update anyway.
    }

    try {
      await tracker.updateIssue(
        issueId,
        {
          labels: [MERGED_UNVERIFIED_LABEL],
          removeLabels: ["agent:backlog", "agent:in-progress"],
          comment: `PR merged. Issue awaiting human verification on staging.`,
        },
        project,
      );
    } catch (err) {
      logger?.error?.(`[backlog] Failed to close issue ${session.issueId}:`, err);
    }
    processedIssues.add(key);
  }
}

/**
 * Detect reopened issues (open + agent:done label) and swap the label
 * back to agent:backlog so the poller picks them up on the next cycle.
 */
async function relabelReopenedIssues(
  config: OrchestratorConfig,
  registry: PluginRegistry,
  logger?: BacklogLogger,
): Promise<void> {
  for (const [, project] of Object.entries(config.projects)) {
    if (!project.tracker?.plugin) continue;
    const tracker = registry.get<Tracker>("tracker", project.tracker.plugin);
    if (!tracker?.listIssues || !tracker.updateIssue) continue;

    let reopened: Issue[];
    try {
      reopened = await tracker.listIssues(
        { state: "open", labels: ["agent:done"], limit: 20 },
        project,
      );
    } catch {
      continue;
    }

    for (const issue of reopened) {
      try {
        await tracker.updateIssue(
          issue.id,
          {
            labels: [BACKLOG_LABEL],
            removeLabels: ["agent:done"],
            comment: "Issue reopened — returning to agent backlog.",
          },
          project,
        );
        logger?.info?.(`[backlog] Relabeled reopened issue ${issue.id} → ${BACKLOG_LABEL}`);
      } catch (err) {
        logger?.error?.(`[backlog] Failed to relabel reopened issue ${issue.id}:`, err);
      }
    }
  }
}

/**
 * Scan each project's backlog and spawn worker sessions up to the project's
 * configured concurrency cap. Duplicate detection skips issues already claimed
 * by a live worker session.
 */
async function spawnFromBacklog(
  allSessions: Session[],
  config: OrchestratorConfig,
  registry: PluginRegistry,
  sessionManager: OpenCodeSessionManager,
  logger?: BacklogLogger,
): Promise<void> {
  const allSessionPrefixes = Object.entries(config.projects).map(
    ([id, p]) => p.sessionPrefix ?? id,
  );
  const workerSessions = allSessions.filter(
    (session) =>
      !isOrchestratorSession(
        session,
        config.projects[session.projectId]?.sessionPrefix ?? session.projectId,
        allSessionPrefixes,
      ) && !TERMINAL_STATUSES.has(session.status),
  );

  // Group active workers per project so the cap and duplicate detection are
  // both scoped per project (per-project concurrency).
  const workersByProject = new Map<string, Session[]>();
  for (const session of workerSessions) {
    const list = workersByProject.get(session.projectId) ?? [];
    list.push(session);
    workersByProject.set(session.projectId, list);
  }

  for (const [projectId, project] of Object.entries(config.projects)) {
    if (!project.tracker?.plugin) continue;

    const projectWorkers = workersByProject.get(projectId) ?? [];
    let availableSlots = getMaxConcurrentAgents(project) - projectWorkers.length;
    if (availableSlots <= 0) continue; // At capacity for this project

    const tracker = registry.get<Tracker>("tracker", project.tracker.plugin);
    if (!tracker?.listIssues) continue;

    const activeIssueIds = new Set(
      projectWorkers
        .map((session) => session.issueId?.toLowerCase())
        .filter((issueId): issueId is string => Boolean(issueId)),
    );

    let backlogIssues: Issue[];
    try {
      backlogIssues = await tracker.listIssues(
        // Request up to the number of free slots so larger per-project caps
        // (maxConcurrentAgents > 10) are honored within a single poll cycle.
        { state: "open", labels: [BACKLOG_LABEL], limit: availableSlots },
        project,
      );
    } catch {
      continue; // Tracker unavailable — skip this project
    }

    for (const issue of backlogIssues) {
      if (availableSlots <= 0) break;

      // Skip if already being worked on
      if (activeIssueIds.has(issue.id.toLowerCase())) continue;

      try {
        await sessionManager.spawn({ projectId, issueId: issue.id });
        availableSlots--;
        activeIssueIds.add(issue.id.toLowerCase());

        // Mark as claimed on the tracker
        if (tracker.updateIssue) {
          await tracker.updateIssue(
            issue.id,
            {
              labels: ["agent:in-progress"],
              removeLabels: ["agent:backlog"],
              comment: "Claimed by agent orchestrator — session spawned.",
            },
            project,
          );
        }
      } catch (err) {
        logger?.error?.(`[backlog] Failed to spawn session for issue ${issue.id}:`, err);
      }
    }
  }
}

/**
 * Create a backlog poller. State (the verification-dedup set, the timer) is
 * held per instance, so each process owns one poller.
 */
export function createBacklogPoller(options: BacklogPollerOptions): BacklogPoller {
  const { resolveServices, logger } = options;
  const intervalMs = options.intervalMs ?? BACKLOG_POLL_INTERVAL_MS;
  const lockPath = options.lockPath === undefined ? defaultLockPath() : options.lockPath;

  // Track which issues we've already labeled for verification to avoid repeats.
  const processedIssues = new Set<string>();

  let timer: ReturnType<typeof setInterval> | undefined;
  let started = false;
  // The currently-running poll, if any. Tracked so `stop()` can await an
  // in-flight spawn before shutdown enumerates sessions to kill.
  let activePoll: Promise<void> | undefined;

  async function pollOnce(): Promise<void> {
    const lock = lockPath ? tryAcquireLock(lockPath) : null;
    if (lockPath && !lock) {
      // Another process is mid-cycle — skip to avoid double-spawning.
      return;
    }

    try {
      const { config, registry, sessionManager } = await resolveServices();

      const allSessions = await sessionManager.list();
      await labelIssuesForVerification(allSessions, config, registry, processedIssues, logger);
      await relabelReopenedIssues(config, registry, logger);
      await spawnFromBacklog(allSessions, config, registry, sessionManager, logger);
    } catch (err) {
      logger?.error?.("[backlog] Poll failed:", err);
    } finally {
      lock?.release();
    }
  }

  // Run a poll while recording it as the active one, so `stop()` can await it.
  function runTracked(): Promise<void> {
    const poll = pollOnce().finally(() => {
      if (activePoll === poll) activePoll = undefined;
    });
    activePoll = poll;
    return poll;
  }

  return {
    start() {
      if (started) return;
      started = true;
      void runTracked();
      timer = setInterval(() => void runTracked(), intervalMs);
      // Don't keep the process alive solely for the poll loop.
      timer.unref?.();
    },
    async stop(): Promise<void> {
      if (timer) clearInterval(timer);
      timer = undefined;
      started = false;
      // Wait for an in-flight poll (e.g. a spawn) to settle so a session
      // created right before shutdown is still seen by the graceful-stop path.
      await activePoll;
    },
    pollOnce,
  };
}
