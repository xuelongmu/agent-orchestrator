// @vitest-environment node
import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";

const workflow = readFileSync(new URL("../../.github/workflows/frontend-release.yml", import.meta.url), "utf8");
const forgeConfig = readFileSync(new URL("../forge.config.ts", import.meta.url), "utf8");

function job(name) {
	const marker = `  ${name}:\n`;
	const start = workflow.indexOf(marker);
	expect(start, `missing ${name} job`).toBeGreaterThan(-1);
	const rest = workflow.slice(start + marker.length);
	const next = rest.search(/^  [a-z][a-z0-9-]*:\n/m);
	return next === -1 ? rest : rest.slice(0, next);
}

describe("stable desktop release workflow", () => {
	it("checks eligibility without secrets or an environment", () => {
		const eligibility = job("release-eligibility");
		expect(eligibility).toContain("contents: read");
		expect(eligibility).toContain("ref: ${{ github.sha }}");
		expect(eligibility).not.toContain("environment:");
		expect(eligibility).not.toContain("secrets.");
		expect(eligibility).toContain("refs/heads/$DEFAULT_BRANCH");
		expect(eligibility).toContain("refs/tags/desktop-v$version");
		expect(eligibility).toContain("releases?per_page=100");
		expect(workflow).toContain("cancel-in-progress: false");
	});

	it("validates exactly six signing secrets behind release approval", () => {
		const secrets = job("release-secrets");
		const names = [...secrets.matchAll(/\$\{\{ secrets\.([A-Z0-9_]+) \}\}/g)].map((match) => match[1]);
		expect(secrets).toContain("environment: release");
		expect(names).toHaveLength(6);
		expect(new Set(names)).toEqual(
			new Set([
				"CSC_LINK",
				"CSC_KEY_PASSWORD",
				"APPLE_API_KEY_BASE64",
				"APPLE_API_KEY_ID",
				"APPLE_API_ISSUER",
				"APPLE_SIGNING_IDENTITY",
			]),
		);
	});

	it("stages every platform as a draft after eligibility and secret validation", () => {
		for (const name of ["release", "release-intel"]) {
			const publisher = job(name);
			expect(publisher).toContain("needs: [release-eligibility, release-secrets]");
			expect(publisher).toContain("environment: release");
			expect(publisher).toContain('AO_RELEASE_DRAFT: "true"');
		}
		expect(forgeConfig).toContain('draft: process.env.AO_RELEASE_DRAFT === "true"');
		expect(workflow.match(/AO_RELEASE_DRAFT: "true"/g)).toHaveLength(2);
	});

	it("publishes only after every platform and feed upload succeeds", () => {
		const feed = job("publish-feed");
		expect(feed).toContain("needs: [release-eligibility, release-secrets, release, release-intel]");
		expect(feed).toContain("environment: release");
		for (const name of ["latest.yml", "latest-mac.yml", "latest-linux.yml"]) {
			expect(feed).toContain(`dist/${name}`);
		}
		expect(feed.indexOf('gh release upload "$tag"')).toBeLessThan(
			feed.indexOf('gh release edit "$tag" --draft=false --prerelease=false --latest'),
		);
	});
});
