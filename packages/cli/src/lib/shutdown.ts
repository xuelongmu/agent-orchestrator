/**
 * SIGINT/SIGTERM shutdown handler for the long-running `ao start` process.
 *
 * Installs `process.once` listeners that perform a full graceful shutdown:
 * stop lifecycle workers, kill all active sessions, record last-stop state
 * for restore on next `ao start`, unregister from running.json, await the
 * bun-tmp janitor's final sweep, then exit.
 *
 * Lives in its own module so the orchestration is testable in isolation
 * and so the equivalent kill-and-record logic in `ao stop` can converge
 * here in a later refactor (today the two paths duplicate the core loop;
 * see ao-118 plan PR B).
 */

import {
  isBlockedByDependency,
  isTerminalSession,
  loadConfig,
  markDaemonShutdownHandlerInstalled,
  recordActivityEvent,
  sweepDaemonChildren,
  type Session,
} from "@aoagents/ao-core";
import { loadMergedScopeConfig } from "./config-scope.js";
import { stopBunTmpJanitor } from "./bun-tmp-janitor.js";
import { getSessionManager } from "./create-session-manager.js";
import { stopAllLifecycleWorkers } from "./lifecycle-service.js";
import { stopProjectSupervisor } from "./project-supervisor.js";
import { stopBacklogPoller } from "./backlog-service.js";
import { unregister, writeLastStop } from "./running-state.js";

const SHUTDOWN_TIMEOUT_MS = 10_000;

/**
 * Upper bound on how long shutdown waits for an in-flight backlog poll to
 * settle before proceeding to kill sessions. Comfortably under
 * {@link SHUTDOWN_TIMEOUT_MS} so a poll stuck in a slow tracker call or
 * `sessionManager.spawn()` can never let the force-exit timer fire before the
 * kill loop and last-stop write run.
 */
const BACKLOG_DRAIN_TIMEOUT_MS = 3_000;

export interface ShutdownContext {
  /** Path to the orchestrator config; re-read at shutdown time so any
   *  config edits since startup are honored. */
  configPath: string;
  /** Project this `ao start` invocation owns; used to scope last-stop's
   *  primary `sessionIds` field (other projects go to `otherProjects`). */
  projectId: string;
}

// Module-level guards so a second call to installShutdownHandlers within
// the same process is a no-op (vs. registering duplicate listeners that
// would each race to writeLastStop / unregister / process.exit on signal).
let handlersInstalled = false;
let shuttingDown = false;

export function isShutdownInProgress(): boolean {
  return shuttingDown;
}

/**
 * Install SIGINT/SIGTERM handlers. Process-wide idempotent — calling
 * this more than once is a no-op. Only the first signal triggers
 * cleanup; subsequent signals are ignored until the 10-second
 * force-exit timer fires.
 */
