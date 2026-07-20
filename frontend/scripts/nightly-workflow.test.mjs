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
	const start = lines.findIndex((line) => line.includes('desktop_tags="$(git tag --list'));
	expect(start, "missing latest-stable lookup").toBeGreaterThan(-1);
	return lines
		.slice(start, start + 3)
		.map((line) => line.trim())
		.join("\n");
}

function exactStableTagPattern() {
	const match = latestStableLookup().match(/awk '\/(.+)\/ \{ print; exit \}'/);
	expect(match, "missing exact stable-tag filter").not.toBeNull();
	return new RegExp(match[1]);
}

function runLookup(tags = [], generatedStableTags = 0) {
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
GENERATED_STABLE_TAGS="$1"
shift
for tag in "$@"; do git tag "$tag"; done
if [ "$GENERATED_STABLE_TAGS" -gt 0 ]; then
	head="$(git rev-parse HEAD)"
	i=0
	while [ "$i" -lt "$GENERATED_STABLE_TAGS" ]; do
		printf 'create refs/tags/desktop-v9999.%s.0 %s\\n' "$i" "$head"
		i=$((i + 1))
	done | git update-ref --stdin
fi
${latestStableLookup()}
printf '%s' "$latest_stable"
`;

	return execFileSync("bash", ["-e", "-o", "pipefail", "-s", "--", String(generatedStableTags), ...tags], {
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
		expect(lookup).not.toContain("| head");
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

	it.skipIf(!shellIntegrationAvailable)("selects the first sorted stable tag without a downstream head stage", () => {
		expect(runLookup([], 5_000)).toBe("desktop-v9999.4999.0");
	});
});
