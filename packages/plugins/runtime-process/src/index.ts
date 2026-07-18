import { spawn, type ChildProcess } from "node:child_process";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";
import {
  getShell,
  isWindows,
  killProcessTree,
  registerWindowsPtyHost,
  unregisterWindowsPtyHost,
  type PluginModule,
  type Runtime,
  type RuntimeCreateConfig,
  type RuntimeHandle,
  type RuntimeMetrics,
  type AttachInfo,
} from "@aoagents/ao-core";
import {
  getPipePath,
  ptyHostSendMessage,
  ptyHostSendRaw,
  ptyHostGetOutput,
  ptyHostIsAlive,
  ptyHostKill,
} from "./pty-client.js";

/** Escape key — cancels the agent's in-flight generation without exiting it. */
const INTERRUPT_KEY = "\x1b";

export const manifest = {
  name: "process",
  slot: "runtime" as const,
  description: "Runtime plugin: child processes",
  version: "0.1.0",
};

/** Only allow safe characters in session IDs */
const SAFE_SESSION_ID = /^[a-zA-Z0-9_-]+$/;

function assertValidSessionId(id: string): void {
  if (!SAFE_SESSION_ID.test(id)) {
    throw new Error(`Invalid session ID "${id}": must match ${SAFE_SESSION_ID}`);
  }
}

interface ProcessEntry {
  process: ChildProcess | null;
  outputBuffer: string[];
  createdAt: number;
}

const MAX_OUTPUT_LINES = 1000;

