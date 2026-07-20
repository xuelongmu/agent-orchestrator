import path from "node:path";
import { describe, expect, it, vi } from "vitest";
import { evaluateDaemonIdentity, type DaemonLaunchSpec } from "./daemon-launch";
import { pathIdentityKey, pathInside, samePath, type PathIdentityOptions } from "./path-identity";

function fakeRealpath(platform: NodeJS.Platform, existingPaths: string[], caseInsensitive = false) {
	const paths = platform === "win32" ? path.win32 : path.posix;
	const entries = new Map(
		existingPaths.map((entry) => {
			const resolved = paths.resolve(entry);
			return [caseInsensitive ? resolved.toLowerCase() : resolved, resolved];
		}),
	);
	return vi.fn((value: string) => {
		const lookup = caseInsensitive ? value.toLowerCase() : value;
		const canonical = entries.get(lookup);
		if (canonical) return canonical;
		throw Object.assign(new Error(`ENOENT: ${value}`), { code: "ENOENT" });
	});
}

function options(platform: NodeJS.Platform, existingPaths: string[], caseInsensitive = false): PathIdentityOptions {
	return { platform, realpath: fakeRealpath(platform, existingPaths, caseInsensitive) };
}

describe("path identity", () => {
	it("uses filesystem-canonical casing for equivalent macOS paths", () => {
		const canonical = "/Users/example/Documents/projects/agent-orchestrator/backend";
		const identity = options("darwin", [canonical], true);

		expect(samePath(canonical, canonical.toLowerCase(), identity)).toBe(true);
		expect(pathIdentityKey(canonical.toLowerCase(), identity)).toBe(canonical);
	});

	it("does not confuse genuinely different checkouts", () => {
		const first = "/Users/example/Documents/projects/agent-orchestrator/backend";
		const second = "/Users/example/Documents/archive/agent-orchestrator/backend";
		const identity = options("darwin", [first, second], true);

		expect(samePath(first, second, identity)).toBe(false);
	});

	it("recognizes children but not sibling path prefixes", () => {
		const checkout = "/Users/example/Documents/projects/agent-orchestrator/backend";
		const executable = `${checkout}/bin/ao`;
		const sibling = `${checkout}-old/bin/ao`;
		const identity = options("darwin", [checkout, executable, sibling], true);

		expect(pathInside(executable, checkout, identity)).toBe(true);
		expect(pathInside(checkout, checkout, identity)).toBe(true);
		expect(pathInside(sibling, checkout, identity)).toBe(false);
	});

	it("canonicalizes the nearest existing ancestor of a missing child", () => {
		const checkout = "/Users/example/Documents/projects/agent-orchestrator/backend";
		const realpath = fakeRealpath("darwin", [checkout], true);
		const identity: PathIdentityOptions = { platform: "darwin", realpath };

		expect(
			samePath(
				`${checkout}/generated/bin/ao`,
				"/Users/example/documents/projects/agent-orchestrator/backend/generated/bin/ao",
				identity,
			),
		).toBe(true);
		expect(pathIdentityKey(`${checkout}/generated/bin/ao`, identity)).toBe(`${checkout}/generated/bin/ao`);
		expect(realpath).toHaveBeenCalledWith(`${checkout}/generated/bin/ao`);
		expect(realpath).toHaveBeenCalledWith(checkout);
	});

	it("keeps Windows comparisons case-insensitive", () => {
		const identity = options("win32", ["C:\\Users\\Example\\AO\\backend"], true);

		expect(samePath("C:\\Users\\Example\\AO\\backend", "c:\\users\\example\\ao\\BACKEND", identity)).toBe(true);
	});

	it("keeps platform behavior injectable instead of depending on the test host", () => {
		const missing = vi.fn((_value: string) => {
			throw Object.assign(new Error("ENOENT"), { code: "ENOENT" });
		});

		expect(samePath("/repo/AO", "/repo/ao", { platform: "darwin", realpath: missing })).toBe(false);
		expect(samePath("C:\\repo\\AO", "c:\\REPO\\ao", { platform: "win32", realpath: missing })).toBe(true);
	});
});

describe("daemon identity path boundaries", () => {
	it("retains bundled executable identity checks", () => {
		const executable = "/Applications/Agent Orchestrator.app/Contents/Resources/daemon/ao";
		const launch: DaemonLaunchSpec = {
			command: executable,
			args: ["daemon"],
			cwd: "/Applications/Agent Orchestrator.app/Contents/Resources",
			shell: false,
			source: "bundled",
		};
		const identity = options("darwin", [executable], true);

		expect(
			evaluateDaemonIdentity(
				launch,
				{ status: "ready", service: "ao-daemon", pid: 42, executablePath: executable.toLowerCase() },
				{
					enforceDevCheckout: false,
					samePath: (a, b) => samePath(a, b, identity),
					pathInside: (child, parent) => pathInside(child, parent, identity),
				},
			),
		).toBeNull();
	});

	it("retains development executable containment checks", () => {
		const checkout = "/Users/example/Documents/projects/agent-orchestrator/backend";
		const executable = `${checkout}/tmp/ao`;
		const launch: DaemonLaunchSpec = {
			command: "go",
			args: ["run", "./cmd/ao", "daemon"],
			cwd: checkout,
			shell: false,
			source: "dev",
		};
		const identity = options("darwin", [checkout, executable], true);

		expect(
			evaluateDaemonIdentity(
				launch,
				{ status: "ready", service: "ao-daemon", pid: 42, executablePath: executable.toLowerCase() },
				{
					enforceDevCheckout: true,
					samePath: (a, b) => samePath(a, b, identity),
					pathInside: (child, parent) => pathInside(child, parent, identity),
				},
			),
		).toBeNull();
	});
});
