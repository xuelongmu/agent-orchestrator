// @vitest-environment node
import { describe, expect, it } from "vitest";
import { createDevServerProxy, resolveDevDaemonConfig } from "./dev-daemon-config";

const HOME = "/home/tester";

describe("development daemon config", () => {
	it("shares the standard daemon and state by default", () => {
		const config = resolveDevDaemonConfig({}, HOME);

		expect(config).toEqual({
			isIsolated: false,
			port: 3001,
			runFilePath: "/home/tester/.ao/running.json",
			dataDir: "/home/tester/.ao/data",
			apiTarget: "http://127.0.0.1:3001",
		});
		expect(createDevServerProxy(config)).toEqual({
			"/api": { target: "http://127.0.0.1:3001", changeOrigin: false },
			"/mux": { target: "http://127.0.0.1:3001", changeOrigin: false, ws: true },
		});
	});

	it("isolates the daemon, state, and proxies only when explicitly enabled", () => {
		const config = resolveDevDaemonConfig({ ISOLATE_DEV: "true" }, HOME);

		expect(config).toEqual({
			isIsolated: true,
			port: 3002,
			runFilePath: "/home/tester/.ao/dev/running.json",
			dataDir: "/home/tester/.ao/dev/data",
			apiTarget: "http://127.0.0.1:3002",
		});
		expect(createDevServerProxy(config)).toEqual({
			"/api": { target: "http://127.0.0.1:3002", changeOrigin: false },
			"/mux": { target: "http://127.0.0.1:3002", changeOrigin: false, ws: true },
		});
	});

	it("preserves explicit daemon and proxy overrides", () => {
		const config = resolveDevDaemonConfig(
			{
				ISOLATE_DEV: "true",
				AO_PORT: "4100",
				AO_RUN_FILE: "/custom/running.json",
				AO_DATA_DIR: "/custom/data",
				AO_DEV_API_TARGET: "http://daemon.test:4200",
			},
			HOME,
		);

		expect(config).toEqual({
			isIsolated: true,
			port: 4100,
			runFilePath: "/custom/running.json",
			dataDir: "/custom/data",
			apiTarget: "http://daemon.test:4200",
		});
		expect(createDevServerProxy(config)).toEqual({
			"/api": { target: "http://daemon.test:4200", changeOrigin: false },
			"/mux": { target: "http://daemon.test:4200", changeOrigin: false, ws: true },
		});
	});
});
