import { describe, expect, it } from "vitest";
import {
	daemonLaunchAgentOwnsPID,
	DEFAULT_DAEMON_LAUNCH_AGENT_LABEL,
	parseDaemonLaunchAgentPID,
	renderDaemonLaunchAgentPlist,
	resolveDaemonLaunchAgent,
	shouldReplaceDaemonLaunchAgent,
	splitDaemonLaunchAgentEnvironment,
} from "./launch-agent";

describe("parseDaemonLaunchAgentPID", () => {
	it("reads a running service PID and rejects a dormant job", () => {
		expect(parseDaemonLaunchAgentPID("state = running\n\tpid = 4242\n")).toBe(4242);
		expect(parseDaemonLaunchAgentPID("state = exited\n\tlast exit code = 0\n")).toBeNull();
	});

	it("requires the live service PID to match the probed daemon", () => {
		expect(daemonLaunchAgentOwnsPID(4242, 4242, null)).toBe(true);
		expect(daemonLaunchAgentOwnsPID(4242, 5151, 4242)).toBe(true);
		expect(daemonLaunchAgentOwnsPID(null, 4242, null)).toBe(false);
		expect(daemonLaunchAgentOwnsPID(4242, 5151, 6161)).toBe(false);
		expect(daemonLaunchAgentOwnsPID(4242, undefined, null)).toBe(false);
	});

	it("preserves a loaded packaged definition for shared development", () => {
		expect(
			shouldReplaceDaemonLaunchAgent({
				loaded: true,
				ownsDaemon: true,
				identityMismatch: false,
				definitionChanged: true,
				preserveLoadedDefinition: true,
			}),
		).toBe(false);
		expect(
			shouldReplaceDaemonLaunchAgent({
				loaded: true,
				ownsDaemon: true,
				identityMismatch: false,
				definitionChanged: true,
				preserveLoadedDefinition: false,
			}),
		).toBe(true);
	});
});

describe("resolveDaemonLaunchAgent", () => {
	it("uses the stable production label for the canonical run file", () => {
		const job = resolveDaemonLaunchAgent("/Users/me/.ao/running.json", "/Users/me/.ao/running.json", 501);
		expect(job).toEqual({
			label: DEFAULT_DAEMON_LAUNCH_AGENT_LABEL,
			domain: "gui/501",
			serviceTarget: "gui/501/dev.ao.daemon",
			plistPath: "/Users/me/.ao/launchd/dev.ao.daemon.plist",
			environmentLockPath: "/Users/me/.ao/launchd/environment.lock",
			stdoutPath: "/Users/me/.ao/dev.ao.daemon.stdout.log",
			stderrPath: "/Users/me/.ao/dev.ao.daemon.stderr.log",
		});
	});

	it("gives an isolated run file a deterministic non-conflicting label", () => {
		const first = resolveDaemonLaunchAgent("/Users/me/.ao/dev/running.json", "/Users/me/.ao/running.json", 501);
		const second = resolveDaemonLaunchAgent("/Users/me/.ao/dev/running.json", "/Users/me/.ao/running.json", 501);
		expect(first.label).toBe(second.label);
		expect(first.label).toMatch(/^dev\.ao\.daemon\.[0-9a-f]{8}$/);
		expect(first.plistPath).toContain("/Users/me/.ao/dev/launchd/");
		expect(first.environmentLockPath).toBe("/Users/me/.ao/launchd/environment.lock");
	});

	it("maps equivalent canonical run-file spellings to the production label", () => {
		const job = resolveDaemonLaunchAgent("/Users/me/.ao/dev/../running.json", "/Users/me/.ao/./running.json", 501);
		expect(job.label).toBe(DEFAULT_DAEMON_LAUNCH_AGENT_LABEL);
		expect(job.plistPath).toBe("/Users/me/.ao/launchd/dev.ao.daemon.plist");
	});

	it("keeps log files distinct for isolated jobs in the same directory", () => {
		const first = resolveDaemonLaunchAgent("/tmp/ao/first.json", "/Users/me/.ao/running.json", 501);
		const second = resolveDaemonLaunchAgent("/tmp/ao/second.json", "/Users/me/.ao/running.json", 501);
		expect(first.stdoutPath).not.toBe(second.stdoutPath);
		expect(first.stderrPath).not.toBe(second.stderrPath);
	});
});