export function create(): Runtime {
  // Per-instance process map — each create() call gets its own isolated state
  const processes = new Map<string, ProcessEntry>();

  const writeManagedStdin = async (handle: RuntimeHandle, data: string): Promise<void> => {
    const entry = processes.get(handle.id);
    if (!entry) {
      throw new Error(`No process found for session ${handle.id}`);
    }

    const child = entry.process;
    if (!child) {
      throw new Error(`Process for session ${handle.id} is still spawning`);
    }
    const stdin = child.stdin;
    if (!stdin || !stdin.writable) {
      throw new Error(`stdin not writable for session ${handle.id}`);
    }

    await new Promise<void>((resolve, reject) => {
      let done = false;
      const finish = (err?: Error | null) => {
        if (done) return;
        done = true;
        cleanup();
        if (err) reject(err);
        else resolve();
      };
      const onError = (err: Error) => finish(err);
      const onDrain = () => {
        // Drain means backpressure cleared — still wait for write callback
      };
      const cleanup = () => {
        stdin.removeListener("error", onError);
        stdin.removeListener("drain", onDrain);
      };
      stdin.on("error", onError);
      stdin.on("drain", onDrain);
      stdin.write(data, (err) => finish(err ?? null));
    });
  };

  return {
    name: "process",

    async create(config: RuntimeCreateConfig): Promise<RuntimeHandle> {
      assertValidSessionId(config.sessionId);

      const handleId = config.sessionId;

      // Prevent duplicate session IDs — check and reserve atomically (no await
      // between check and set) so concurrent create() calls can't both pass.
      if (processes.has(handleId)) {
        throw new Error(`Session "${handleId}" already exists — destroy it before re-creating`);
      }

      // Reserve the slot synchronously for both platforms so duplicate-create,
      // getMetrics, and getAttachInfo see the entry on Windows too. Unix used
      // to create this further down; we hoist it so the Windows branch shares
      // the same bookkeeping.
      const entry: ProcessEntry = {
        process: null,
        outputBuffer: [],
        createdAt: Date.now(),
      };
      processes.set(handleId, entry);

      // --- Windows: spawn via PTY host (ConPTY) ---
      if (isWindows()) {
        const pipePath = getPipePath(handleId);
        const shellInfo = getShell();
        const ptyHostScript = resolve(
          dirname(fileURLToPath(import.meta.url)),
          "pty-host.js",
        );

        const ptyEnv = { ...process.env, ...config.environment };
        try {
          const ptyChild = spawn(
            process.execPath,
            [
              ptyHostScript,
              handleId,
              pipePath,
              config.workspacePath,
              shellInfo.cmd,
              ...shellInfo.args(config.launchCommand),
            ],
            {
              cwd: config.workspacePath,
              env: ptyEnv,
              stdio: ["ignore", "pipe", "pipe"],
              detached: true, // Must survive parent exit (like tmux daemon)
              // Suppress the console window node-pty's conpty helper would
              // otherwise flash on screen when a ConPTY child fails. Without
              // this, errors from node-pty's internal conpty_console_list_agent
              // surface as visible Windows error dialogs (0x800700e8).
              windowsHide: true,
            },
          );

          // Wait for PTY host to signal readiness (writes "READY:<pid>\n" to stdout)
          const ptyPid = await new Promise<number>((resolveReady, reject) => {
            const timeout = setTimeout(() => {
              ptyChild.kill();
              reject(new Error("PTY host startup timeout (10s)"));
            }, 10_000);
            let data = "";
            ptyChild.stdout?.on("data", (chunk: Buffer) => {
              data += chunk.toString();
              const match = data.match(/READY:(\d+)/);
              if (match) {
                clearTimeout(timeout);
                resolveReady(parseInt(match[1], 10));
              }
            });
            ptyChild.stderr?.on("data", (chunk: Buffer) => {
              data += chunk.toString();
            });
            ptyChild.on("error", (err) => {
              clearTimeout(timeout);
              reject(new Error(`PTY host spawn error: ${err.message}`));
            });
            ptyChild.on("exit", (code) => {
              clearTimeout(timeout);
              reject(new Error(`PTY host exited during startup with code ${code}: ${data}`));
            });
          });

          // Unref so this process can exit while pty-host stays alive
          ptyChild.unref();
          ptyChild.stdout?.destroy();
          ptyChild.stderr?.destroy();

          // Sideband registration so `ao stop` can find and graceful-kill this
          // pty-host even if its session JSON is later wiped. Without this,
          // `detached: true` puts pty-host in its own console group, escaping
          // `taskkill /T` on the parent process. See windows-pty-registry.ts.
          if (typeof ptyChild.pid === "number") {
            try {
              registerWindowsPtyHost({
                sessionId: handleId,
                ptyHostPid: ptyChild.pid,
                pipePath,
              });
            } catch {
              /* registry is best-effort; spawn must succeed regardless */
            }
          }

          return {
            id: handleId,
            runtimeName: "process",
            data: {
              pid: ptyPid,
              ptyHostPid: ptyChild.pid,
              pipePath,
              createdAt: entry.createdAt,
            },
          };
        } catch (err) {
          processes.delete(handleId);
          throw err;
        }
      }

      // --- Unix: existing piped stdio path (unchanged below) ---
      // Use explicit shell args instead of spawn's shell: option.
      // When shell is a string, Node.js internally passes -c which is ambiguous
      // on PowerShell 5.1 (-c matches both -Command and -ConfigurationName).
      // getShell().args() returns the correct flag (-Command for pwsh/powershell.exe, /c for cmd).
      // launchCommand comes from trusted YAML config and may contain pipes and redirects.
      const shellInfo = getShell();
      let child: ChildProcess;
      try {
        child = spawn(shellInfo.cmd, shellInfo.args(config.launchCommand), {
          cwd: config.workspacePath,
          env: { ...process.env, ...config.environment },
          stdio: ["pipe", "pipe", "pipe"],
          detached: !isWindows(), // Own process group so destroy() can kill child commands (Unix only)
        });
      } catch (err: unknown) {
        processes.delete(handleId);
        const msg = err instanceof Error ? err.message : String(err);
        throw new Error(`Failed to spawn process for session ${handleId}: ${msg}`, { cause: err });
      }

      entry.process = child;

      // Attach exit handler immediately — before any await — so fast-exiting
      // processes can't slip through the gap.
      child.once("exit", () => {
        entry.outputBuffer.push(`[process exited with code ${child.exitCode}]`);
        processes.delete(handleId);
      });

      // Handle late errors (process crashes after spawn)
      child.on("error", () => {
        // Already captured via exit handler — prevent unhandled error crash
      });

      // Wait for spawn success or error
      await new Promise<void>((resolve, reject) => {
        const onError = (err: Error) => {
          child.removeListener("spawn", onSpawn);
          processes.delete(handleId);
          reject(new Error(`Failed to spawn process for session ${handleId}: ${err.message}`));
        };
        const onSpawn = () => {
          child.removeListener("error", onError);
          resolve();
        };
        child.once("error", onError);
        child.once("spawn", onSpawn);
      });

      // Capture stdout and stderr into rolling buffer.
      // Each stream gets its own partial-line buffer so interleaved chunks
      // from different streams don't corrupt each other.
      function makeAppendOutput(): (data: Buffer) => void {
        let partial = "";
        return (data: Buffer) => {
          const text = partial + data.toString("utf-8");
          const lines = text.split("\n");
          // Last element is either "" (if text ended with \n) or a partial line
          partial = lines.pop()!;
          for (const line of lines) {
            entry.outputBuffer.push(line);
          }
          // Trim buffer to max size
          if (entry.outputBuffer.length > MAX_OUTPUT_LINES) {
            entry.outputBuffer.splice(0, entry.outputBuffer.length - MAX_OUTPUT_LINES);
          }
        };
      }

      const appendStdout = makeAppendOutput();
      const appendStderr = makeAppendOutput();
      child.stdout?.on("data", appendStdout);
      child.stderr?.on("data", appendStderr);

      // Flush any trailing partial lines when the process exits
      child.once("exit", () => {
        // Trigger flush by sending a final newline through each handler
        appendStdout(Buffer.from("\n"));
        appendStderr(Buffer.from("\n"));
      });

      return {
        id: handleId,
        runtimeName: "process",
        data: {
          pid: child.pid,
          createdAt: entry.createdAt,
        },
      };
    },

    async destroy(handle: RuntimeHandle): Promise<void> {
      // PTY host path (Windows) — kill via named pipe + process tree
      const pipePath = (handle.data as Record<string, unknown>)?.pipePath as string | undefined;
      if (pipePath) {
        // Ask the pty-host to dispose its ConPTY handle and exit gracefully.
        await ptyHostKill(pipePath);
        const ptyHostPid = (handle.data as Record<string, unknown>)?.ptyHostPid;
        if (typeof ptyHostPid === "number" && ptyHostPid > 0) {
          // Give the host ~500ms to shut down cleanly so node-pty can release
          // the ConPTY. SIGKILLing immediately orphans the
          // conpty_console_list_agent helper and triggers Windows Error
          // Reporting dialogs (0x800700e8).
          const deadline = Date.now() + 500;
          while (Date.now() < deadline) {
            try {
              process.kill(ptyHostPid, 0); // probe
            } catch (err: unknown) {
              // EPERM: the process exists but we lack permission to signal it
              // (cross-context on Windows). It is NOT gone — fall through to
              // killProcessTree so the orphan is reaped. Any other error code
              // (typically ESRCH) means the process is gone — clean exit.
              if ((err as { code?: string }).code === "EPERM") break;
              processes.delete(handle.id);
              try {
                unregisterWindowsPtyHost(handle.id);
              } catch {
                /* best effort */
              }
              return; // already gone — clean exit
            }
            await new Promise((r) => setTimeout(r, 25));
          }
          await killProcessTree(ptyHostPid, "SIGKILL");
        }
        processes.delete(handle.id);
        try {
          unregisterWindowsPtyHost(handle.id);
        } catch {
          /* best effort */
        }
        return;
      }

      const entry = processes.get(handle.id);
      if (!entry) {
        // Process was spawned by a different Node.js process (e.g. ao spawn).
        // The in-memory Map doesn't have it, but handle.data.pid has the OS PID.
        const pid = (handle.data as Record<string, unknown>)?.pid;
        if (typeof pid === "number" && pid > 0) {
          await killProcessTree(pid, "SIGKILL");
        }
        return;
      }

      const child = entry.process;
      if (!child) {
        // Process hasn't spawned yet — just remove the reservation
        processes.delete(handle.id);
        return;
      }
      if (child.exitCode === null && child.signalCode === null) {
        const pid = child.pid;

        // Register the exit listener BEFORE sending the kill signal to avoid
        // the race where the process exits during the async killProcessTree
        // call and the "exit" event fires before the listener is attached,
        // causing the full 5-second timeout to always elapse on Windows.
        const waitForExit = new Promise<void>((resolve) => {
          const timeout = setTimeout(() => {
            Promise.resolve(
              child.exitCode === null && child.signalCode === null
                ? pid
                  ? killProcessTree(pid, "SIGKILL")
                  : void child.kill("SIGKILL")
                : undefined,
            )
              .catch(() => {})
              .finally(resolve);
          }, 5000);

          child.once("exit", () => {
            clearTimeout(timeout);
            resolve();
          });
        });

        // Send SIGTERM after the listener is registered so we cannot miss
        // the exit event regardless of how quickly the process terminates.
        if (pid) {
          await killProcessTree(pid, "SIGTERM");
        } else {
          child.kill("SIGTERM");
        }

        await waitForExit;
      }

      processes.delete(handle.id);
    },

    async sendMessage(handle: RuntimeHandle, message: string): Promise<void> {
      // PTY host path (Windows)
      const pipePath = (handle.data as Record<string, unknown>)?.pipePath as string | undefined;
      if (pipePath) {
        await ptyHostSendMessage(pipePath, message);
        return;
      }

      await writeManagedStdin(handle, message + "\n");
    },

    async submitInput(handle: RuntimeHandle): Promise<void> {
      const pipePath = (handle.data as Record<string, unknown>)?.pipePath as string | undefined;
      if (pipePath) {
        // The text is already in Codex's editor, so recovery sends only Enter.
        await ptyHostSendRaw(pipePath, "\r");
        return;
      }

      await writeManagedStdin(handle, "\n");
    },

    async interrupt(handle: RuntimeHandle): Promise<void> {
      // Send Escape to cancel the agent's in-flight generation, halting token
      // spend (e.g. for an over-budget session) while keeping the process alive.
      const pipePath = (handle.data as Record<string, unknown>)?.pipePath as string | undefined;
      if (pipePath) {
        await ptyHostSendRaw(pipePath, INTERRUPT_KEY);
        return;
      }

      const entry = processes.get(handle.id);
      const stdin = entry?.process?.stdin;
      if (stdin?.writable) {
        // Await the write (mirroring sendMessage) so an async failure —
        // EPIPE/ERR_STREAM_DESTROYED when the child exits or closes stdin while
        // we're writing — surfaces as a rejection instead of a fire-and-forget.
        // A fire-and-forget would (a) let the lifecycle manager latch
        // budgetInterrupted=true even though Escape never landed, suppressing
        // retries, and (b) emit an unhandled 'error' event that can crash AO.
        await new Promise<void>((resolve, reject) => {
          let done = false;
          const finish = (err?: Error | null) => {
            if (done) return;
            done = true;
            stdin.removeListener("error", onError);
            if (err) reject(err);
            else resolve();
          };
          const onError = (err: Error) => finish(err);
          stdin.on("error", onError);
          stdin.write(INTERRUPT_KEY, (err) => finish(err ?? null));
        });
        return;
      }

      // No in-memory entry: the session was recovered from metadata or launched
      // by a previous AO process, so this process does not own the child's stdin
      // and cannot write Escape. The Unix child was spawned detached (its own
      // process group, pgid == pid), so we deliver SIGINT to that group via the
      // persisted PID — the same cancel signal a terminal Ctrl+C sends to its
      // foreground group. This halts the in-flight generation without the
      // SIGKILL teardown destroy() uses, which would trip the dead-runtime
      // reconciler (#1735) and read as killed rather than paused.
      //
      // Negative-PID process-group signalling is POSIX-only (see
      // docs/CROSS_PLATFORM.md). On Windows there is no equivalent
      // non-destructive interrupt for a foreign child — a raw process.kill(pid)
      // would forcibly terminate it — so we signal only on POSIX and otherwise
      // fail loudly rather than do something destructive.
      const pid = (handle.data as Record<string, unknown>)?.pid;
      if (!isWindows() && typeof pid === "number" && pid > 0) {
        try {
          // Negative pid signals the whole process group, reaching the agent
          // binary even though the persisted pid is the shell leader.
          process.kill(-pid, "SIGINT");
          return;
        } catch (err: unknown) {
          // ESRCH: the group is already gone — nothing to interrupt.
          if ((err as NodeJS.ErrnoException).code === "ESRCH") return;
          // Group signalling not available/permitted — fall back to the leader.
          try {
            process.kill(pid, "SIGINT");
            return;
          } catch (err2: unknown) {
            if ((err2 as NodeJS.ErrnoException).code === "ESRCH") return;
            throw new Error(
              `cannot interrupt process session ${handle.id} via persisted PID ${pid}: ` +
                `${err2 instanceof Error ? err2.message : String(err2)}`,
              { cause: err2 },
            );
          }
        }
      }

      // No stdin handle and no POSIX process-group channel (no persisted PID, or
      // running on Windows): there is genuinely no non-destructive way to
      // interrupt this session. Fail loudly rather than resolve — a silent
      // success would let the lifecycle manager latch the session as
      // "interrupted" while the agent keeps spending.
      throw new Error(
        `cannot interrupt process session ${handle.id}: no in-memory stdin handle and no ` +
          `POSIX process-group channel (session not owned by this AO process — recovered from ` +
          `metadata or started by a prior run${isWindows() ? "; negative-PID signalling is POSIX-only" : ""})`,
      );
    },

    async getOutput(handle: RuntimeHandle, lines = 50): Promise<string> {
      const pipePath = (handle.data as Record<string, unknown>)?.pipePath as string | undefined;
      if (pipePath) {
        return ptyHostGetOutput(pipePath, lines);
      }

      const entry = processes.get(handle.id);
      if (!entry) return "";

      const buffer = entry.outputBuffer;
      const start = Math.max(0, buffer.length - lines);
      return buffer.slice(start).join("\n");
    },

    async isAlive(handle: RuntimeHandle): Promise<boolean> {
      const pipePath = (handle.data as Record<string, unknown>)?.pipePath as string | undefined;
      if (pipePath) {
        return ptyHostIsAlive(pipePath);
      }

      const entry = processes.get(handle.id);
      if (!entry || !entry.process) {
        // Not in our in-memory Map — check via PID signal-0
        const pid = (handle.data as Record<string, unknown>)?.pid;
        if (typeof pid === "number" && pid > 0) {
          try {
            process.kill(pid, 0);
            return true;
          } catch (err: unknown) {
            // EPERM means process exists but we don't have permission — still alive
            if ((err as NodeJS.ErrnoException).code === "EPERM") return true;
            return false;
          }
        }
        return false;
      }
      return entry.process.exitCode === null && entry.process.signalCode === null;
    },

    async getMetrics(handle: RuntimeHandle): Promise<RuntimeMetrics> {
      const entry = processes.get(handle.id);
      const createdAt = entry?.createdAt ?? Date.now();
      return {
        uptimeMs: Date.now() - createdAt,
      };
    },

    async getAttachInfo(handle: RuntimeHandle): Promise<AttachInfo> {
      const pipePath = (handle.data as Record<string, unknown>)?.pipePath as string | undefined;
      if (pipePath) {
        const alive = await ptyHostIsAlive(pipePath);
        if (!alive) {
          return { type: "process", target: "", command: `# process for session ${handle.id} is no longer running` };
        }
        return { type: "process", target: String((handle.data as Record<string, unknown>)?.pid ?? ""), command: pipePath };
      }

      const entry = processes.get(handle.id);
      if (
        !entry ||
        !entry.process ||
        entry.process.exitCode !== null ||
        entry.process.signalCode !== null
      ) {
        return {
          type: "process",
          target: "",
          command: `# process for session ${handle.id} is no longer running`,
        };
      }
      return {
        type: "process",
        target: String(entry.process.pid),
      };
    },
  };
}

