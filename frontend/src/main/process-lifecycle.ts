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

type TerminateProcessTreeOptions = {
	platform?: NodeJS.Platform;
	signalProcess?: SignalProcess;
	runTaskkill?: (pid: number) => Promise<void>;
};

function runTaskkill(pid: number): Promise<void> {
	return new Promise((resolve, reject) => {
		execFile("taskkill", ["/PID", String(pid), "/T", "/F"], { windowsHide: true }, (error) => {
			if (error) reject(error);
			else resolve();
		});
	});
}

/**
 * Start a cross-platform process-tree stop, or confirm that the PID is already
 * dead. Callers may report success once this returns true.
 */
export async function terminateProcessTree(pid: number, options: TerminateProcessTreeOptions = {}): Promise<boolean> {
	const signalProcess = options.signalProcess ?? process.kill.bind(process);
	if (!Number.isSafeInteger(pid) || pid <= 0) return false;
	if (probeProcessLiveness(pid, signalProcess) === "dead") return true;

	if ((options.platform ?? process.platform) === "win32") {
		try {
			await (options.runTaskkill ?? runTaskkill)(pid);
			return true;
		} catch {
			return probeProcessLiveness(pid, signalProcess) === "dead";
		}
	}

	try {
		signalProcess(-pid, "SIGTERM");
		return true;
	} catch {
		try {
			signalProcess(pid, "SIGTERM");
			return true;
		} catch {
			return probeProcessLiveness(pid, signalProcess) === "dead";
		}
	}
}
