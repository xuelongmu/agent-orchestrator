import type { DaemonLaunchSpec } from "../shared/daemon-launch";

export const DEFAULT_DAEMON_LAUNCH_AGENT_LABEL = "dev.ao.daemon";

export type DaemonLaunchAgent = {
	label: string;
	domain: string;
	serviceTarget: string;
	plistPath: string;
	environmentLockPath: string;
	environmentSocketPath: string;
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
	"AO_DESKTOP_BUILD_ID",
	"AO_HOST",
	"AO_LAUNCH_AGENT_ENV_ID",
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

export function daemonLaunchAgentOwnsPID(
	launchAgentPID: number | null,
	daemonPID: number | undefined,
	daemonAncestorPIDs: readonly number[],
): boolean {
	return (
		launchAgentPID !== null &&
		daemonPID !== undefined &&
		(launchAgentPID === daemonPID || daemonAncestorPIDs.includes(launchAgentPID))
	);
}

export function daemonLaunchAgentOwnsSupervisor(command: string, plist: string): boolean {
	return command.includes("ao-daemon-supervisor") && plist.includes("<string>ao-daemon-supervisor</string>");
}

export function daemonLaunchAgentShutdownTimeout(plist: string): string | undefined {
	const match = plist.match(/<key>AO_SHUTDOWN_TIMEOUT<\/key>\s*<string>([^<]*)<\/string>/);
	return match?.[1];
}

export function shouldReplaceDaemonLaunchAgent(options: {
	loaded: boolean;
	ownsDaemon: boolean;
	identityMismatch: boolean;
	definitionChanged: boolean;
}): boolean {
	return options.loaded && options.ownsDaemon && (options.identityMismatch || options.definitionChanged);
}

function joinPath(...segments: string[]): string {
	return segments.map((segment) => segment.replace(/\/+$/, "")).join("/");
}

function normalizePathIdentity(value: string): string {
	const absolute = value.startsWith("/");
	const parts: string[] = [];
	for (const part of value.split(/[/\\]+/)) {
		if (!part || part === ".") continue;
		if (part === "..") {
			parts.pop();
		} else {
			parts.push(part);
		}
	}
	return `${absolute ? "/" : ""}${parts.join("/")}`;
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
	const normalizedRunFile = normalizePathIdentity(runFilePath);
	const normalizedDefaultRunFile = normalizePathIdentity(defaultRunFilePath);
	const stateDir = normalizedRunFile.replace(/[/\\][^/\\]+$/, "");
	const defaultStateDir = normalizedDefaultRunFile.replace(/[/\\][^/\\]+$/, "");
	const label =
		normalizedRunFile === normalizedDefaultRunFile
			? DEFAULT_DAEMON_LAUNCH_AGENT_LABEL
			: `${DEFAULT_DAEMON_LAUNCH_AGENT_LABEL}.${stableSuffix(normalizedRunFile)}`;
	const domain = `gui/${uid}`;
	return {
		label,
		domain,
		serviceTarget: `${domain}/${label}`,
		plistPath: joinPath(stateDir, "launchd", `${label}.plist`),
		environmentLockPath: joinPath(defaultStateDir, "launchd", "environment.lock"),
		environmentSocketPath: joinPath(defaultStateDir, "launchd", `${label}.environment.sock`),
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

export const DAEMON_SUPERVISOR_SCRIPT = [
	"socket=$1",
	"stdout=$2",
	"stderr=$3",
	"shift 3",
	"max_log_bytes=5242880",
	"rotate_log() {",
	"  file=$1",
	'  size=$(/usr/bin/stat -f %z "$file" 2>/dev/null || printf 0)',
	'  if [ "$size" -ge "$max_log_bytes" ]; then /bin/mv -f "$file" "$file.1"; fi',
	"}",
	"pump_log() {",
	"  file=$1",
	'  while IFS= read -r line || [ -n "$line" ]; do',
	'    rotate_log "$file"',
	'    printf "%s\\n" "$line" >>"$file"',
	"  done",
	"}",
	'while IFS= read -r name; do unset "$name"; done < <(compgen -e)',
	'while IFS= read -r -d "" entry; do export "$entry"; done < <(/usr/bin/nc -U "$socket")',
	'if [ "$AO_HANDOFF_COMPLETE" != 1 ]; then',
	'  rotate_log "$stderr"',
	'  echo "AO: private launch environment delivery failed" >>"$stderr"',
	"  exit 78",
	"fi",
	"unset AO_HANDOFF_COMPLETE",
	"child=",
	"attempt=0",
	"delay=1",
	"stop() {",
	'  if [ -n "$child" ]; then',
	'    kill -TERM "$child" 2>/dev/null || true',
	'    wait "$child"',
	"  fi",
	"  exit 0",
	"}",
	"trap stop TERM INT HUP",
	"while :; do",
	"  started=$(/bin/date +%s)",
	'  "$@" > >(pump_log "$stdout") 2> >(pump_log "$stderr") &',
	"  child=$!",
	'  wait "$child"',
	"  status=$?",
	"  child=",
	'  if [ "$status" -eq 0 ]; then exit 0; fi',
	"  ended=$(/bin/date +%s)",
	'  if [ "$((ended - started))" -ge 30 ]; then attempt=0; delay=1; fi',
	"  attempt=$((attempt + 1))",
	'  if [ "$attempt" -ge 4 ]; then',
	'    rotate_log "$stderr"',
	'    echo "AO: daemon restart budget exhausted after $attempt unsuccessful exits" >>"$stderr"',
	'    exit "$status"',
	"  fi",
	'  sleep "$delay"',
	"  delay=$((delay * 2))",
	'  if [ "$delay" -gt 8 ]; then delay=8; fi',
	"done",
].join("\n");

export function renderDaemonLaunchAgentPlist(
	job: DaemonLaunchAgent,
	launch: DaemonLaunchSpec,
	env: NodeJS.ProcessEnv,
): string {
	const daemonArgs =
		launch.source === "configured" ? ["/bin/sh", "-c", launch.command] : [launch.command, ...launch.args];
	const args = [
		"/bin/bash",
		"-c",
		DAEMON_SUPERVISOR_SCRIPT,
		"ao-daemon-supervisor",
		job.environmentSocketPath,
		job.stdoutPath,
		job.stderrPath,
		...daemonArgs,
	];
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
		"  <key>LimitLoadToSessionType</key>",
		"  <string>Aqua</string>",
		"  <key>StandardOutPath</key>",
		"  <string>/dev/null</string>",
		"  <key>StandardErrorPath</key>",
		"  <string>/dev/null</string>",
		"</dict>",
		"</plist>",
		"",
	].join("\n");
}
