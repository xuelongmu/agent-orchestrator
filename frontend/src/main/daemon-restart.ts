import type { ProcessLiveness } from "./process-lifecycle";

export type DaemonRestartControllerOptions = {
	restart: () => unknown | Promise<unknown>;
	shouldRestart: () => boolean;
	log: (message: string) => void;
	onExhausted?: () => void;
	maxRestarts?: number;
	restartDelayMs?: number;
	stableWindowMs?: number;
};

export type DaemonSupervisorLink = {
	connected: boolean;
	dispose: () => void;
};

export type AdoptedDaemonLoss = "not-owned" | "graceful-stop" | "still-running" | "crash";

/** Tracks whether Electron owns the daemon and the matching liveness link. */
export class DaemonOwnershipController {
	private ownedByApp = false;
	private supervisorLink: DaemonSupervisorLink | null = null;

	get appOwned(): boolean {
		return this.ownedByApp;
	}

	get hasSupervisorLink(): boolean {
		return this.supervisorLink !== null;
	}

	get supervisorConnected(): boolean {
		return this.supervisorLink?.connected ?? false;
	}

	setAppOwned(appOwned: boolean): void {
		this.ownedByApp = appOwned;
		if (!appOwned) this.clearSupervisorLink();
	}

	replaceSupervisorLink(link: DaemonSupervisorLink): void {
		this.clearSupervisorLink();
		this.supervisorLink = link;
	}

	clear(): void {
		this.ownedByApp = false;
		this.clearSupervisorLink();
	}

	/**
	 * Classify loss of an adopted daemon using durable run-file ownership plus
	 * process liveness. Graceful Go shutdown removes its owned run-file; a crash
	 * leaves the same app-owned PID behind. A still-live PID is never guessed dead.
	 */
	classifyAdoptedLoss(
		info: { pid: number; owner?: string } | null,
		previousPID: number | undefined,
		probeProcess: (pid: number) => ProcessLiveness,
	): AdoptedDaemonLoss {
		if (!this.ownedByApp) return "not-owned";
		if (previousPID === undefined || validatedDaemonOwner(info, previousPID) !== "app") return "graceful-stop";
		return probeProcess(previousPID) === "dead" ? "crash" : "still-running";
	}

	private clearSupervisorLink(): void {
		this.supervisorLink?.dispose();
		this.supervisorLink = null;
	}
}

/** Only trust a run-file owner when it describes the daemon that answered. */
export function validatedDaemonOwner(
	info: { pid: number; owner?: string } | null,
	probedPID: number | undefined,
): string | undefined {
	return info && info.pid === probedPID ? info.owner : undefined;
}

/** SIGINT/SIGTERM and exit 0 are intentional stops; everything else is crash-like. */
export function isCrashLikeDaemonExit(code: number | null, signal: string | null): boolean {
	if (signal === "SIGINT" || signal === "SIGTERM") return false;
	return code !== 0;
}

export type ReachableDaemon<TFromRunFile, TFromPort> =
	{ source: "run-file"; value: TFromRunFile } | { source: "port"; value: TFromPort };

/** Fall back to the expected port before declaring an adopted daemon gone. */
export async function resolveReachableDaemon<TFromRunFile, TFromPort>(
	fromRunFile: () => Promise<TFromRunFile | null>,
	fromExpectedPort: () => Promise<TFromPort | null>,
): Promise<ReachableDaemon<TFromRunFile, TFromPort> | null> {
	const runFileDaemon = await fromRunFile();
	if (runFileDaemon !== null) return { source: "run-file", value: runFileDaemon };
	const portDaemon = await fromExpectedPort();
	return portDaemon === null ? null : { source: "port", value: portDaemon };
}

const DEFAULT_MAX_RESTARTS = 3;
const DEFAULT_RESTART_DELAY_MS = 1_000;
const DEFAULT_STABLE_WINDOW_MS = 30_000;

/**
 * Bounded restart policy for an app-owned daemon process.
 *
 * A daemon that stays ready for stableWindowMs starts a fresh retry budget. An
 * explicit stop or app quit calls cancel(), so those paths never look like a
 * crash and never schedule a replacement process.
 */
