import assert from "node:assert/strict";
import test from "node:test";

import { repositoryIdentity } from "./repository-identity.mjs";

test("repositoryIdentity strips credentials from URL remotes", () => {
	assert.equal(repositoryIdentity("https://user:token@gitlab.com/org/repo.git"), "gitlab.com/org/repo");
});

test("repositoryIdentity normalizes SCP-style remotes", () => {
	assert.equal(repositoryIdentity("git@gitlab.com:org/repo.git"), "gitlab.com/org/repo");
});

test("repositoryIdentity omits unsafe or local remotes", () => {
	assert.equal(repositoryIdentity("C:\\repos\\private"), "");
	assert.equal(repositoryIdentity("../private"), "");
});
