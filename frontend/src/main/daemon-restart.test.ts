// @vitest-environment node
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { DaemonRestartController } from "./daemon-restart";

describe("DaemonRestartController", () => {
	beforeEach(() => {
		vi.useFakeTimers();
	});

	afterEach(() => {
		vi.useRealTimers();
	});

	function createController(overrides: { shouldRestart?: () => boolean; maxRestarts?: number } = {}) {
		const restart = vi.fn();
		const log = vi.fn();
		const controller = new DaemonRestartController({
			restart,
			shouldRestart: overrides.shouldRestart ?? (() => true),
			log,
			maxRestarts: overrides.maxRestarts,
			restartDelayMs: 100,
			stableWindowMs: 1_000,
		});
		return { controller, restart, log };
	}

	it("restarts once after an unexpected exit", async () => {
		const { controller, restart, log } = createController();

		controller.onUnexpectedExit();
		expect(restart).not.toHaveBeenCalled();

		await vi.advanceTimersByTimeAsync(100);

		expect(restart).toHaveBeenCalledTimes(1);
		expect(log).toHaveBeenCalledWith("daemon exited unexpectedly; scheduling restart 1/3");
		expect(log).toHaveBeenCalledWith("restarting daemon (1/3)");
	});

	it("bounds a crash loop", async () => {
		const { controller, restart, log } = createController({ maxRestarts: 2 });

		for (let crash = 0; crash < 3; crash += 1) {
			controller.onUnexpectedExit();
			await vi.advanceTimersByTimeAsync(100);
		}

		expect(restart).toHaveBeenCalledTimes(2);
		expect(log).toHaveBeenCalledWith("daemon restart limit reached (2); leaving it stopped");
	});

	it("restores the retry budget only after the daemon stays ready", async () => {
		const { controller, restart } = createController({ maxRestarts: 1 });

		controller.onUnexpectedExit();
		await vi.advanceTimersByTimeAsync(100);
		controller.onReady();
		await vi.advanceTimersByTimeAsync(1_000);
		controller.onUnexpectedExit();
		await vi.advanceTimersByTimeAsync(100);

		expect(restart).toHaveBeenCalledTimes(2);
	});

	it("does not reset the retry budget when readiness is followed by another quick crash", async () => {
		const { controller, restart } = createController({ maxRestarts: 1 });

		controller.onUnexpectedExit();
		await vi.advanceTimersByTimeAsync(100);
		controller.onReady();
		await vi.advanceTimersByTimeAsync(999);
		controller.onUnexpectedExit();
		await vi.advanceTimersByTimeAsync(100);

		expect(restart).toHaveBeenCalledTimes(1);
	});

	it("cancels a pending restart for an intentional stop or app quit", async () => {
		const { controller, restart } = createController();

		controller.onUnexpectedExit();
		controller.cancel();
		await vi.advanceTimersByTimeAsync(100);

		expect(restart).not.toHaveBeenCalled();
	});

	it("does not schedule when the app no longer permits restarts", async () => {
		const { controller, restart } = createController({ shouldRestart: () => false });

		controller.onUnexpectedExit();
		await vi.runAllTimersAsync();

		expect(restart).not.toHaveBeenCalled();
	});
});
