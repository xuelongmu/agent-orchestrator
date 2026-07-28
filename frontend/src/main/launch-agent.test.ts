import { describe, expect, it } from "vitest";
import {
	DEFAULT_DAEMON_LAUNCH_AGENT_LABEL,
	renderDaemonLaunchAgentPlist,
	resolveDaemonLaunchAgent,
	splitDaemonLaunchAgentEnvironment,
} from "./launch-agent";

describe("resolveDaemonLaunchAgent", () => {
	it("uses the stable production label for the canonical run file", () => {
		const job = resolveDaemonLaunchAgent("/Users/me/.ao/running.json", "/Users/me/.ao/running.json", 501);
		expect(job).toEqual({
			label: DEFAULT_DAEMON_LAUNCH_AGENT_LABEL,
			domain: "gui/501",
			serviceTarget: "gui/501/dev.ao.daemon",
			plistPath: "/Users/me/.ao/launchd/dev.ao.daemon.plist",
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
		expect(plist).toContain("<string>/Applications/AO &amp; Friends/ao</string>");
		expect(plist).toContain("<string>/tmp/a &lt; b</string>");
		expect(plist).toContain("<string>a&amp;b&lt;&quot;c&quot;&gt;</string>");
		expect(plist).toContain("<key>KeepAlive</key>\n  <dict>\n    <key>SuccessfulExit</key>\n    <false/>\n  </dict>");
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
		expect(plist).toContain("<string>/bin/sh</string>\n    <string>-lc</string>");
		expect(plist).toContain("<string>/opt/ao daemon --flag</string>");
	});
});
