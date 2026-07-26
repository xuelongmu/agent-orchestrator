// @vitest-environment node
import { expect, test } from "vitest";

import { repositoryIdentity } from "./repository-identity.mjs";

test("repositoryIdentity strips credentials from URL remotes", () => {
	expect(repositoryIdentity("https://user:token@gitlab.com/org/repo.git")).toBe("gitlab.com/org/repo");
});

test("repositoryIdentity normalizes SCP-style remotes", () => {
	expect(repositoryIdentity("git@gitlab.com:org/repo.git")).toBe("gitlab.com/org/repo");
	expect(repositoryIdentity("git@github.com:org/repo.git")).toBe("org/repo");
});

test("repositoryIdentity omits unsafe or local remotes", () => {
	expect(repositoryIdentity("C:\\repos\\private")).toBe("");
	expect(repositoryIdentity("../private")).toBe("");
});
