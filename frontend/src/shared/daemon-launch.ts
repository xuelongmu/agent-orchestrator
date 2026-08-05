import type { DaemonProbe } from "./daemon-attach";

export type DaemonLaunchSpec = {
	command: string;
	args: string[];
	cwd: string;
	shell: boolean;
	source: "configured" | "bundled" | "dev";
};

export type DaemonIdentityPolicy = {
	enforceDevCheckout: boolean;
	samePath: (a: string, b: string) => boolean;
	pathInside: (child: string, parent: string) => boolean;
};

/** Validate an attached daemon against the launch identity Electron expects. */
export function evaluateDaemonIdentity(
	launch: DaemonLaunchSpec,
	probe: DaemonProbe,
	policy: DaemonIdentityPolicy,
): string | null {
	if (launch.source === "dev") {
		// Development must never silently attach to a daemon from another checkout
		// or a packaged app. The policy remains explicit for unit-level consumers;
		// Electron always enables it for development launches.
		if (!policy.enforceDevCheckout) return null;

		const cwdMatches = probe.workingDirectory ? policy.samePath(probe.workingDirectory, launch.cwd) : false;
		const executableMatches = probe.executablePath ? policy.pathInside(probe.executablePath, launch.cwd) : false;
		if (!probe.workingDirectory && !probe.executablePath) {
			return "An older AO daemon is already running, but it does not report its checkout identity. Stop it and restart this app.";
		}
		if (!cwdMatches && !executableMatches) {
			const actual = probe.workingDirectory ?? probe.executablePath ?? "an unknown location";
			return `Another AO daemon is already running from ${actual}; expected this checkout at ${launch.cwd}. Stop the other daemon before using this checkout.`;
		}
		return null;
	}

	if (launch.source === "bundled") {
		if (!probe.executablePath) {
			return "An older AO daemon is already running, but it does not report its binary path. Stop it and restart this app.";
		}
		if (!policy.samePath(probe.executablePath, launch.command)) {
			return `Another AO daemon is already running from ${probe.executablePath}; expected ${launch.command}. Stop the other daemon before using this app.`;
		}
	}
	return null;
}

function joinPath(...segments: string[]): string {
	return segments.map((segment) => segment.replace(/[/\\]+$/, "")).join("/");
}

export function daemonBinaryName(platform: NodeJS.Platform): string {
	return platform === "win32" ? "ao.exe" : "ao";
}

export function resolveDaemonLaunch(
	env: Record<string, string | undefined>,
	isPackaged: boolean,
	resourcesPath: string,
	appPath: string,
	platform: NodeJS.Platform,
): DaemonLaunchSpec | null {
	const configuredCommand = env.AO_DAEMON_COMMAND?.trim();
	if (configuredCommand) {
		return {
			command: configuredCommand,
			args: [],
			cwd: appPath,
			shell: true,
			source: "configured",
		};
	}

	if (!isPackaged) {
		// Spawn the daemon binary that `predev` builds into frontend/daemon, not
		// `go run ./cmd/ao daemon`. `go run` compiles to a temp binary and execs it
		// as a GRANDCHILD, so Electron's handle would be the `go` wrapper rather
		// than the process it has to observe, signal, and stop. Windows has no
		// process group to fall back on there, which left the real daemon holding
		// the port after Electron quit.
		//
		// cwd stays the backend checkout so the daemon still reports this checkout
		// as its working directory for evaluateDaemonIdentity above.
		return {
			command: joinPath(appPath, "daemon", daemonBinaryName(platform)),
			args: ["daemon"],
			cwd: joinPath(appPath, "..", "backend"),
			shell: false,
			source: "dev",
		};
	}

	return {
		command: joinPath(resourcesPath, "daemon", daemonBinaryName(platform)),
		args: ["daemon"],
		cwd: resourcesPath,
		shell: false,
		source: "bundled",
	};
}
