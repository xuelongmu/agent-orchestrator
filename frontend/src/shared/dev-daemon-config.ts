import { DEFAULT_DAEMON_PORT, expectedDaemonPort } from "./daemon-attach";

export const ISOLATED_DEV_DAEMON_PORT = 3002;

export type DevDaemonConfig = {
	isIsolated: boolean;
	port: number;
	runFilePath: string | null;
	dataDir: string | null;
	apiTarget: string;
};

export type DevServerProxyConfig = Record<
	"/api" | "/mux",
	{ target: string; changeOrigin: false; ws?: true }
>;

function joinPath(...segments: string[]): string {
	return segments.map((segment) => segment.replace(/[/\\]+$/, "")).join("/");
}

/**
 * Resolve Electron's development daemon settings and Vite's matching proxy
 * target without reading process state. Development shares the canonical
 * ~/.ao daemon by default; ISOLATE_DEV=true opts into ~/.ao/dev and port 3002.
 */
export function resolveDevDaemonConfig(
	env: Record<string, string | undefined>,
	homeDir: string,
): DevDaemonConfig {
	const isIsolated = env.ISOLATE_DEV === "true";
	const port = env.AO_PORT
		? expectedDaemonPort(env)
		: isIsolated
			? ISOLATED_DEV_DAEMON_PORT
			: DEFAULT_DAEMON_PORT;
	const stateDir = homeDir ? joinPath(homeDir, ".ao", ...(isIsolated ? ["dev"] : [])) : null;

	return {
		isIsolated,
		port,
		runFilePath: env.AO_RUN_FILE || (stateDir ? joinPath(stateDir, "running.json") : null),
		dataDir: env.AO_DATA_DIR || (stateDir ? joinPath(stateDir, "data") : null),
		apiTarget: env.AO_DEV_API_TARGET ?? `http://127.0.0.1:${port}`,
	};
}

export function createDevServerProxy(config: DevDaemonConfig): DevServerProxyConfig {
	return {
		"/api": {
			target: config.apiTarget,
			changeOrigin: false,
		},
		"/mux": {
			target: config.apiTarget,
			changeOrigin: false,
			ws: true,
		},
	};
}
