import { describe, expect, it } from "vitest";
import {
	DEFAULT_DAEMON_SHUTDOWN_TIMEOUT_MS,
	MAX_TIMER_DELAY_MS,
	SHUTDOWN_KILL_HEADROOM_MS,
	ownedShutdownGraceMs,
	parseGoDurationMs,
} from "./daemon-shutdown";

describe("parseGoDurationMs", () => {
	it("parses the duration forms the daemon config accepts", () => {
		expect(parseGoDurationMs("10s")).toBe(10_000);
		expect(parseGoDurationMs("500ms")).toBe(500);
		expect(parseGoDurationMs("1m30s")).toBe(90_000);
		expect(parseGoDurationMs("1.5s")).toBe(1_500);
		expect(parseGoDurationMs("2h")).toBe(7_200_000);
		expect(parseGoDurationMs("1500us")).toBe(1.5);
		expect(parseGoDurationMs("1500µs")).toBe(1.5);
		expect(parseGoDurationMs(" 30s ")).toBe(30_000);
	});

	// Verified against Go: "30.s" -> 30s and ".5s" -> 500ms both parse, because
	// the mantissa only needs a digit on one side of the point. ".s" does not.
	it("accepts Go's decimal forms with an empty side of the point", () => {
		expect(parseGoDurationMs("30.s")).toBe(30_000);
		expect(parseGoDurationMs(".5s")).toBe(500);
		expect(parseGoDurationMs("1m30.s")).toBe(90_000);
		expect(parseGoDurationMs(".s")).toBeNull();
		expect(parseGoDurationMs("30.")).toBeNull(); // missing unit, as in Go
	});

	it("rejects what parsePositiveDuration rejects, so the caller uses the daemon's default", () => {
		// The daemon requires > 0 and falls back to DefaultShutdownTimeout otherwise.
		expect(parseGoDurationMs("0")).toBeNull();
		expect(parseGoDurationMs("-5s")).toBeNull();
		expect(parseGoDurationMs("10")).toBeNull(); // unitless, not a Go duration
		expect(parseGoDurationMs("abc")).toBeNull();
		expect(parseGoDurationMs("10s junk")).toBeNull();
		expect(parseGoDurationMs("")).toBeNull();
		expect(parseGoDurationMs(undefined)).toBeNull();
	});
});

describe("ownedShutdownGraceMs", () => {
	it("outlasts the daemon's default shutdown deadline", () => {
		expect(ownedShutdownGraceMs({})).toBe(DEFAULT_DAEMON_SHUTDOWN_TIMEOUT_MS + SHUTDOWN_KILL_HEADROOM_MS);
		expect(ownedShutdownGraceMs({})).toBeGreaterThan(DEFAULT_DAEMON_SHUTDOWN_TIMEOUT_MS);
	});

	it("tracks an AO_SHUTDOWN_TIMEOUT override instead of a fixed timer", () => {
		// The regression this guards: a fixed 7s fallback signalled a daemon that
		// was still inside its own graceful deadline.
		expect(ownedShutdownGraceMs({ AO_SHUTDOWN_TIMEOUT: "30s" })).toBe(30_000 + SHUTDOWN_KILL_HEADROOM_MS);
		expect(ownedShutdownGraceMs({ AO_SHUTDOWN_TIMEOUT: "1m" })).toBe(60_000 + SHUTDOWN_KILL_HEADROOM_MS);
	});

	it("falls back to the default when the override is one the daemon would also reject", () => {
		expect(ownedShutdownGraceMs({ AO_SHUTDOWN_TIMEOUT: "nonsense" })).toBe(
			DEFAULT_DAEMON_SHUTDOWN_TIMEOUT_MS + SHUTDOWN_KILL_HEADROOM_MS,
		);
	});

	// Node stores the delay in a signed 32-bit int and silently reschedules an
	// overflowing one to 1ms, which would signal the daemon immediately — the
	// opposite of what this grace period is for.
	it("clamps a delay that would overflow Node's timer", () => {
		const grace = ownedShutdownGraceMs({ AO_SHUTDOWN_TIMEOUT: "600h" });
		expect(grace).toBe(MAX_TIMER_DELAY_MS);
		expect(grace).toBeLessThanOrEqual(2_147_483_647);
	});
});