describe("splitDaemonLaunchAgentEnvironment", () => {
	it("persists only non-secret launch configuration", () => {
		const result = splitDaemonLaunchAgentEnvironment({
			HOME: "/Users/me",
			PATH: "/opt/homebrew/bin:/usr/bin",
			LC_CTYPE: "UTF-8",
			AO_RUN_FILE: "/tmp/ao/running.json",
			AO_DESKTOP_BUILD_ID: "release:0.10.4",
			AO_LAUNCH_AGENT_ENV_ID: "sha256:credential-fingerprint",
			AO_TELEMETRY_POSTHOG_HOST: "https://example.test",
			GH_TOKEN: "github-secret",
			AO_GITHUB_TOKEN: "ao-secret",
			AO_TELEMETRY_POSTHOG_KEY: "telemetry-secret",
			OPENAI_API_KEY: "api-secret",
			HTTPS_PROXY: "https://user:password@example.test",
		});

		expect(result.persisted).toEqual({
			HOME: "/Users/me",
			PATH: "/opt/homebrew/bin:/usr/bin",
			LC_CTYPE: "UTF-8",
			AO_RUN_FILE: "/tmp/ao/running.json",
			AO_DESKTOP_BUILD_ID: "release:0.10.4",
			AO_LAUNCH_AGENT_ENV_ID: "sha256:credential-fingerprint",
			AO_TELEMETRY_POSTHOG_HOST: "https://example.test",
		});
		expect(Object.keys(result.persisted)).not.toContain("GH_TOKEN");
		expect(result.transient).toEqual([
			["AO_GITHUB_TOKEN", "ao-secret"],
			["AO_TELEMETRY_POSTHOG_KEY", "telemetry-secret"],
			["GH_TOKEN", "github-secret"],
			["HTTPS_PROXY", "https://user:password@example.test"],
			["OPENAI_API_KEY", "api-secret"],
		]);

		const job = resolveDaemonLaunchAgent("/Users/me/.ao/running.json", "/Users/me/.ao/running.json", 501);
		const plist = renderDaemonLaunchAgentPlist(
			job,
			{
				command: "/Applications/AO.app/Contents/Resources/ao",
				args: ["daemon"],
				cwd: "/Users/me",
				shell: false,
				source: "bundled",
			},
			result.persisted,
		);
		expect(plist).not.toContain("secret");
		expect(plist).not.toContain("GH_TOKEN");
		expect(plist).toContain("<key>AO_DESKTOP_BUILD_ID</key>\n    <string>release:0.10.4</string>");
		expect(plist).toContain("<key>AO_LAUNCH_AGENT_ENV_ID</key>\n    <string>sha256:credential-fingerprint</string>");
	});
});

describe("renderDaemonLaunchAgentPlist", () => {
	it("pins the daemon to Aqua and escapes paths and environment values", () => {
		const job = resolveDaemonLaunchAgent("/Users/me/.ao/running.json", "/Users/me/.ao/running.json", 501);
		const plist = renderDaemonLaunchAgentPlist(
			job,
			{
				command: "/Applications/AO & Friends/ao",
				args: ["daemon"],
				cwd: "/tmp/a < b",
				shell: false,
				source: "bundled",
			},
			{ PATH: "/usr/bin:/bin", AO_RUN_FILE: 'a&b<"c">' },
		);

		expect(plist).toContain("<key>LimitLoadToSessionType</key>\n  <string>Aqua</string>");
		expect(plist).toContain("<string>ao-daemon-supervisor</string>");
		expect(plist).toContain("daemon restart budget exhausted");
		expect(plist).toContain("<string>/Applications/AO &amp; Friends/ao</string>");
		expect(plist).toContain("<string>/tmp/a &lt; b</string>");
		expect(plist).toContain("<string>a&amp;b&lt;&quot;c&quot;&gt;</string>");
		expect(plist).not.toContain("<key>KeepAlive</key>");
	});

	it("runs an explicitly configured command through the shell", () => {
		const job = resolveDaemonLaunchAgent("/Users/me/.ao/running.json", "/Users/me/.ao/running.json", 501);
		const plist = renderDaemonLaunchAgentPlist(
			job,
			{
				command: "/opt/ao daemon --flag",
				args: [],
				cwd: "/tmp",
				shell: true,
				source: "configured",
			},
			{},
		);
		expect(plist).toContain("<string>/bin/sh</string>\n    <string>-c</string>");
		expect(plist).toContain("<string>/opt/ao daemon --flag</string>");
	});
});
