// @vitest-environment node
import { execFileSync, spawnSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";

const workflow = readFileSync(new URL("../../.github/workflows/frontend-nightly.yml", import.meta.url), "utf8");
const shellIntegrationAvailable =
	spawnSync("bash", ["--version"], { stdio: "ignore" }).status === 0 &&
	spawnSync("git", ["--version"], { stdio: "ignore" }).status === 0;

function latestStableLookup() {
	const lines = workflow.split(/\r?\n/);
	const start = lines.findIndex((line) => line.includes('latest_stable="$(git tag --list'));
	expect(start, "missing latest-stable lookup").toBeGreaterThan(-1);
	return lines
		.slice(start, start + 2)
		.map((line) => line.trim())
		.join("\n");
}

function exactStableTagPattern() {
	const match = latestStableLookup().match(/\|\s+awk '\/(.+)\/'\s+\|/);
	expect(match, "missing exact stable-tag filter").not.toBeNull();
	return new RegExp(match[1]);
}

function runLookup(tags = []) {
	const script = `
repository="$(mktemp -d)"
trap 'rm -rf "$repository"' EXIT
cd "$repository"
git init --quiet
git config user.email nightly-test@example.com
git config user.name "Nightly Test"
printf 'nightly workflow test\\n' > README.md
git add README.md
git commit --quiet -m "test fixture"
for tag in "$@"; do git tag "$tag"; done
${latestStableLookup()}
printf '%s' "$latest_stable"
`;

	return execFileSync("bash", ["-e", "-o", "pipefail", "-s", "--", ...tags], {
		encoding: "utf8",
		input: script,
	});
}

describe("desktop nightly stable-tag lookup", () => {
	it("portably restricts candidates to exact stable tags", () => {
		const lookup = latestStableLookup();
		const candidates = [
			"desktop-v1.9.0",
			"desktop-v1.10.0",
			"desktop-v99.0.0-nightly.202607201330",
			"desktop-v999.0.0oops",
		];

		expect(candidates.filter((tag) => exactStableTagPattern().test(tag))).toEqual([
			"desktop-v1.9.0",
			"desktop-v1.10.0",
		]);
		expect(lookup).toContain("--sort=-version:refname");
		expect(lookup).toContain('latest_stable="${latest_stable:-desktop-v0.0.0}"');
	});

	it.skipIf(!shellIntegrationAvailable)("uses the explicit zero-version fallback when no stable tag exists", () => {
		expect(runLookup()).toBe("desktop-v0.0.0");
	});

	it.skipIf(!shellIntegrationAvailable)("keeps ordering while rejecting prerelease and malformed tags", () => {
		expect(
			runLookup(["desktop-v1.9.0", "desktop-v1.10.0", "desktop-v99.0.0-nightly.202607201330", "desktop-v999.0.0oops"]),
		).toBe("desktop-v1.10.0");
	});
});
