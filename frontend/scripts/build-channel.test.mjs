// @vitest-environment node
import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";

import { buildChannel } from "./build-channel.mjs";

describe("buildChannel", () => {
	it("uses validated explicit release context", () => {
		expect(buildChannel("stable", "0.10.3")).toBe("stable");
		expect(buildChannel("testing", "0.10.3")).toBe("testing");
		expect(buildChannel("nightly", "0.10.3")).toBe("nightly");
	});

	it("does not classify a normal stable-version checkout as stable", () => {
		expect(buildChannel(undefined, "0.10.3")).toBe("development");
	});

	it("recognizes prerelease versions when no explicit context is present", () => {
		expect(buildChannel(undefined, "0.10.4-nightly.20260726")).toBe("nightly");
		expect(buildChannel(undefined, "0.0.0-testing-abcdef0")).toBe("testing");
	});

	it("rejects unknown explicit channels", () => {
		expect(() => buildChannel("preview", "0.10.3")).toThrow("unsupported AO_BUILD_CHANNEL");
	});

	it("receives explicit context from every packaging workflow", () => {
		const stable = readFileSync(new URL("../../.github/workflows/frontend-release.yml", import.meta.url), "utf8");
		const nightly = readFileSync(new URL("../../.github/workflows/frontend-nightly.yml", import.meta.url), "utf8");
		const testing = readFileSync(new URL("../../.github/workflows/testing-build.yml", import.meta.url), "utf8");
		const desktopTesting = readFileSync(
			new URL("../../.github/workflows/desktop-testing.yml", import.meta.url),
			"utf8",
		);
		expect(stable.match(/AO_BUILD_CHANNEL: stable/g)).toHaveLength(2);
		expect(nightly.match(/AO_BUILD_CHANNEL: nightly/g)).toHaveLength(2);
		expect(testing.match(/AO_BUILD_CHANNEL: testing/g)).toHaveLength(1);
		expect(desktopTesting.match(/AO_BUILD_CHANNEL: testing/g)).toHaveLength(1);
	});
});
