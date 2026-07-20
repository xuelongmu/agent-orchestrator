// @vitest-environment node
import { execFileSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";

const workflow = readFileSync(new URL("../../.github/workflows/frontend-nightly.yml", import.meta.url), "utf8");

function latestStableLookup() {
	const lines = workflow.split(/\r?\n/);
	const start = lines.findIndex((line) => line.includes('latest_stable="$(git tag --list'));
	expect(start, "missing latest-stable lookup").toBeGreaterThan(-1);
	return lines
		.slice(start, start + 2)
		.map((line) => line.trim())
		.join("\n");
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
	it("uses the explicit zero-version fallback when no stable tag exists", () => {
		expect(runLookup()).toBe("desktop-v0.0.0");
	});

	it("keeps version ordering when stable tags exist", () => {
		expect(runLookup(["desktop-v1.9.0", "desktop-v1.10.0"])).toBe("desktop-v1.10.0");
	});
});
