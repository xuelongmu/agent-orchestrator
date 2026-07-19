export type DaemonRestartControllerOptions = {
	restart: () => void | Promise<void>;
	shouldRestart: () => boolean;
	log: (message: string) => void;
	maxRestarts?: number;
	restartDelayMs?: number;
	stableWindowMs?: number;
};

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
	private readonly restart: () => void | Promise<void>;
	private readonly shouldRestart: () => boolean;
	private readonly log: (message: string) => void;
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
				});
		}, this.restartDelayMs);
	}

	/** Reset the bounded retry budget only after the replacement proves stable. */
	onReady(): void {
		this.clearStableTimer();
		if (this.restartAttempts === 0) return;
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
