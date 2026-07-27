// The daemon's own graceful-shutdown budget, mirrored on the supervisor side so
// Electron never signals an owned daemon that is still inside its deadline.
//
// config.DefaultShutdownTimeout (backend/internal/config/config.go) is the hard
// cap the daemon puts on its own drain; AO_SHUTDOWN_TIMEOUT overrides it with a
// Go duration. The daemon reads that variable out of the environment Electron
// hands it, so reading the same variable here keeps both sides on one number.
export const DEFAULT_DAEMON_SHUTDOWN_TIMEOUT_MS = 10_000;

// Slack on top of the daemon's own deadline: it force-exits when the deadline
// passes, so the signal is only for a daemon that failed to honor it at all.
export const SHUTDOWN_KILL_HEADROOM_MS = 5_000;

const DURATION_UNIT_MS: Record<string, number> = {
	ns: 1e-6,
	us: 1e-3,
	// Go accepts both micro-sign variants for microseconds.
	"µs": 1e-3,
	"μs": 1e-3,
	ms: 1,
	s: 1_000,
	m: 60_000,
	h: 3_600_000,
};

const DURATION_PATTERN = /^([0-9]*\.?[0-9]+)(ns|us|µs|μs|ms|s|m|h)/;

/**
 * Parse a Go `time.ParseDuration` string into milliseconds, mirroring the
 * daemon's parsePositiveDuration: anything invalid or non-positive is rejected
 * so the caller falls back to the same default the daemon would.
 */
export function parseGoDurationMs(raw: string | undefined): number | null {
	const text = raw?.trim();
	if (!text) return null;

	let rest = text;
	let sign = 1;
	if (rest.startsWith("+") || rest.startsWith("-")) {
		if (rest.startsWith("-")) sign = -1;
		rest = rest.slice(1);
	}
	// Go's only unitless duration.
	if (rest === "0") return null;
	if (rest === "") return null;

	let total = 0;
	while (rest !== "") {
		const match = DURATION_PATTERN.exec(rest);
		if (!match) return null;
		const [consumed, value, unit] = match;
		total += Number(value) * DURATION_UNIT_MS[unit];
		rest = rest.slice(consumed.length);
	}
	const ms = sign * total;
	return ms > 0 ? ms : null;
}

/**
 * How long an owned daemon that accepted POST /shutdown may take before the
 * supervisor falls back to a signal. Derived from the daemon's configured
 * deadline rather than a fixed constant: a signal inside that window is a hard
 * TerminateProcess on Windows, which would strand running.json — exactly the
 * cleanup the graceful route exists to perform.
 */
export function ownedShutdownGraceMs(env: Record<string, string | undefined>): number {
	const configured = parseGoDurationMs(env.AO_SHUTDOWN_TIMEOUT);
	return (configured ?? DEFAULT_DAEMON_SHUTDOWN_TIMEOUT_MS) + SHUTDOWN_KILL_HEADROOM_MS;
}
