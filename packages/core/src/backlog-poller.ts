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
import { isBlockedByDependency } from "./lifecycle-state.js";
import { updateMetadata } from "./metadata.js";
import { getProjectSessionsDir } from "./paths.js";
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

/**
 * Session-metadata marker recording that a merged session has already been run
 * through verification labeling. Persisted so the dedupe survives a daemon
 * restart (which empties the in-memory `processedIssues` set): without it, after
 * a verified issue is reopened to `agent:backlog`, the stale on-disk merged
 * session would be re-labeled `merged-unverified` on the next poll, stripping
 * `agent:backlog` and stranding the reopened issue.
 */
export const VERIFICATION_PROCESSED_MARKER = "verificationProcessed";

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
 * Wrap an open lock-file descriptor (holding `lockPath`) in a heartbeat + release
 * handle. Shared by the initial acquire and the stale-lock reclaim paths.
 */
function makeLockHandle(fd: number, lockPath: string): LockHandle {
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
    return makeLockHandle(openSync(lockPath, "wx"), lockPath);
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
      // Immediately re-create a lock at `lockPath` so it stays VISIBLE while we
      // verify the reclaim. Otherwise `lockPath` would be absent during the
      // mtime recheck/restore below and a third poller could acquire it and run
      // concurrently — defeating the dedupe the lock provides.
      let fd: number;
      try {
        fd = openSync(lockPath, "wx");
      } catch {
        // Another poller already recreated the lock in the tiny gap after our
        // rename — it owns the cycle now; drop the file we moved and back off.
        rmSync(reclaimPath, { force: true });
        return null;
      }
      // The file we grabbed could be one a peer recreated fresh between our stat
      // and our rename. Re-check its mtime: if it's no longer stale we stole a
      // live lock. Restore the peer's lock to `lockPath` so its heartbeat keeps
      // it fresh (leaving OUR placeholder there instead would go stale after
      // LOCK_STALE_MS with no heartbeat and let a third poller reclaim it while
      // the peer still runs). Close our placeholder fd first, then atomically
      // replace it with the peer's still-live lock file.
      try {
        const reclaimed = statSync(reclaimPath);
        if (Date.now() - reclaimed.mtimeMs <= LOCK_STALE_MS) {
          try {
            closeSync(fd);
          } catch {
            // Ignore — we still restore the peer's lock over the placeholder.
          }
          try {
            // Atomic replace on POSIX and Windows (MoveFileEx). `lockPath` never
            // goes absent, so no third poller can slip in during the handoff.
            renameSync(reclaimPath, lockPath);
          } catch {
            // Rare: replace-over-existing unsupported here. Remove the placeholder
            // then restore (a single tight syscall gap in this already-rare race).
            try {
              rmSync(lockPath, { force: true });
            } catch {
              // Best-effort.
            }
            try {
              renameSync(reclaimPath, lockPath);
            } catch {
              rmSync(reclaimPath, { force: true });
            }
          }
          return null;
        }
      } catch {
        // Renamed file vanished — nothing to restore; hold our placeholder.
      }
      // Legit reclaim of a stale lock: hold `lockPath` via our placeholder fd.
      rmSync(reclaimPath, { force: true });
      return makeLockHandle(fd, lockPath);
    } catch {
      // The lock vanished between open and stat — let the next cycle retry.
    }
    return null;
  }
}

function getMaxConcurrentAgents(project: ProjectConfig): number {
  // Honor the existing `maxConcurrent` cap (enforced by the dependency scheduler
  // for launched workers) as the fallback before the default, so a project with
  // `maxConcurrent: 1` doesn't silently let the backlog poller spawn the default
  // five. `maxConcurrentAgents` (backlog-specific) still wins when both are set.
  const configured = project.maxConcurrentAgents ?? project.maxConcurrent;
  return typeof configured === "number" && configured > 0
    ? configured
    : DEFAULT_MAX_CONCURRENT_AGENTS;
}

/**
 * Label GitHub issues for verification when their PRs have been merged.
 *
 * Dedupe is keyed per *session* (`${projectId}:${session.id}`), not per issue:
 * a verified issue can be reopened and reworked by a NEW session for the same
 * issue id, and that new session must be re-labeled when it merges while the old
 * merged session stays deduped (keying by issue id would either re-label via the
 * stale session or block the rework). `processedIssues` is mutated to avoid
 * repeated API calls within a process; a per-session
 * {@link VERIFICATION_PROCESSED_MARKER} is also persisted to the session's
 * metadata so the dedupe survives a daemon restart (the in-memory set is empty
 * after restart while the merged session is still on disk).
 *
 * Returns the set of keys whose merged session could NOT be transitioned this
 * cycle (label update failed while the issue may still carry `agent:backlog`).
 * Each such issue is recorded under BOTH its per-project `${projectId}:${issueId}`
 * key AND its tracker-wide canonical issue URL (when available), so a sibling
 * project pointing at the same tracker/repo also skips it. The caller passes
 * these to `spawnFromBacklog` so it won't spawn a fresh worker for already-merged
 * work before the next poll retries the label.
 */
