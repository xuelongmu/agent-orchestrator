import type { DaemonLaunchSpec } from "../shared/daemon-launch";

export const DEFAULT_DAEMON_LAUNCH_AGENT_LABEL = "dev.ao.daemon";

export type DaemonLaunchAgent = {
	label: string;
	domain: string;
	serviceTarget: string;
	plistPath: string;
	stdoutPath: string;
	stderrPath: string;
};

export type DaemonLaunchAgentEnvironment = {
	persisted: NodeJS.ProcessEnv;
	transient: Array<[string, string]>;
};

const PERSISTED_ENVIRONMENT_KEYS = new Set([
	"HOME",
	"USER",
	"LOGNAME",
	"SHELL",
	"TMPDIR",
	"PATH",
	"TERM",
	"LANG",
	"AO_AGENT",
	"AO_ALLOWED_ORIGINS",
	"AO_DATA_DIR",
	"AO_HOST",
	"AO_PORT",
	"AO_REQUEST_TIMEOUT",
	"AO_RUN_FILE",
	"AO_SHUTDOWN_TIMEOUT",
	"AO_TELEMETRY_EVENTS",
	"AO_TELEMETRY_POSTHOG_HOST",
	"AO_TELEMETRY_REMOTE",
]);

export function splitDaemonLaunchAgentEnvironment(env: NodeJS.ProcessEnv): DaemonLaunchAgentEnvironment {
	const persisted: NodeJS.ProcessEnv = {};
	const transient: Array<[string, string]> = [];
	for (const [key, value] of Object.entries(env)) {
		if (typeof value !== "string") continue;
		if (PERSISTED_ENVIRONMENT_KEYS.has(key) || key.startsWith("LC_")) {
			persisted[key] = value;
		} else {
			transient.push([key, value]);
		}
	}
	transient.sort(([a], [b]) => a.localeCompare(b));
	return { persisted, transient };
}

export function parseDaemonLaunchAgentPID(output: string): number | null {
	const match = output.match(/^\s*pid = (\d+)\s*$/m);
	if (!match) return null;
	const pid = Number(match[1]);
	return Number.isSafeInteger(pid) && pid > 0 ? pid : null;
}

export function daemonLaunchAgentOwnsPID(launchAgentPID: number | null, daemonPID: number | undefined): boolean {
	return launchAgentPID !== null && daemonPID !== undefined && launchAgentPID === daemonPID;
}

function joinPath(...segments: string[]): string {
	return segments.map((segment) => segment.replace(/\/+$/, "")).join("/");
}

function stableSuffix(value: string): string {
	let hash = 0x811c9dc5;
	for (let i = 0; i < value.length; i += 1) {
		hash ^= value.charCodeAt(i);
		hash = Math.imul(hash, 0x01000193);
	}
	return (hash >>> 0).toString(16).padStart(8, "0");
}

export function resolveDaemonLaunchAgent(
	runFilePath: string,
	defaultRunFilePath: string,
	uid: number,
): DaemonLaunchAgent {
	const stateDir = runFilePath.replace(/[/\\][^/\\]+$/, "");
	const label =
		runFilePath === defaultRunFilePath
			? DEFAULT_DAEMON_LAUNCH_AGENT_LABEL
			: `${DEFAULT_DAEMON_LAUNCH_AGENT_LABEL}.${stableSuffix(runFilePath)}`;
	const domain = `gui/${uid}`;
	return {
		label,
		domain,
		serviceTarget: `${domain}/${label}`,
		plistPath: joinPath(stateDir, "launchd", `${label}.plist`),
		stdoutPath: joinPath(stateDir, `${label}.stdout.log`),
		stderrPath: joinPath(stateDir, `${label}.stderr.log`),
	};
}

function xml(value: string): string {
	return value
		.replaceAll("&", "&amp;")
		.replaceAll("<", "&lt;")
		.replaceAll(">", "&gt;")
		.replaceAll('"', "&quot;")
		.replaceAll("'", "&apos;");
}

function plistString(value: string, indent: string): string {
	return `${indent}<string>${xml(value)}</string>`;
}

export function renderDaemonLaunchAgentPlist(
	job: DaemonLaunchAgent,
	launch: DaemonLaunchSpec,
	env: NodeJS.ProcessEnv,
): string {
	const args = launch.source === "configured" ? ["/bin/sh", "-lc", launch.command] : [launch.command, ...launch.args];
	const environment = Object.entries(env)
		.filter((entry): entry is [string, string] => typeof entry[1] === "string")
		.sort(([a], [b]) => a.localeCompare(b));

	return [
		'<?xml version="1.0" encoding="UTF-8"?>',
		'<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">',
		'<plist version="1.0">',
		"<dict>",
		"  <key>Label</key>",
		plistString(job.label, "  "),
		"  <key>ProgramArguments</key>",
		"  <array>",
		...args.map((arg) => plistString(arg, "    ")),
		"  </array>",
		"  <key>WorkingDirectory</key>",
		plistString(launch.cwd, "  "),
		"  <key>EnvironmentVariables</key>",
		"  <dict>",
		...environment.flatMap(([key, value]) => [`    <key>${xml(key)}</key>`, plistString(value, "    ")]),
		"  </dict>",
		"  <key>RunAtLoad</key>",
		"  <true/>",
		"  <key>KeepAlive</key>",
		"  <dict>",
		"    <key>SuccessfulExit</key>",
		"    <false/>",
		"  </dict>",
		"  <key>LimitLoadToSessionType</key>",
		"  <string>Aqua</string>",
		"  <key>StandardOutPath</key>",
		plistString(job.stdoutPath, "  "),
		"  <key>StandardErrorPath</key>",
		plistString(job.stderrPath, "  "),
		"</dict>",
		"</plist>",
		"",
	].join("\n");
}
