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

function guardCheck() {
	const lines = workflow.split(/\r?\n/);
	const start = lines.findIndex((line) => line.includes('nightly_tags="$(git tag --list'));
	expect(start, "missing nightly guard lookup").toBeGreaterThan(-1);
	const end = lines.findIndex((line, index) => index > start && line.trim() === "fi");
	expect(end, "missing nightly guard condition").toBeGreaterThan(start);
	return lines
		.slice(start, end + 1)
		.map((line) => line.trim())
		.join("\n");
}

function exactStableTagPattern() {
	const match = latestStableLookup().match(/awk '\/(.+)\/ \{ print; exit \}'/);
	expect(match, "missing exact stable-tag filter").not.toBeNull();
	return new RegExp(match[1]);
}

function runInRepository(body, args = []) {
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
${body}
`;

	return execFileSync("bash", ["-e", "-o", "pipefail", "-s", "--", ...args], {
		encoding: "utf8",
		input: script,
	});
}

function runLookup(tags = [], generatedStableTags = 0) {
	return runInRepository(
		`
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
`,
		[String(generatedStableTags), ...tags],
	);
}

function runGuard(generatedNightlyTags = 0) {
	return runInRepository(
		`
GENERATED_NIGHTLY_TAGS="$1"
if [ "$GENERATED_NIGHTLY_TAGS" -gt 0 ]; then
	head="$(git rev-parse HEAD)"
	i=0
	while [ "$i" -lt "$GENERATED_NIGHTLY_TAGS" ]; do
		printf 'create refs/tags/v9999.%s.0-nightly.202607201330 %s\\n' "$i" "$head"
		i=$((i + 1))
	done | git update-ref --stdin
fi
GITHUB_OUTPUT="$repository/github-output"
export GITHUB_OUTPUT
${guardCheck()}
cat "$GITHUB_OUTPUT"
`,
		[String(generatedNightlyTags)],
	);
}

describe("desktop nightly workflow tag lookups", () => {
	it("portably restricts candidates to exact stable tags", () => {
		const lookup = latestStableLookup();
		const candidates = [
			"desktop-v1.9.0",
			"desktop-v1.10.0",
			"desktop-v99.0.0-nightly.202607201330",
			"desktop-v999.0.0oops",
			"desktop-v01.2.3",
			"desktop-v1.02.3",
			"desktop-v1.2.03",
			"desktop-v999.00.0",
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
			runLookup([
				"desktop-v1.9.0",
				"desktop-v1.10.0",
				"desktop-v99.0.0-nightly.202607201330",
				"desktop-v999.0.0oops",
				"desktop-v999.00.0",
			]),
		).toBe("desktop-v1.10.0");
	});

	it.skipIf(!shellIntegrationAvailable)("selects the first sorted stable tag without a downstream head stage", () => {
		expect(runLookup([], 5_000)).toBe("desktop-v9999.4999.0");
	});

	it("portably keeps the nightly guard free of a downstream head stage", () => {
		const check = guardCheck();
		expect(check).toContain("--sort=-creatordate");
		expect(check).not.toContain("| head");
	});

	it.skipIf(!shellIntegrationAvailable)("emits should_build=true when no nightly tag exists", () => {
		expect(runGuard()).toBe("should_build=true\n");
	});

	it.skipIf(!shellIntegrationAvailable)("handles many nightly tags and emits should_build=false for HEAD", () => {
		expect(runGuard(5_000)).toBe("should_build=false\n");
	});
});
