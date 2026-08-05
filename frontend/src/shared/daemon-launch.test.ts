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

	it("runs the built dev daemon binary directly so Electron owns the daemon process", () => {
		expect(resolveDaemonLaunch({}, false, "/resources", "/repo/frontend", "darwin")).toEqual({
			command: "/repo/frontend/daemon/ao",
			args: ["daemon"],
			cwd: "/repo/frontend/../backend",
			shell: false,
			source: "dev",
		});
	});

	it("uses the dev daemon exe on Windows", () => {
		expect(resolveDaemonLaunch({}, false, "C:\\repo\\resources", "C:\\repo\\frontend", "win32")).toEqual({
			command: "C:\\repo\\frontend/daemon/ao.exe",
			args: ["daemon"],
			cwd: "C:\\repo\\frontend/../backend",
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
		command: "/repo/frontend/daemon/ao",
		args: ["daemon"],
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
					enforceDevCheckout: true,
					samePath,
					pathInside,
				}),
		});
	}

	it("rejects the canonical packaged daemon in shared development", async () => {
		await expect(attachWith({})).resolves.toMatchObject({
			state: "error",
			port: 3001,
			pid: 4242,
			code: "identity_mismatch",
		});
	});

	it("retains strict checkout identity in isolated development", async () => {
		await expect(attachWith({ ISOLATE_DEV: "true" })).resolves.toMatchObject({
			state: "error",
			port: 3002,
			pid: 4242,
			code: "identity_mismatch",
		});
	});

	// The built dev binary lives in frontend/daemon, outside the backend cwd, so
	// isolated dev accepts it on the reported working directory rather than on
	// executable containment.
	it("accepts a daemon built into frontend/daemon and run from the backend checkout", () => {
		const identity = evaluateDaemonIdentity(
			launch,
			{
				status: "ok",
				service: DAEMON_SERVICE_NAME,
				pid: 4242,
				executablePath: "/repo/frontend/daemon/ao",
				workingDirectory: "/repo/backend",
			},
			{ enforceDevCheckout: true, samePath, pathInside },
		);
		expect(identity).toBeNull();
	});

	it("still rejects a daemon built and run from another checkout", () => {
		const identity = evaluateDaemonIdentity(
			launch,
			{
				status: "ok",
				service: DAEMON_SERVICE_NAME,
				pid: 4242,
				executablePath: "/other/frontend/daemon/ao",
				workingDirectory: "/other/backend",
			},
			{ enforceDevCheckout: true, samePath, pathInside },
		);
		expect(identity).toContain("/other/backend");
	});
});
