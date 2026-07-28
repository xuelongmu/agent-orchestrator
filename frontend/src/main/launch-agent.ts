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
		stdoutPath: joinPath(stateDir, "daemon.stdout.log"),
		stderrPath: joinPath(stateDir, "daemon.stderr.log"),
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
		"  <true/>",
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
