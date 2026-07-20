import { describe, expect, it } from "vitest";
import { DAEMON_SERVICE_NAME, type DaemonProber, resolveDaemonFromPort } from "./daemon-attach";
import { resolveDevDaemonConfig } from "./dev-daemon-config";
import { evaluateDaemonIdentity, resolveDaemonLaunch, type DaemonLaunchSpec } from "./daemon-launch";

describe("resolveDaemonLaunch", () => {
	it("uses AO_DAEMON_COMMAND when configured", () => {
		expect(resolveDaemonLaunch({ AO_DAEMON_COMMAND: "/tmp/ao daemon" }, false, "/resources", "/app", "darwin")).toEqual(
			{
				command: "/tmp/ao daemon",
				args: [],
				cwd: "/app",
				shell: true,
				source: "configured",
			},
		);
	});

	it("runs the backend daemon from source in dev without an explicit command", () => {
		expect(resolveDaemonLaunch({}, false, "/resources", "/repo/frontend", "darwin")).toEqual({
			command: "go",
			args: ["run", "./cmd/ao", "daemon"],
			cwd: "/repo/frontend/../backend",
			shell: false,
			source: "dev",
		});
	});

	it("uses the bundled daemon binary for packaged macOS/Linux builds", () => {
		expect(
			resolveDaemonLaunch({}, true, "/Applications/Agent Orchestrator.app/Contents/Resources", "/app", "darwin"),
		).toEqual({
			command: "/Applications/Agent Orchestrator.app/Contents/Resources/daemon/ao",
			args: ["daemon"],
			cwd: "/Applications/Agent Orchestrator.app/Contents/Resources",
			shell: false,
			source: "bundled",
		});
	});

	it("uses the bundled daemon exe for packaged Windows builds", () => {
		expect(
			resolveDaemonLaunch(
				{},
				true,
				"C:\\Program Files\\AO\\resources",
				"C:\\Program Files\\AO\\resources\\app.asar",
				"win32",
			),
		).toEqual({
			command: "C:\\Program Files\\AO\\resources/daemon/ao.exe",
			args: ["daemon"],
			cwd: "C:\\Program Files\\AO\\resources",
			shell: false,
			source: "bundled",
		});
	});
});

describe("development daemon attach identity", () => {
	const launch: DaemonLaunchSpec = {
		command: "go",
		args: ["run", "./cmd/ao", "daemon"],
		cwd: "/repo/backend",
		shell: false,
		source: "dev",
	};
	const packagedIdentity = {
		executablePath: "/Applications/Agent Orchestrator.app/Contents/Resources/daemon/ao",
		workingDirectory: "/Applications/Agent Orchestrator.app/Contents/Resources",
	};
	const probe: DaemonProber = async (_port, endpoint) => ({
		status: endpoint === "healthz" ? "ok" : "ready",
		service: DAEMON_SERVICE_NAME,
		pid: 4242,
		...packagedIdentity,
	});
	const samePath = (a: string, b: string) => a === b;
	const pathInside = (child: string, parent: string) => child === parent || child.startsWith(`${parent}/`);

	async function attachWith(env: Record<string, string | undefined>) {
		const devConfig = resolveDevDaemonConfig(env, "/home/tester");
		return resolveDaemonFromPort({
			expectedPort: devConfig.port,
			probe,
			identityError: (daemonProbe) =>
				evaluateDaemonIdentity(launch, daemonProbe, {
					enforceDevCheckout: devConfig.isIsolated,
					samePath,
					pathInside,
				}),
		});
	}

	it("attaches to the canonical packaged daemon in shared development", async () => {
		await expect(attachWith({})).resolves.toMatchObject({ state: "ready", port: 3001, pid: 4242 });
	});

	it("retains strict checkout identity in isolated development", async () => {
		await expect(attachWith({ ISOLATE_DEV: "true" })).resolves.toMatchObject({
			state: "error",
			port: 3002,
			pid: 4242,
			code: "identity_mismatch",
		});
	});
});
