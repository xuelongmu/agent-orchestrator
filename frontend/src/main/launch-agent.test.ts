import { describe, expect, it } from "vitest";
import {
	DEFAULT_DAEMON_LAUNCH_AGENT_LABEL,
	renderDaemonLaunchAgentPlist,
	resolveDaemonLaunchAgent,
} from "./launch-agent";

describe("resolveDaemonLaunchAgent", () => {
	it("uses the stable production label for the canonical run file", () => {
		const job = resolveDaemonLaunchAgent("/Users/me/.ao/running.json", "/Users/me/.ao/running.json", 501);
		expect(job).toEqual({
			label: DEFAULT_DAEMON_LAUNCH_AGENT_LABEL,
			domain: "gui/501",
			serviceTarget: "gui/501/dev.ao.daemon",
			plistPath: "/Users/me/.ao/launchd/dev.ao.daemon.plist",
			stdoutPath: "/Users/me/.ao/daemon.stdout.log",
			stderrPath: "/Users/me/.ao/daemon.stderr.log",
		});
	});

	it("gives an isolated run file a deterministic non-conflicting label", () => {
		const first = resolveDaemonLaunchAgent("/Users/me/.ao/dev/running.json", "/Users/me/.ao/running.json", 501);
		const second = resolveDaemonLaunchAgent("/Users/me/.ao/dev/running.json", "/Users/me/.ao/running.json", 501);
		expect(first.label).toBe(second.label);
		expect(first.label).toMatch(/^dev\.ao\.daemon\.[0-9a-f]{8}$/);
		expect(first.plistPath).toContain("/Users/me/.ao/dev/launchd/");
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
			{ PATH: "/usr/bin:/bin", TOKEN: 'a&b<"c">' },
		);

		expect(plist).toContain("<key>LimitLoadToSessionType</key>\n  <string>Aqua</string>");
		expect(plist).toContain("<string>/Applications/AO &amp; Friends/ao</string>");
		expect(plist).toContain("<string>/tmp/a &lt; b</string>");
		expect(plist).toContain("<string>a&amp;b&lt;&quot;c&quot;&gt;</string>");
		expect(plist).toContain("<key>KeepAlive</key>\n  <true/>");
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
