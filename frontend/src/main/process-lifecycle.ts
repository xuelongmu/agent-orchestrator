import { execFile } from "node:child_process";

export type ProcessLiveness = "alive" | "dead" | "unknown";

type SignalProcess = (pid: number, signal?: NodeJS.Signals | number) => boolean;

/**
 * Probe a PID without treating permission or other indeterminate failures as
 * proof that the process died. EPERM specifically means alive-but-unsignalable
 * on Windows; only ESRCH is durable dead-PID evidence.
 */
export function probeProcessLiveness(
	pid: number,
	signalProcess: SignalProcess = process.kill.bind(process),
): ProcessLiveness {
	if (!Number.isSafeInteger(pid) || pid <= 0) return "dead";
	try {
		signalProcess(pid, 0);
		return "alive";
	} catch (error: unknown) {
		const code = (error as NodeJS.ErrnoException).code;
		if (code === "ESRCH") return "dead";
		if (code === "EPERM") return "alive";
		return "unknown";
	}
}

type TerminateProcessOptions = {
	platform?: NodeJS.Platform;
	signalProcess?: SignalProcess;
	runCommand?: (command: string, args: string[]) => Promise<void>;
};

function runCommand(command: string, args: string[]): Promise<void> {
	return new Promise((resolve, reject) => {
		execFile(command, args, { windowsHide: true }, (error) => {
			if (error) reject(error);
			else resolve();
		});
	});
}

/**
 * Start a cross-platform stop for only the requested PID, or confirm that it is
 * already dead. Session-host child processes must be left for reconciliation.
 */
export async function terminateProcess(pid: number, options: TerminateProcessOptions = {}): Promise<boolean> {
	const signalProcess = options.signalProcess ?? process.kill.bind(process);
	if (!Number.isSafeInteger(pid) || pid <= 0) return false;
	if (probeProcessLiveness(pid, signalProcess) === "dead") return true;

	if ((options.platform ?? process.platform) === "win32") {
		try {
			// Deliberately omit /T: ConPTY session hosts are daemon children and must
			// survive daemon replacement so boot reconciliation can adopt them.
			await (options.runCommand ?? runCommand)("taskkill", ["/PID", String(pid), "/F"]);
			return true;
		} catch {
			return probeProcessLiveness(pid, signalProcess) === "dead";
		}
	}

	try {
		// Adopted-daemon fallback targets only the daemon. Its session processes
		// intentionally outlive it and are reconciled by the replacement daemon.
		signalProcess(pid, "SIGTERM");
		return true;
	} catch {
		return probeProcessLiveness(pid, signalProcess) === "dead";
	}
}

/** The part of a spawned daemon handle that killDaemon needs. */
export type SignalableChild = {
	pid?: number;
	kill: (signal?: NodeJS.Signals | number) => boolean;
};

/**
 * Stop the daemon Electron spawned. The group signal exists for the shell:true
 * `configured` launch, where the direct child is /bin/sh and the daemon behind it
 * would otherwise be orphaned; dev and bundled launches exec the daemon binary
 * directly, so the group leader already is the daemon. Falls back to a direct
 * kill when the group signal cannot be delivered — including Windows, which has
 * no POSIX process group.
 *
 * This deliberately never kills a process tree. Session hosts (tmux servers,
 * detached ConPTY hosts) are outside the daemon's group and must survive so the
 * replacement daemon can reconcile them.
 */
export function killDaemon(child: SignalableChild, signalProcess: SignalProcess = process.kill.bind(process)): void {
	if (child.pid === undefined) return;
	try {
		signalProcess(-child.pid, "SIGTERM");
	} catch {
		child.kill("SIGTERM");
	}
}
