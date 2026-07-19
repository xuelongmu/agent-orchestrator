// @vitest-environment node
import { describe, expect, it, vi } from "vitest";
import { probeProcessLiveness, terminateProcessTree } from "./process-lifecycle";

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

describe("terminateProcessTree", () => {
	it("uses taskkill for a live Windows process tree", async () => {
		const signalProcess = vi.fn(() => true);
		const runTaskkill = vi.fn(async () => undefined);

		await expect(terminateProcessTree(42, { platform: "win32", signalProcess, runTaskkill })).resolves.toBe(true);
		expect(runTaskkill).toHaveBeenCalledWith(42);
		expect(signalProcess).toHaveBeenCalledWith(42, 0);
	});

	it("signals the process group on POSIX with a direct-PID fallback", async () => {
		const signalProcess = vi.fn((pid: number) => {
			if (pid < 0) throw errno("ESRCH");
			return true;
		});

		await expect(terminateProcessTree(42, { platform: "linux", signalProcess })).resolves.toBe(true);
		expect(signalProcess).toHaveBeenNthCalledWith(1, 42, 0);
		expect(signalProcess).toHaveBeenNthCalledWith(2, -42, "SIGTERM");
		expect(signalProcess).toHaveBeenNthCalledWith(3, 42, "SIGTERM");
	});
});