/**
 * Sweep all registered Windows pty-hosts: send a graceful kill via the named
 * pipe (so node-pty disposes its ConPTY handle and avoids the WER 0x800700e8
 * dialog), wait briefly, then SIGKILL stragglers via the OS process tree.
 *
 * Invoked by `ao stop` on Windows so orphaned pty-hosts (whose session JSON
 * was wiped or whose parent died ungracefully) still get cleaned up.
 *
 * Returns counts for diagnostics. No-op on non-Windows.
 */
export async function sweepWindowsPtyHosts(): Promise<{
  attempted: number;
  gracefullyExited: number;
  forceKilled: number;
  failed: number;
}> {
  if (!isWindows()) {
    return { attempted: 0, gracefullyExited: 0, forceKilled: 0, failed: 0 };
  }
  const { getWindowsPtyHosts, unregisterWindowsPtyHost } = await import("@aoagents/ao-core");
  const entries = getWindowsPtyHosts();
  let gracefullyExited = 0;
  let forceKilled = 0;
  let failed = 0;
  for (const entry of entries) {
    try {
      // Step 1: graceful kill via the existing pipe protocol.
      await ptyHostKill(entry.pipePath);

      // Step 2: poll up to 500ms for the host to exit.
      const deadline = Date.now() + 500;
      let exited = false;
      while (Date.now() < deadline) {
        try {
          process.kill(entry.ptyHostPid, 0);
        } catch (err: unknown) {
          // EPERM: process exists but we can't signal it (cross-context on
          // Windows). It is NOT gone — fall through to force-kill. Any other
          // code (typically ESRCH) means it has already exited.
          if ((err as { code?: string }).code !== "EPERM") {
            exited = true;
          }
          break;
        }
        await new Promise((r) => setTimeout(r, 25));
      }

      if (exited) {
        gracefullyExited++;
      } else {
        // Step 3: force-kill stragglers.
        await killProcessTree(entry.ptyHostPid, "SIGKILL");
        forceKilled++;
      }

      try {
        unregisterWindowsPtyHost(entry.sessionId);
      } catch {
        /* best effort */
      }
    } catch {
      failed++;
    }
  }
  return { attempted: entries.length, gracefullyExited, forceKilled, failed };
}

export default { manifest, create } satisfies PluginModule<Runtime>;
