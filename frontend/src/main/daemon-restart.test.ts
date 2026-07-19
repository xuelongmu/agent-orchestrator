// @vitest-environment node
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
	DaemonOwnershipController,
	DaemonRestartController,
	DaemonStopIntentController,
	isCrashLikeDaemonExit,
	resolveReachableDaemon,
	stopAdoptedDaemon,
	validatedDaemonOwner,
} from "./daemon-restart";

describe("DaemonRestartController", () => {
	beforeEach(() => {
		vi.useFakeTimers();
	});

	afterEach(() => {
		vi.useRealTimers();
	});

	function createController(
		overrides: {
			shouldRestart?: () => boolean;
			maxRestarts?: number;
			onExhausted?: () => void;
			restart?: () => unknown | Promise<unknown>;
		} = {},
	) {
		const restart = vi.fn(overrides.restart ?? (() => undefined));
		const log = vi.fn();
		const controller = new DaemonRestartController({
			restart,
			shouldRestart: overrides.shouldRestart ?? (() => true),
			log,
			onExhausted: overrides.onExhausted,
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
		const onExhausted = vi.fn();
		const { controller, restart, log } = createController({ maxRestarts: 2, onExhausted });

		for (let crash = 0; crash < 3; crash += 1) {
			controller.onUnexpectedExit();
			await vi.advanceTimersByTimeAsync(100);
		}

		expect(restart).toHaveBeenCalledTimes(2);
		expect(log).toHaveBeenCalledWith("daemon restart limit reached (2); leaving it stopped");
		expect(onExhausted).toHaveBeenCalledTimes(1);
	});

	it("restores the retry budget when confirmed readiness stays stable", async () => {
		const { controller, restart } = createController({ maxRestarts: 1 });

		controller.onUnexpectedExit();
		await vi.advanceTimersByTimeAsync(100);
		controller.onReady();
		await vi.advanceTimersByTimeAsync(1_000);
		controller.onUnexpectedExit();
		await vi.advanceTimersByTimeAsync(100);

		expect(restart).toHaveBeenCalledTimes(2);
	});

	it("does not restore the retry budget for port-unconfirmed renderer readiness", async () => {
		const { controller, restart } = createController({ maxRestarts: 1 });

		controller.onUnexpectedExit();
		await vi.advanceTimersByTimeAsync(100);
		controller.onReady(false);
		await vi.advanceTimersByTimeAsync(1_000);
		controller.onUnexpectedExit();
		await vi.advanceTimersByTimeAsync(100);

		expect(restart).toHaveBeenCalledTimes(1);
	});

	it("cancels a pending stability reset while an adopted PID is still running", async () => {
		const { controller, restart } = createController({ maxRestarts: 1 });

		controller.onUnexpectedExit();
		await vi.advanceTimersByTimeAsync(100);
		controller.onReady();
		await vi.advanceTimersByTimeAsync(500);
		controller.onAdoptedLoss("still-running");
		await vi.advanceTimersByTimeAsync(500);
		controller.onUnexpectedExit();
		await vi.advanceTimersByTimeAsync(100);

		expect(restart).toHaveBeenCalledTimes(1);
	});

	it("cancels a scheduled restart when the adopted PID is later found live without replenishing budget", async () => {
		const onExhausted = vi.fn();
		const { controller, restart } = createController({ maxRestarts: 1, onExhausted });

		controller.onUnexpectedExit();
		controller.onAdoptedLoss("still-running");
		await vi.advanceTimersByTimeAsync(100);

		expect(restart).not.toHaveBeenCalled();

		controller.onUnexpectedExit();
		await vi.advanceTimersByTimeAsync(100);

		expect(restart).not.toHaveBeenCalled();
		expect(onExhausted).toHaveBeenCalledTimes(1);
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

	it("continues the bounded policy when a replacement reports a spawn error", async () => {
		const { controller, restart } = createController();

		controller.onUnexpectedExit();
		await vi.advanceTimersByTimeAsync(100);
		// The replacement ChildProcess emitted error instead of exit.
		controller.onUnexpectedExit();
		await vi.advanceTimersByTimeAsync(100);

		expect(restart).toHaveBeenCalledTimes(2);
	});

	it("continues the bounded policy when starting a replacement rejects", async () => {
		const onExhausted = vi.fn();
		const { controller, restart } = createController({
			maxRestarts: 2,
			onExhausted,
			restart: () => Promise.reject(new Error("launch failed")),
		});

		controller.onUnexpectedExit();
		await vi.advanceTimersByTimeAsync(200);

		expect(restart).toHaveBeenCalledTimes(2);
		expect(onExhausted).toHaveBeenCalledTimes(1);
	});

	it("suppresses duplicate restart requests while one is pending", async () => {
		const { controller, restart } = createController();

		controller.onUnexpectedExit();
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

	it("leaves an adopted graceful stop stopped", async () => {
		const ownership = new DaemonOwnershipController();
		ownership.setAppOwned(true);
		const { controller, restart } = createController();
		const loss = ownership.classifyAdoptedLoss(null, 42, () => "dead");

		controller.onAdoptedLoss(loss);
		await vi.runAllTimersAsync();

		expect(loss).toBe("graceful-stop");
		expect(restart).not.toHaveBeenCalled();
	});

	it("restarts an adopted daemon whose dead PID retains its app-owned run-file", async () => {
		const ownership = new DaemonOwnershipController();
		ownership.setAppOwned(true);
		const { controller, restart } = createController();
		const loss = ownership.classifyAdoptedLoss({ pid: 42, owner: "app" }, 42, () => "dead");

		controller.onAdoptedLoss(loss);
		await vi.advanceTimersByTimeAsync(100);

		expect(loss).toBe("crash");
		expect(restart).toHaveBeenCalledTimes(1);
	});
});

describe("daemon restart ownership and reachability", () => {
	it("disposes and clears a stale supervisor link when adopting a headless daemon", () => {
		const ownership = new DaemonOwnershipController();
		const link = { connected: true, dispose: vi.fn() };
		ownership.setAppOwned(true);
		ownership.replaceSupervisorLink(link);

		ownership.setAppOwned(false);

		expect(link.dispose).toHaveBeenCalledTimes(1);
		expect(ownership.appOwned).toBe(false);
		expect(ownership.hasSupervisorLink).toBe(false);
		expect(ownership.supervisorConnected).toBe(false);
	});

	it("trusts direct-port ownership only when running.json matches the probed PID", () => {
		const info = { pid: 42, owner: "app" };

		expect(validatedDaemonOwner(info, 42)).toBe("app");
		expect(validatedDaemonOwner(info, 99)).toBeUndefined();
		expect(validatedDaemonOwner(null, 42)).toBeUndefined();
	});

	it("uses the expected-port probe before declaring an adopted daemon unreachable", async () => {
		const fromRunFile = vi.fn().mockResolvedValue(null);
		const ready = { state: "ready" as const, port: 3001, pid: 42 };
		const fromExpectedPort = vi.fn().mockResolvedValue(ready);

		await expect(resolveReachableDaemon(fromRunFile, fromExpectedPort)).resolves.toEqual({
			source: "port",
			value: ready,
		});
		expect(fromExpectedPort).toHaveBeenCalledTimes(1);
	});

	it("does not probe the port when the validated run-file daemon is reachable", async () => {
		const fromRunFile = vi.fn().mockResolvedValue({ status: { state: "ready" }, owner: "app" });
		const fromExpectedPort = vi.fn();

		await expect(resolveReachableDaemon(fromRunFile, fromExpectedPort)).resolves.toMatchObject({
			source: "run-file",
		});
		expect(fromExpectedPort).not.toHaveBeenCalled();
	});

	it("does not guess that an unreachable adopted daemon with a live PID crashed", () => {
		const ownership = new DaemonOwnershipController();
		ownership.setAppOwned(true);

		const loss = ownership.classifyAdoptedLoss({ pid: 42, owner: "app" }, 42, () => "alive");

		expect(loss).toBe("still-running");
	});

	it("does not guess that an indeterminate adopted PID probe is a crash", () => {
		const ownership = new DaemonOwnershipController();
		ownership.setAppOwned(true);

		const loss = ownership.classifyAdoptedLoss({ pid: 42, owner: "app" }, 42, () => "unknown");

		expect(loss).toBe("still-running");
	});
});

describe("adopted daemon stop", () => {
	it("invalidates an in-flight refresh and suppresses re-adoption until an explicit start", () => {
		const intent = new DaemonStopIntentController();
		const inFlightRefreshEpoch = intent.currentEpoch;

		intent.requestStop();

		expect(intent.permitsAdoption(inFlightRefreshEpoch)).toBe(false);
		expect(intent.permitsAdoption()).toBe(false);

		intent.allowExplicitStart();
		expect(intent.permitsAdoption()).toBe(true);
		expect(intent.permitsAdoption(inFlightRefreshEpoch)).toBe(false);
	});

	it("falls back to the owned PID before clearing a disconnected supervisor link", async () => {
		const events: string[] = [];
		const stopped = await stopAdoptedDaemon({
			appOwned: true,
			alreadyStopped: false,
			supervisorConnected: false,
			pid: 42,
			requestShutdown: async () => {
				events.push("shutdown-request");
				return false;
			},
			confirmStopped: async () => true,
			terminateProcess: async (pid) => {
				events.push(`terminate-${pid}`);
				return true;
			},
			clearOwnership: () => events.push("clear"),
		});

		expect(stopped).toBe(true);
		expect(events).toEqual(["shutdown-request", "terminate-42", "clear"]);
	});

	it("retains ownership until an accepted HTTP shutdown is confirmed", async () => {
		let confirmStop: (stopped: boolean) => void = () => undefined;
		const confirmation = new Promise<boolean>((resolve) => {
			confirmStop = resolve;
		});
		const clearOwnership = vi.fn();
		const terminateProcess = vi.fn(async () => true);
		let settled = false;
		const stopPromise = stopAdoptedDaemon({
			appOwned: true,
			alreadyStopped: false,
			supervisorConnected: true,
			pid: 42,
			requestShutdown: async () => true,
			confirmStopped: () => confirmation,
			terminateProcess,
			clearOwnership,
		}).then((result) => {
			settled = true;
			return result;
		});
		await Promise.resolve();
		await Promise.resolve();

		expect(settled).toBe(false);
		expect(clearOwnership).not.toHaveBeenCalled();
		expect(terminateProcess).not.toHaveBeenCalled();

		confirmStop(true);
		await expect(stopPromise).resolves.toBe(true);
		expect(clearOwnership).toHaveBeenCalledTimes(1);
		expect(terminateProcess).not.toHaveBeenCalled();
	});

	it("falls back to single-PID termination when accepted HTTP shutdown is not confirmed", async () => {
		const events: string[] = [];
		const stopped = await stopAdoptedDaemon({
			appOwned: true,
			alreadyStopped: false,
			supervisorConnected: true,
			pid: 42,
			requestShutdown: async () => {
				events.push("shutdown-request");
				return true;
			},
			confirmStopped: async () => {
				events.push("confirm");
				return false;
			},
			terminateProcess: async (pid) => {
				events.push(`terminate-${pid}`);
				return true;
			},
			clearOwnership: () => events.push("clear"),
		});

		expect(stopped).toBe(true);
		expect(events).toEqual(["shutdown-request", "confirm", "terminate-42", "clear"]);
	});

	it("does not report success or clear ownership without an effective stop path", async () => {
		const clearOwnership = vi.fn();
		const stopped = await stopAdoptedDaemon({
			appOwned: true,
			alreadyStopped: false,
			supervisorConnected: false,
			pid: 42,
			requestShutdown: async () => false,
			confirmStopped: async () => false,
			terminateProcess: async () => false,
			clearOwnership,
		});

		expect(stopped).toBe(false);
		expect(clearOwnership).not.toHaveBeenCalled();
	});

	it("waits for a connected supervisor stop to complete before reporting success", async () => {
		const events: string[] = [];
		let confirmStop: (stopped: boolean) => void = () => undefined;
		const confirmation = new Promise<boolean>((resolve) => {
			confirmStop = resolve;
		});
		const terminateProcess = vi.fn(async () => true);
		let settled = false;
		const stopPromise = stopAdoptedDaemon({
			appOwned: true,
			alreadyStopped: false,
			supervisorConnected: true,
			pid: 42,
			requestShutdown: async () => false,
			confirmStopped: () => confirmation,
			terminateProcess,
			clearOwnership: () => events.push("clear"),
		}).then((result) => {
			settled = true;
			return result;
		});
		await Promise.resolve();
		await Promise.resolve();

		expect(events).toEqual(["clear"]);
		expect(settled).toBe(false);
		expect(terminateProcess).not.toHaveBeenCalled();

		confirmStop(true);
		await expect(stopPromise).resolves.toBe(true);
		expect(settled).toBe(true);
	});
});

describe("isCrashLikeDaemonExit", () => {
	it("leaves graceful external stops stopped", () => {
		expect(isCrashLikeDaemonExit(0, null)).toBe(false);
		expect(isCrashLikeDaemonExit(null, "SIGTERM")).toBe(false);
		expect(isCrashLikeDaemonExit(null, "SIGINT")).toBe(false);
	});

	it("restarts non-zero exits, fatal signals, and unknown exits", () => {
		expect(isCrashLikeDaemonExit(1, null)).toBe(true);
		expect(isCrashLikeDaemonExit(null, "SIGKILL")).toBe(true);
		expect(isCrashLikeDaemonExit(null, null)).toBe(true);
	});
});