async function labelIssuesForVerification(
  sessions: Session[],
  config: OrchestratorConfig,
  registry: PluginRegistry,
  processedIssues: Set<string>,
  logger?: BacklogLogger,
  signal?: AbortSignal,
): Promise<Set<string>> {
  const unlabeledMergedIssues = new Set<string>();
  if (signal?.aborted) return unlabeledMergedIssues;
  const mergedSessions = sessions.filter(
    (s) =>
      s.lifecycle.pr.state === "merged" &&
      s.issueId &&
      !processedIssues.has(`${s.projectId}:${s.id}`) &&
      // Persisted cross-restart guard: a session already run through verification
      // labeling carries this marker, so a reopened issue isn't re-labeled by its
      // stale merged session after the in-memory set is cleared by a restart.
      s.metadata?.[VERIFICATION_PROCESSED_MARKER] !== "1",
  );

  for (const session of mergedSessions) {
    // Shutdown requested mid-cycle (stop() aborted us) — stop mutating tracker
    // state so a graceful stop leaves issues untouched.
    if (signal?.aborted) break;
    const key = `${session.projectId}:${session.id}`;
    // Mark this session done with verification, in-memory AND persisted to disk
    // so the dedupe survives a daemon restart.
    const markProcessed = (): void => {
      processedIssues.add(key);
      try {
        updateMetadata(getProjectSessionsDir(session.projectId), session.id, {
          [VERIFICATION_PROCESSED_MARKER]: "1",
        });
      } catch (err) {
        // Best-effort persistence — the in-memory set still dedupes this process.
        logger?.error?.(
          `[backlog] Failed to persist verification marker for session ${session.id}:`,
          err,
        );
      }
    };
    const issueId = session.issueId;
    if (!issueId) {
      markProcessed();
      continue;
    }
    const issueKey = `${session.projectId}:${issueId.toLowerCase()}`;
    const project = config.projects[session.projectId];
    if (!project?.tracker?.plugin) {
      markProcessed();
      continue;
    }

    const tracker = registry.get<Tracker>("tracker", project.tracker.plugin);
    if (!tracker?.updateIssue) {
      markProcessed();
      continue;
    }

    // Tracker-wide canonical URL for this issue (matches the key `spawnFromBacklog`
    // builds for active workers), so a merged-but-unlabeled issue is skipped by
    // EVERY project sharing this tracker/repo, not just the one that owns it.
    let canonicalIssueUrl: string | undefined;
    if (tracker.issueUrl) {
      try {
        canonicalIssueUrl = tracker.issueUrl(issueId, project);
      } catch {
        // URL construction failed — the per-project id key still guards this project.
      }
    }
    const markUnlabeled = (): void => {
      unlabeledMergedIssues.add(issueKey);
      if (canonicalIssueUrl) unlabeledMergedIssues.add(canonicalIssueUrl);
    };

    // Consult the tracker's authoritative state before (re-)labeling. This
    // guards two cases: a peer poller process may have already labeled the
    // issue (our in-memory `processedIssues` set is per-process), and after a
    // daemon restart `processedIssues` is empty while the merged session is
    // still on disk.
    let currentIssue: Issue;
    try {
      currentIssue = await tracker.getIssue(issueId, project);
    } catch (err) {
      // Can't determine authoritative state — the update's correctness depends
      // on it (whether to re-open an auto-closed issue, whether it's already
      // verified). Skip and retry next poll rather than updating blindly. Don't
      // mark processed, and keep the issue out of this cycle's spawn pass.
      logger?.error?.(`[backlog] getIssue failed for ${session.issueId}; will retry:`, err);
      markUnlabeled();
      continue;
    }

    // Skip only when the issue already carries an explicit verification label
    // (e.g. a verified / verification-failed issue whose `merged-unverified` was
    // removed by `ao verify`) — re-labeling would drag it back into the verify
    // queue and post a duplicate comment. A *closed* tracker state alone is NOT
    // AO verification: a tracker may auto-close the issue from a PR closing
    // keyword on merge. Treating that as verified would silently drop the issue
    // from the human-verification surfaces, so we still label (and reopen) it.
    const labels = currentIssue.labels ?? [];
    if (VERIFICATION_LABELS.some((label) => labels.includes(label))) {
      markProcessed();
      continue;
    }

    try {
      await tracker.updateIssue(
        issueId,
        {
          // Reopen if the tracker auto-closed the issue on merge, so it lands in
          // the (state:open-filtered) verification queue for human staging
          // validation. A no-op when the issue is already open.
          ...(currentIssue.state === "closed" ? { state: "open" as const } : {}),
          labels: [MERGED_UNVERIFIED_LABEL],
          removeLabels: ["agent:backlog", "agent:in-progress"],
          comment: `PR merged. Issue awaiting human verification on staging.`,
        },
        project,
      );
      // Only mark processed after a confirmed update. A transient tracker /
      // network failure must NOT consume the dedupe slot — otherwise every
      // later poll in this daemon skips the merged session and the issue never
      // reaches the verify queue until the process restarts. Leaving it unset
      // lets the next cycle retry the label.
      markProcessed();
    } catch (err) {
      logger?.error?.(`[backlog] Failed to label issue ${session.issueId} for verification:`, err);
      // The label transition failed while the merged issue may still carry
      // `agent:backlog`. Keep it out of this cycle's spawn pass so we don't
      // launch a fresh worker for work whose PR already merged; the next poll
      // retries the label (the dedupe slot was intentionally left unconsumed).
      markUnlabeled();
    }
  }

  return unlabeledMergedIssues;
}