export function installShutdownHandlers(ctx: ShutdownContext): void {
  if (handlersInstalled) return;
  handlersInstalled = true;
  markDaemonShutdownHandlerInstalled();

  const shutdown = (signal: NodeJS.Signals): void => {
    if (shuttingDown) return;
    shuttingDown = true;

    const exitCode = signal === "SIGINT" ? 130 : 0;

    recordActivityEvent({
      projectId: ctx.projectId,
      source: "cli",
      kind: "cli.shutdown_signal",
      level: "info",
      summary: `received ${signal}, beginning graceful shutdown`,
      data: { signal, exitCode },
    });

    let backlogStopped: Promise<void> = Promise.resolve();
    try {
      backlogStopped = stopBacklogPoller();
      stopProjectSupervisor();
      stopAllLifecycleWorkers();
    } catch {
      // Best-effort — never block shutdown on observability.
    }

    const forceExit = setTimeout(() => {
      recordActivityEvent({
        projectId: ctx.projectId,
        source: "cli",
        kind: "cli.shutdown_force_exit",
        level: "warn",
        summary: `force-exit after ${SHUTDOWN_TIMEOUT_MS}ms timeout`,
        data: { signal, timeoutMs: SHUTDOWN_TIMEOUT_MS, exitCode },
      });
      process.exit(exitCode);
    }, SHUTDOWN_TIMEOUT_MS);
    forceExit.unref();

    void (async () => {
      try {
        // Wait for an in-flight backlog poll to settle so a session it spawned
        // right before the signal is enumerated below and killed cleanly. A
        // rejection here must not abort the kill path, so swallow it. Bound the
        // wait: if the poll is stuck in a slow tracker call or spawn, proceed
        // anyway rather than let the 10s force-exit fire with sessions un-killed
        // and last-stop unwritten.
        let drainTimer: ReturnType<typeof setTimeout> | undefined;
        const drained = await Promise.race([
          backlogStopped.then(
            () => true,
            () => true,
          ),
          new Promise<boolean>((resolve) => {
            // NOT unref'd: this timeout gates the kill / last-stop cleanup below.
            // On a headless daemon with `backlogStopped` still pending, an
            // unref'd timer plus a pending promise leaves nothing keeping the
            // event loop alive, so Node could exit before cleanup runs. Cleared
            // once the race settles so a fast drain leaves no dangling handle.
            drainTimer = setTimeout(() => resolve(false), BACKLOG_DRAIN_TIMEOUT_MS);
          }),
        ]);
        if (drainTimer) clearTimeout(drainTimer);
        if (!drained) {
          recordActivityEvent({
            projectId: ctx.projectId,
            source: "cli",
            kind: "cli.shutdown_backlog_drain_timeout",
            level: "warn",
            summary: `backlog poll did not settle within ${BACKLOG_DRAIN_TIMEOUT_MS}ms; proceeding with shutdown`,
            data: { signal, timeoutMs: BACKLOG_DRAIN_TIMEOUT_MS },
          });
        }
        // Best-effort like the restore path (start.ts): if the merged scope can't
        // be built — a corrupt global registry, or a project/sessionPrefix/notifier
        // collision introduced while the daemon ran — fall back to the plain config
        // so we still enumerate and kill THIS daemon's sessions, write last-stop,
        // sweep children, and unregister. Throwing to the outer catch here would
        // record `cli.shutdown_failed` and exit with managed runtimes still running.
        let shutdownConfig;
        try {
          shutdownConfig = loadMergedScopeConfig(ctx.configPath);
        } catch (err) {
          recordActivityEvent({
            projectId: ctx.projectId,
            source: "cli",
            kind: "cli.shutdown_merged_scope_fallback",
            level: "warn",
            summary: `merged shutdown scope failed to load; falling back to base config`,
            data: { errorMessage: err instanceof Error ? err.message : String(err) },
          });
          shutdownConfig = loadConfig(ctx.configPath);
        }
        const sm = await getSessionManager(shutdownConfig);

        // Every session killed across one or two enumeration passes, keyed by id
        // so the second pass never double-kills or double-records a session.
        const killedSessions = new Map<string, Session>();
        const killActivePass = async (): Promise<void> => {
          // Held (blocked-by-dependency) sessions own no runtime — there is
          // nothing to stop. Leave their reservation on disk so the scheduler
          // resumes them on the next `ao start` instead of `kill()` terminating
          // them and losing the held marker across stop/restore (#10).
          const active = (await sm.list()).filter(
            (s) => !isTerminalSession(s) && !(s.lifecycle && isBlockedByDependency(s.lifecycle)),
          );
          for (const session of active) {
            if (killedSessions.has(session.id)) continue;
            try {
              const result = await sm.kill(session.id);
              if (result.cleaned || result.alreadyTerminated) {
                killedSessions.set(session.id, session);
              }
            } catch (err) {
              recordActivityEvent({
                projectId: session.projectId ?? ctx.projectId,
                sessionId: session.id,
                source: "cli",
                kind: "cli.shutdown_session_kill_failed",
                level: "warn",
                summary: `failed to kill session during shutdown`,
                data: { errorMessage: err instanceof Error ? err.message : String(err) },
              });
            }
          }
        };

        // First pass: kill everything enumerable right now.
        await killActivePass();

        // If the bounded drain above gave up, the backlog poll may still be inside
        // an in-flight `sessionManager.spawn()` — aborting can't cancel it, so a
        // worker could be created after this first enumeration and escape the kill
        // loop. Wait (bounded again) for the poll to settle: the poller tears down
        // a worker it spawned once it observes the abort (see backlog-poller
        // spawnFromBacklog), so by the time `backlogStopped` resolves the escapee
        // is gone. Re-enumerate as a safety net in case that teardown failed.
        if (!drained) {
          let secondDrainTimer: ReturnType<typeof setTimeout> | undefined;
          await Promise.race([
            backlogStopped.then(
              () => undefined,
              () => undefined,
            ),
            new Promise<void>((resolve) => {
              secondDrainTimer = setTimeout(resolve, BACKLOG_DRAIN_TIMEOUT_MS);
            }),
          ]);
          if (secondDrainTimer) clearTimeout(secondDrainTimer);
          await killActivePass();
        }

        const killedSessionIds = [...killedSessions.keys()];
        if (killedSessionIds.length > 0) {
          const killedList = [...killedSessions.values()];
          const targetIds = killedSessionIds.filter(
            (id) => killedSessions.get(id)?.projectId === ctx.projectId,
          );
          const otherProjects: Array<{ projectId: string; sessionIds: string[] }> = [];
          const otherByProject = new Map<string, string[]>();
          for (const s of killedList) {
            if (s.projectId === ctx.projectId) continue;
            const list = otherByProject.get(s.projectId ?? "unknown") ?? [];
            list.push(s.id);
            otherByProject.set(s.projectId ?? "unknown", list);
          }
          for (const [pid, ids] of otherByProject) {
            otherProjects.push({ projectId: pid, sessionIds: ids });
          }
          try {
            await writeLastStop({
              stoppedAt: new Date().toISOString(),
              projectId: ctx.projectId,
              sessionIds: targetIds,
              otherProjects: otherProjects.length > 0 ? otherProjects : undefined,
            });
          } catch (err) {
            recordActivityEvent({
              projectId: ctx.projectId,
              source: "cli",
              kind: "cli.last_stop_write_failed",
              level: "error",
              summary: `failed to write last-stop state during shutdown`,
              data: {
                targetSessionCount: targetIds.length,
                otherProjectCount: otherProjects.length,
                totalKilled: killedSessionIds.length,
                errorMessage: err instanceof Error ? err.message : String(err),
              },
            });
          }
        }

        await sweepDaemonChildren({ ownerPid: process.pid });
        await unregister();
        recordActivityEvent({
          projectId: ctx.projectId,
          source: "cli",
          kind: "cli.shutdown_completed",
          level: "info",
          summary: `clean shutdown completed`,
          data: { signal, killedSessionCount: killedSessionIds.length, exitCode },
        });
      } catch (err) {
        recordActivityEvent({
          projectId: ctx.projectId,
          source: "cli",
          kind: "cli.shutdown_failed",
          level: "error",
          summary: `shutdown body threw before cleanup completed`,
          data: { signal, errorMessage: err instanceof Error ? err.message : String(err) },
        });
      }
      try {
        // Await any in-flight sweep so shutdown does not exit while
        // unlink() calls are still mid-flight against the filesystem.
        await stopBunTmpJanitor();
      } catch {
        // Best-effort cleanup.
      }
      process.exit(exitCode);
    })();
  };

  process.once("SIGINT", (sig) => {
    shutdown(sig);
  });
  process.once("SIGTERM", (sig) => {
    shutdown(sig);
  });
}
