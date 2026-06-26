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
  renameSync,
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

/**
 * Labels indicating an issue has already entered or completed the human-
 * verification flow (`ao verify` / the web verify tab). A merged issue carrying
 * any of these must neither be re-labeled for verification nor respawned as a
 * backlog worker: `merged-unverified` is awaiting review, `verified` /
 * `verification-failed` are post-review terminal states, and `agent:done` marks
 * completed work. Shared by the labeling and spawn paths so both honor the same
 * source of truth.
 */
export const VERIFICATION_LABELS = [
  MERGED_UNVERIFIED_LABEL,
  "verified",
  "verification-failed",
  "agent:done",
];

/** Default interval between backlog poll cycles (1 minute). */
export const BACKLOG_POLL_INTERVAL_MS = 60_000;

/** Default per-project concurrency cap when `maxConcurrentAgents` is unset. */
export const DEFAULT_MAX_CONCURRENT_AGENTS = 5;

/**
 * Upper bound on how many backlog issues a single project poll will request.
 * The fetch grows its page when stale skip-only issues (claimed-but-unrelabeled
 * or verification-labeled) saturate it, so fresh work further down isn't
 * starved; this caps that growth so a large backlog can't trigger an unbounded
 * listing call.
 */
const MAX_BACKLOG_FETCH = 200;

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
  try {
    // Inside the try so a setup failure (e.g. a read-only config mount) is
    // treated as "could not acquire" rather than thrown out of pollOnce —
    // an unhandled rejection here would propagate into the shutdown path.
    mkdirSync(dirname(lockPath), { recursive: true });
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
    // Lock exists. Reclaim it if it is stale (holder likely crashed) — but do
    // it atomically. A plain `rmSync` + recursive acquire is a TOCTOU race:
    // two pollers can both pass the stale-mtime check, then one unlinks the
    // other's freshly-acquired lock and both run `spawnFromBacklog` (double
    // spawn). Instead, rename the stale file to a per-process name as the
    // handoff: `renameSync` is atomic, so only one racing process can move a
    // given file — the loser gets ENOENT and backs off until the next cycle.
    try {
      const info = statSync(lockPath);
      if (Date.now() - info.mtimeMs <= LOCK_STALE_MS) {
        return null; // Fresh lock — a live holder owns it.
      }
      const reclaimPath = `${lockPath}.reclaim.${process.pid}`;
      try {
        renameSync(lockPath, reclaimPath);
      } catch {
        // Lost the rename race (gone or already reclaimed) — retry next cycle.
        return null;
      }
      // The file we grabbed could be one a peer recreated fresh between our
      // stat and our rename. Re-check its mtime: if it's no longer stale we
      // stole a live lock, so restore it (best effort) and back off.
      try {
        const reclaimed = statSync(reclaimPath);
        if (Date.now() - reclaimed.mtimeMs <= LOCK_STALE_MS) {
          try {
            renameSync(reclaimPath, lockPath);
          } catch {
            rmSync(reclaimPath, { force: true });
          }
          return null;
        }
      } catch {
        // Renamed file vanished — nothing to restore; fall through to acquire.
      }
      rmSync(reclaimPath, { force: true });
      return tryAcquireLock(lockPath);
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

    // Consult the tracker's authoritative state before (re-)labeling. This
    // guards two cases: a peer poller process may have already labeled the
    // issue (our in-memory `processedIssues` set is per-process), and after a
    // daemon restart `processedIssues` is empty while the merged session is
    // still on disk.
    let currentIssue: Issue | undefined;
    try {
      currentIssue = await tracker.getIssue(issueId, project);
    } catch {
      // getIssue failed — fall through and attempt the update anyway.
    }

    // Skip only when the issue already carries an explicit verification label
    // (e.g. a verified / verification-failed issue whose `merged-unverified` was
    // removed by `ao verify`) — re-labeling would drag it back into the verify
    // queue and post a duplicate comment. A *closed* tracker state alone is NOT
    // AO verification: a tracker may auto-close the issue from a PR closing
    // keyword on merge. Treating that as verified would silently drop the issue
    // from the human-verification surfaces, so we still label (and reopen) it.
    const labels = currentIssue?.labels ?? [];
    if (currentIssue && VERIFICATION_LABELS.some((label) => labels.includes(label))) {
      processedIssues.add(key);
      continue;
    }

    try {
      await tracker.updateIssue(
        issueId,
        {
          // Reopen if the tracker auto-closed the issue on merge, so it lands in
          // the (state:open-filtered) verification queue for human staging
          // validation. A no-op when the issue is already open.
          ...(currentIssue?.state === "closed" ? { state: "open" as const } : {}),
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
            // Clear every verification label, not just `agent:done`. A web-
            // verified issue is closed with both `verified` and `agent:done`; if
            // it's later reopened, leaving `verified` behind would make
            // `spawnFromBacklog` skip it via VERIFICATION_LABELS, stranding it in
            // the backlog forever. Removing them all returns it cleanly.
            removeLabels: [...VERIFICATION_LABELS],
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
  signal?: AbortSignal,
): Promise<void> {
  if (signal?.aborted) return;

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

  // Tracker-wide set of issue URLs that are already taken — by an active
  // worker in ANY project, or claimed earlier in this same poll cycle. Keyed
  // by URL (globally unique) rather than the bare issue id, since ids like
  // GitHub "#42" collide across repos. Duplicate detection therefore spans
  // projects that point at the same tracker/repo (concurrency caps stay per
  // project), and a worker for issue 42 in project A is no longer invisible
  // while polling project B even if A's claim label transition failed.
  const takenIssueUrls = new Set<string>();
  for (const session of workerSessions) {
    if (!session.issueId) continue;
    const sessionProject = config.projects[session.projectId];
    const plugin = sessionProject?.tracker?.plugin;
    if (!sessionProject || !plugin) continue;
    const sessionTracker = registry.get<Tracker>("tracker", plugin);
    if (!sessionTracker?.issueUrl) continue;
    try {
      takenIssueUrls.add(sessionTracker.issueUrl(session.issueId, sessionProject));
    } catch {
      // URL construction failed — the per-project id check below still applies.
    }
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

    // Canonicalize via the tracker's own `issueUrl()` rather than trusting the
    // listed `issue.url`. `takenIssueUrls` is built from `issueUrl()`, but some
    // trackers' `listIssues()` returns a URL that isn't byte-identical to it —
    // e.g. Linear records sessions with the short `issueUrl()` but lists
    // `node.url`, which can carry a title slug. Deriving both keys from the same
    // function avoids missing an already-running worker and double-spawning.
    const canonicalUrlFor = (issue: Issue): string => {
      if (tracker.issueUrl) {
        try {
          return tracker.issueUrl(issue.id, project);
        } catch {
          // URL construction failed — fall back to the listed URL; the
          // per-project id check still guards same-project duplicates.
        }
      }
      return issue.url;
    };
    // An issue is skipped when it's already being worked on (per-project id),
    // already taken by any project / claimed earlier this cycle (tracker-wide by
    // URL), or still carries a verification label. The last is a merged issue
    // awaiting human verification that kept `agent:backlog` — on trackers whose
    // updateIssue ignores removeLabels (Linear, GitLab) or after a failed claim.
    // Its session is merged (excluded from workerSessions), so nothing else
    // guards it. Shared by the fetch sizing and the spawn loop so both agree.
    const wouldSkipForSpawn = (issue: Issue): boolean =>
      VERIFICATION_LABELS.some((label) => issue.labels.includes(label)) ||
      activeIssueIds.has(issue.id.toLowerCase()) ||
      takenIssueUrls.has(canonicalUrlFor(issue));

    // Fetch the backlog, growing the page while it stays saturated with issues
    // we'll skip and capacity remains. Without this, stale issues still carrying
    // `agent:backlog` (claimed-but-unrelabeled, or verification-labeled with no
    // live session to clean them) can fill the whole page every poll and
    // indefinitely starve fresh work despite open slots. Over-fetching is
    // harmless — the spawn loop stops at `availableSlots`. Bounded so a large
    // backlog can't trigger an unbounded fetch.
    let limit = availableSlots + takenIssueUrls.size;
    let backlogIssues: Issue[];
    try {
      backlogIssues = await tracker.listIssues(
        { state: "open", labels: [BACKLOG_LABEL], limit },
        project,
      );
    } catch {
      continue; // Tracker unavailable — skip this project
    }
    while (
      backlogIssues.length >= limit &&
      limit < MAX_BACKLOG_FETCH &&
      backlogIssues.filter((issue) => !wouldSkipForSpawn(issue)).length < availableSlots
    ) {
      limit = Math.min(limit * 2, MAX_BACKLOG_FETCH);
      try {
        backlogIssues = await tracker.listIssues(
          { state: "open", labels: [BACKLOG_LABEL], limit },
          project,
        );
      } catch {
        break; // Keep the last good page rather than dropping the project.
      }
    }

    for (const issue of backlogIssues) {
      if (availableSlots <= 0) break;

      // Shutdown requested mid-cycle (stop() aborted us) — bail before creating
      // any further worker, so a spawn can't slip in after the graceful-stop
      // path has already enumerated sessions to kill / written last-stop.
      if (signal?.aborted) return;

      if (wouldSkipForSpawn(issue)) continue;
      const canonicalUrl = canonicalUrlFor(issue);

      try {
        await sessionManager.spawn({ projectId, issueId: issue.id });
        availableSlots--;
        activeIssueIds.add(issue.id.toLowerCase());
        takenIssueUrls.add(canonicalUrl);

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
  // Aborts the in-flight poll's pending spawns when `stop()` is called. Merely
  // awaiting the poll isn't enough: a poll stuck in a slow tracker call or
  // `sessionManager.spawn()` can resume after the bounded shutdown drain and
  // spawn a worker the graceful-stop path has already passed. Aborting makes
  // the poll skip any remaining spawns.
  let activeAbort: AbortController | undefined;

  async function pollOnce(): Promise<void> {
    const lock = lockPath ? tryAcquireLock(lockPath) : null;
    if (lockPath && !lock) {
      // Another process is mid-cycle — skip to avoid double-spawning.
      return;
    }

    const abort = new AbortController();
    activeAbort = abort;
    try {
      const { config, registry, sessionManager } = await resolveServices();

      const allSessions = await sessionManager.list();
      await labelIssuesForVerification(allSessions, config, registry, processedIssues, logger);
      await relabelReopenedIssues(config, registry, logger);
      await spawnFromBacklog(allSessions, config, registry, sessionManager, logger, abort.signal);
    } catch (err) {
      logger?.error?.("[backlog] Poll failed:", err);
    } finally {
      lock?.release();
      if (activeAbort === abort) activeAbort = undefined;
    }
  }

  // Run a poll while recording it as the active one, so `stop()` can await it.
  function runTracked(): Promise<void> {
    // A poll is already in flight — skip this tick instead of overwriting
    // `activePoll`. Otherwise a poll slower than the interval would be replaced
    // by the next (lock-skipped, fast-resolving) tick, and stop() could resolve
    // without awaiting the original spawn the shutdown path depends on.
    if (activePoll) return activePoll;
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
      // Cancel the in-flight poll's pending spawns first, then wait for it to
      // settle. Aborting (not just awaiting) ensures a poll that resumes from a
      // slow tracker call / spawn after the bounded shutdown drain can't create
      // a worker the graceful-stop path has already enumerated and passed.
      activeAbort?.abort();
      await activePoll;
    },
    pollOnce,
  };
}