/**
 * Labels marking an issue as *completed* — a reopened (state:open) issue
 * carrying any of these was finished and is being returned for rework. Web
 * verification closes with `agent:done` (+ `verified`); the `ao verify` CLI
 * closes with only `verified`, so both must be scanned or a CLI-verified
 * reopened issue is never returned to the backlog.
 */
const REOPENED_COMPLETION_LABELS = ["agent:done", "verified"];

/**
 * Detect reopened issues (open + a completion label) and swap the label back to
 * agent:backlog so the poller picks them up on the next cycle.
 */
async function relabelReopenedIssues(
  config: OrchestratorConfig,
  registry: PluginRegistry,
  logger?: BacklogLogger,
  signal?: AbortSignal,
): Promise<void> {
  if (signal?.aborted) return;
  for (const [, project] of Object.entries(config.projects)) {
    if (signal?.aborted) return;
    if (!project.tracker?.plugin) continue;
    const tracker = registry.get<Tracker>("tracker", project.tracker.plugin);
    if (!tracker?.listIssues || !tracker.updateIssue) continue;

    // Scan per completion label and union by id (the `labels` filter is AND, so a
    // single call for both would miss issues carrying only one of them).
    const reopenedById = new Map<string, Issue>();
    for (const label of REOPENED_COMPLETION_LABELS) {
      try {
        const issues = await tracker.listIssues(
          { state: "open", labels: [label], limit: 20 },
          project,
        );
        for (const issue of issues) reopenedById.set(issue.id, issue);
      } catch {
        // Tracker error for this label — try the others; retry the rest next cycle.
      }
    }
    // A `verification-failed` issue the user explicitly re-backlogged (BOTH labels
    // present) means "send it back to an agent". `wouldSkipForSpawn` skips any
    // verification-labeled issue, so it never spawns until `verification-failed` is
    // cleared — do that here (the relabel below drops all VERIFICATION_LABELS).
    try {
      const requeued = await tracker.listIssues(
        { state: "open", labels: ["verification-failed", BACKLOG_LABEL], limit: 20 },
        project,
      );
      for (const issue of requeued) reopenedById.set(issue.id, issue);
    } catch {
      // Tracker error — retry next cycle.
    }
    const reopened = [...reopenedById.values()];

    for (const issue of reopened) {
      // Shutdown requested mid-cycle (stop() aborted us) — perform no further
      // tracker mutations so a graceful stop leaves issue state untouched.
      if (signal?.aborted) return;
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
        // No dedupe bookkeeping needed here: `processedIssues` is keyed per
        // session, so the reopened issue's rework spawns a NEW session whose key
        // is unseen and gets labeled on merge, while the old merged session stays
        // deduped and can't re-label the freshly-reopened backlog work.
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
  unlabeledMergedIssues: Set<string>,
  logger?: BacklogLogger,
  signal?: AbortSignal,
): Promise<void> {
  if (signal?.aborted) return;

  const allSessionPrefixes = Object.entries(config.projects).map(
    ([id, p]) => p.sessionPrefix ?? id,
  );
  const isWorkerSession = (session: Session): boolean =>
    !isOrchestratorSession(
      session,
      config.projects[session.projectId]?.sessionPrefix ?? session.projectId,
      allSessionPrefixes,
    );
  // Non-terminal workers actively claiming an issue — used for duplicate
  // detection (a reopened issue whose old session merged must still be
  // respawnable, so `merged` is intentionally excluded here).
  const workerSessions = allSessions.filter(
    (session) => isWorkerSession(session) && !TERMINAL_STATUSES.has(session.status),
  );

  // Sessions occupying a concurrency slot for the per-project cap: non-terminal
  // workers, PLUS `merged` workers whose runtime is still ALIVE (the post-merge
  // cleanup grace window — counting them avoids exceeding maxConcurrent:1 the
  // moment a PR merges but before the old agent exits). A merged session whose
  // runtime has exited/gone — or that lingers indefinitely because merge cleanup
  // is disabled — must NOT keep consuming a slot, or a reopened issue relabeled
  // to agent:backlog could never spawn. Held (blocked-by-dependency) sessions own
  // no runtime and never count.
  const slotCountByProject = new Map<string, number>();
  for (const session of allSessions) {
    if (!isWorkerSession(session)) continue;
    const occupiesSlot =
      !TERMINAL_STATUSES.has(session.status) ||
      (session.status === "merged" && session.lifecycle.runtime?.state === "alive");
    if (!occupiesSlot || isBlockedByDependency(session.lifecycle)) continue;
    slotCountByProject.set(session.projectId, (slotCountByProject.get(session.projectId) ?? 0) + 1);
  }

  // Group active workers per project so duplicate detection is scoped per
  // project (per-project concurrency).
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
    // Its source (startup) config is unreadable — only a cached copy is in
    // scope. Don't spawn new workers (they'd get a stale AO_CONFIG_PATH); the
    // project's existing sessions are still supervised/killed elsewhere.
    if (project._spawnPaused) continue;

    const projectWorkers = workersByProject.get(projectId) ?? [];
    let availableSlots =
      getMaxConcurrentAgents(project) - (slotCountByProject.get(projectId) ?? 0);
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
    // Also skip an issue whose merged session's verification-label transition
    // failed (or whose tracker state couldn't be read) this cycle: its PR has
    // already merged, so spawning a fresh worker would redo merged work. The
    // label is retried next poll; until then it must stay out of the spawn pass.
    const wouldSkipForSpawn = (issue: Issue): boolean =>
      VERIFICATION_LABELS.some((label) => issue.labels.includes(label)) ||
      activeIssueIds.has(issue.id.toLowerCase()) ||
      takenIssueUrls.has(canonicalUrlFor(issue)) ||
      unlabeledMergedIssues.has(canonicalUrlFor(issue)) ||
      unlabeledMergedIssues.has(`${projectId}:${issue.id.toLowerCase()}`);

    // Fetch the backlog, growing the page while it stays saturated with issues
    // we'll skip and capacity remains. Without this, stale issues still carrying
    // `agent:backlog` (claimed-but-unrelabeled, or verification-labeled with no
    // live session to clean them) can fill the whole page every poll and
    // indefinitely starve fresh work despite open slots. Over-fetching is
    // harmless — the spawn loop stops at `availableSlots`. Bounded so a large
    // backlog can't trigger an unbounded fetch.
    //
    // Clamp the INITIAL page to MAX_BACKLOG_FETCH too: `takenIssueUrls.size` is
    // tracker-wide, so on a multi-project install with many active workers it can
    // already be in the hundreds before the growth loop's cap applies. A project
    // with one free slot would otherwise ask the tracker for an oversized page
    // every poll, which large portfolios can reject/time out (the catch would
    // then skip the project despite open capacity).
    let limit = Math.min(availableSlots + takenIssueUrls.size, MAX_BACKLOG_FETCH);
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
        const spawned = await sessionManager.spawn({ projectId, issueId: issue.id });

        // Shutdown may have aborted us *while* this spawn was in flight. Aborting
        // can't cancel a spawn already running, so the graceful-stop path could
        // have finished enumerating/killing sessions and moved on before this
        // worker existed — leaving it running after `ao start` exits. Tear it down
        // here (the poll that created it owns cleanup) and stop spawning.
        if (signal?.aborted) {
          try {
            await sessionManager.kill(spawned.id);
          } catch (err) {
            logger?.error?.(
              `[backlog] Failed to tear down spawn after shutdown abort for issue ${issue.id}:`,
              err,
            );
          }
          return;
        }

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
      const unlabeledMergedIssues = await labelIssuesForVerification(
        allSessions,
        config,
        registry,
        processedIssues,
        logger,
        abort.signal,
      );
      await relabelReopenedIssues(config, registry, logger, abort.signal);
      await spawnFromBacklog(
        allSessions,
        config,
        registry,
        sessionManager,
        unlabeledMergedIssues,
        logger,
        abort.signal,
      );
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
