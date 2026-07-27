// @vitest-environment node
import { spawn } from "node:child_process";
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { describe, expect, it, vi } from "vitest";
import { killDaemon, probeProcessLiveness, terminateProcess } from "./process-lifecycle";

function errno(code: string): NodeJS.ErrnoException {
	return Object.assign(new Error(code), { code });
}

describe("probeProcessLiveness", () => {
	it("distinguishes verified-dead, permission-denied, and indeterminate probes", () => {
		expect(
			probeProcessLiveness(42, () => {
				throw errno("ESRCH");
			}),
		).toBe("dead");
		expect(
			probeProcessLiveness(42, () => {
				throw errno("EPERM");
			}),
		).toBe("alive");
		expect(
			probeProcessLiveness(42, () => {
				throw errno("EACCES");
			}),
		).toBe("unknown");
	});
});

describe("terminateProcess", () => {
	it("uses taskkill without the child-tree flag for a live Windows daemon", async () => {
		const signalProcess = vi.fn(() => true);
		// Declare the real signature so mock.calls is typed as [string, string[]]
		// rather than an empty tuple, which the argument assertions below index.
		const runCommand = vi.fn(async (_command: string, _args: string[]) => undefined);

		await expect(terminateProcess(42, { platform: "win32", signalProcess, runCommand })).resolves.toBe(true);
		expect(runCommand).toHaveBeenCalledWith("taskkill", ["/PID", "42", "/F"]);
		expect(runCommand.mock.calls[0]?.[1]).not.toContain("/T");
		expect(signalProcess).toHaveBeenCalledWith(42, 0);
	});

	it("signals only the daemon PID on POSIX", async () => {
		const signalProcess = vi.fn(() => true);

		await expect(terminateProcess(42, { platform: "linux", signalProcess })).resolves.toBe(true);
		expect(signalProcess).toHaveBeenNthCalledWith(1, 42, 0);
		expect(signalProcess).toHaveBeenNthCalledWith(2, 42, "SIGTERM");
		expect(signalProcess).not.toHaveBeenCalledWith(-42, "SIGTERM");
	});
});

describe("killDaemon", () => {
	it("falls back to a direct kill when the group signal cannot be delivered", () => {
		// Stands in for Windows, which has no POSIX process group: the direct child
		// is the daemon itself, so the fallback is the one that must reach it.
		const kill = vi.fn(() => true);
		killDaemon({ pid: 42, kill }, () => {
			throw errno("EINVAL");
		});
		expect(kill).toHaveBeenCalledWith("SIGTERM");
	});

	it("does nothing for a handle that never got a PID", () => {
		const kill = vi.fn(() => true);
		const signalProcess = vi.fn(() => true);
		killDaemon({ pid: undefined, kill }, signalProcess);
		expect(kill).not.toHaveBeenCalled();
		expect(signalProcess).not.toHaveBeenCalled();
	});
});

// End-to-end shape of the supervisor's stop: Electron spawns the daemon binary
// directly (detached, so it leads its own group) and the daemon has already
// handed its session hosts to their own groups. Stopping the daemon must not
// take those hosts with it — they outlive a daemon restart and are reconciled
// by the replacement daemon.
describe("daemon stop lifecycle", () => {
	// The group signal is a POSIX concept; on Windows killDaemon's direct-kill
	// fallback is covered by the unit test above.
	it.skipIf(process.platform === "win32")(
		"stops the spawned daemon while its detached session host survives",
		async () => {
			const dir = await mkdtemp(path.join(tmpdir(), "ao-daemon-lifecycle-"));
			const hostPidFile = path.join(dir, "session-host.pid");
			// Session hosts self-expire so a failed assertion cannot leak a process.
			const sessionHost = path.join(dir, "session-host.mjs");
			const daemon = path.join(dir, "daemon.mjs");
			await writeFile(sessionHost, "setTimeout(() => process.exit(0), 30_000);\n");
			await writeFile(
				daemon,
				[
					`import { spawn } from "node:child_process";`,
					`import { writeFileSync } from "node:fs";`,
					// `detached` mirrors how the daemon hands a session host its own
					// process group (tmux server / Windows DETACHED_PROCESS ConPTY host).
					`const host = spawn(process.execPath, [${JSON.stringify(sessionHost)}], {`,
					`	detached: true,`,
					`	stdio: "ignore",`,
					`});`,
					`host.unref();`,
					`writeFileSync(${JSON.stringify(hostPidFile)}, String(host.pid));`,
					`setInterval(() => {}, 1000);`,
				].join("\n"),
			);

			const child = spawn(process.execPath, [daemon], { detached: true, stdio: "ignore" });
			const exited = new Promise<void>((resolve) => child.once("exit", () => resolve()));
			let hostPid = 0;
			try {
				const deadline = Date.now() + 10_000;
				while (Date.now() < deadline && hostPid === 0) {
					try {
						hostPid = Number(await readFile(hostPidFile, "utf8"));
					} catch {
						await new Promise((resolve) => setTimeout(resolve, 50));
					}
				}
				expect(hostPid).toBeGreaterThan(0);

				killDaemon(child);
				await exited;

				expect(probeProcessLiveness(child.pid ?? 0)).toBe("dead");
				expect(probeProcessLiveness(hostPid)).toBe("alive");
			} finally {
				if (hostPid > 0) {
					try {
						process.kill(hostPid, "SIGKILL");
					} catch {
						// Already gone.
					}
				}
				child.kill("SIGKILL");
				await rm(dir, { recursive: true, force: true });
			}
		},
	);
});
