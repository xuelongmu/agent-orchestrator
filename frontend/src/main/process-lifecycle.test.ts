// @vitest-environment node
import { describe, expect, it, vi } from "vitest";
import { probeProcessLiveness, terminateProcess } from "./process-lifecycle";

function errno(code: string): NodeJS.ErrnoException {
	return Object.assign(new Error(code), { code });
}

describe("probeProcessLiveness", () => {
	it("distinguishes verified-dead, permission-denied, and indeterminate probes", () => {
		expect(
			probeProcessLiveness(42, () => {
				throw errno("ESRCH");
			}),
		).toBe("dead");
		expect(
			probeProcessLiveness(42, () => {
				throw errno("EPERM");
			}),
		).toBe("alive");
		expect(
			probeProcessLiveness(42, () => {
				throw errno("EACCES");
			}),
		).toBe("unknown");
	});
});

describe("terminateProcess", () => {
	it("uses taskkill without the child-tree flag for a live Windows daemon", async () => {
		const signalProcess = vi.fn(() => true);
		const runCommand = vi.fn(async () => undefined);

		await expect(terminateProcess(42, { platform: "win32", signalProcess, runCommand })).resolves.toBe(true);
		expect(runCommand).toHaveBeenCalledWith("taskkill", ["/PID", "42", "/F"]);
		expect(runCommand.mock.calls[0]?.[1]).not.toContain("/T");
		expect(signalProcess).toHaveBeenCalledWith(42, 0);
	});

	it("signals only the daemon PID on POSIX", async () => {
		const signalProcess = vi.fn(() => true);

		await expect(terminateProcess(42, { platform: "linux", signalProcess })).resolves.toBe(true);
		expect(signalProcess).toHaveBeenNthCalledWith(1, 42, 0);
		expect(signalProcess).toHaveBeenNthCalledWith(2, 42, "SIGTERM");
		expect(signalProcess).not.toHaveBeenCalledWith(-42, "SIGTERM");
	});
});