export class DaemonRestartController {
	private readonly restart: () => unknown | Promise<unknown>;
	private readonly shouldRestart: () => boolean;
	private readonly log: (message: string) => void;
	private readonly onExhausted: () => void;
	private readonly maxRestarts: number;
	private readonly restartDelayMs: number;
	private readonly stableWindowMs: number;
	private restartAttempts = 0;
	private restartTimer: ReturnType<typeof setTimeout> | undefined;
	private stableTimer: ReturnType<typeof setTimeout> | undefined;

	constructor(options: DaemonRestartControllerOptions) {
		this.restart = options.restart;
		this.shouldRestart = options.shouldRestart;
		this.log = options.log;
		this.onExhausted = options.onExhausted ?? (() => undefined);
		this.maxRestarts = options.maxRestarts ?? DEFAULT_MAX_RESTARTS;
		this.restartDelayMs = options.restartDelayMs ?? DEFAULT_RESTART_DELAY_MS;
		this.stableWindowMs = options.stableWindowMs ?? DEFAULT_STABLE_WINDOW_MS;
	}

	/** Schedule one replacement after an unexpected daemon exit. */
	onUnexpectedExit(): void {
		this.clearStableTimer();
		if (!this.shouldRestart() || this.restartTimer) return;

		if (this.restartAttempts >= this.maxRestarts) {
			this.log(`daemon restart limit reached (${this.maxRestarts}); leaving it stopped`);
			this.onExhausted();
			return;
		}

		this.restartAttempts += 1;
		const attempt = this.restartAttempts;
		this.log(`daemon exited unexpectedly; scheduling restart ${attempt}/${this.maxRestarts}`);
		this.restartTimer = setTimeout(() => {
			this.restartTimer = undefined;
			if (!this.shouldRestart()) return;
			this.log(`restarting daemon (${attempt}/${this.maxRestarts})`);
			void Promise.resolve()
				.then(() => this.restart())
				.catch((error: unknown) => {
					this.log(`daemon restart ${attempt}/${this.maxRestarts} failed: ${String(error)}`);
					this.onUnexpectedExit();
				});
		}, this.restartDelayMs);
	}

	/** Apply the centralized retry policy to loss of an adopted daemon. */
	onAdoptedLoss(loss: AdoptedDaemonLoss): void {
		if (loss === "crash") {
			this.onUnexpectedExit();
		} else if (loss === "still-running") {
			this.clearStableTimer();
		} else if (loss === "graceful-stop" || loss === "not-owned") {
			this.cancel();
		}
	}

	/** Reset the bounded retry budget only after the replacement proves stable. */
	onReady(confirmed = true): void {
		this.clearStableTimer();
		if (!confirmed || this.restartAttempts === 0) return;
		this.stableTimer = setTimeout(() => {
			this.stableTimer = undefined;
			this.restartAttempts = 0;
		}, this.stableWindowMs);
	}

	/** Cancel pending work for intentional stop and app-quit paths. */
	cancel(): void {
		if (this.restartTimer) clearTimeout(this.restartTimer);
		this.restartTimer = undefined;
		this.clearStableTimer();
		this.restartAttempts = 0;
	}

	private clearStableTimer(): void {
		if (this.stableTimer) clearTimeout(this.stableTimer);
		this.stableTimer = undefined;
	}
}

export type AdoptedDaemonStopOptions = {
	appOwned: boolean;
	alreadyStopped: boolean;
	supervisorConnected: boolean;
	pid: number | undefined;
	requestShutdown: () => Promise<boolean>;
	terminateProcess: (pid: number) => Promise<boolean>;
	clearOwnership: () => void;
};

/**
 * Stop an adopted app-owned daemon before releasing its ownership/link state.
 * Success means a shutdown path was initiated or the daemon was confirmed gone.
 */
export async function stopAdoptedDaemon(options: AdoptedDaemonStopOptions): Promise<boolean> {
	if (!options.appOwned || options.alreadyStopped) {
		options.clearOwnership();
		return true;
	}

	try {
		if (await options.requestShutdown()) {
			options.clearOwnership();
			return true;
		}
	} catch {
		// Fall through to the supervisor/PID stop paths.
	}

	if (options.supervisorConnected) {
		// Disposing a connected link starts the daemon's supervisor grace timer.
		options.clearOwnership();
		return true;
	}

	if (options.pid !== undefined) {
		try {
			if (await options.terminateProcess(options.pid)) {
				options.clearOwnership();
				return true;
			}
		} catch {
			// Preserve ownership so a later stop attempt can retry.
		}
	}

	return false;
}
