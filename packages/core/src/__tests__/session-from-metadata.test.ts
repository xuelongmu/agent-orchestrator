import { describe, it, expect } from "vitest";
import { sessionFromMetadata } from "../utils/session-from-metadata.js";
import { AGENT_REPORT_METADATA_KEYS } from "../agent-report.js";

describe("sessionFromMetadata — multi-PR (issue #1821)", () => {
  const baseOptions = { projectId: "my-app" };

  it("1.1 — single PR backwards compat: only pr field in metadata", () => {
    const session = sessionFromMetadata(
      "app-1",
      { pr: "https://github.com/acme/main/pull/10", branch: "feat/pr-10" },
      baseOptions,
    );
    expect(session.pr).not.toBeNull();
    expect(session.pr?.number).toBe(10);
    expect(session.prs).toHaveLength(1);
    expect(session.prs[0]).toBe(session.pr);
  });

  it("1.2 — multiple PRs from prs field: primary is first", () => {
    const session = sessionFromMetadata(
      "app-1",
      {
        prs: "https://github.com/acme/main/pull/10,https://github.com/acme/sub/pull/5",
        branch: "feat/pr-10",
      },
      baseOptions,
    );
    expect(session.prs).toHaveLength(2);
    expect(session.prs[0].number).toBe(10);
    expect(session.prs[1].number).toBe(5);
    expect(session.pr).toBe(session.prs[0]);
  });

  it("1.3 — secondary PR gets its own number, not the primary's", () => {
    const session = sessionFromMetadata(
      "app-1",
      {
        prs: "https://github.com/acme/main/pull/10,https://github.com/acme/sub/pull/42",
        branch: "feat/pr-10",
      },
      baseOptions,
    );
    expect(session.prs[0].number).toBe(10);
    expect(session.prs[1].number).toBe(42);
  });

  it("1.4 — isDraft applies only to primary PR, secondary is always false", () => {
    const session = sessionFromMetadata(
      "app-1",
      {
        prs: "https://github.com/acme/main/pull/10,https://github.com/acme/sub/pull/11",
        branch: "feat/pr-10",
        [AGENT_REPORT_METADATA_KEYS.PR_IS_DRAFT]: "true",
      },
      baseOptions,
    );
    expect(session.prs[0].isDraft).toBe(true);
    expect(session.prs[1].isDraft).toBe(false);
  });

  it("1.5 — empty prs field falls back to pr field", () => {
    const session = sessionFromMetadata(
      "app-1",
      {
        pr: "https://github.com/acme/main/pull/10",
        prs: "",
        branch: "feat/pr-10",
      },
      baseOptions,
    );
    expect(session.prs).toHaveLength(1);
    expect(session.prs[0].number).toBe(10);
  });

  it("1.6 — no PR fields → pr is null and prs is empty", () => {
    const session = sessionFromMetadata("app-1", {}, baseOptions);
    expect(session.pr).toBeNull();
    expect(session.prs).toHaveLength(0);
  });

  it("1.7 — prs field takes precedence over pr field when both are present", () => {
    const session = sessionFromMetadata(
      "app-1",
      {
        pr: "https://github.com/acme/main/pull/10",
        prs: "https://github.com/acme/main/pull/20,https://github.com/acme/main/pull/21",
        branch: "feat/pr-20",
      },
      baseOptions,
    );
    expect(session.prs).toHaveLength(2);
    expect(session.prs[0].number).toBe(20);
    expect(session.prs[1].number).toBe(21);
    expect(session.pr?.number).toBe(20);
  });

  it("1.8 — URLs with whitespace around commas are trimmed", () => {
    const session = sessionFromMetadata(
      "app-1",
      {
        prs: "https://github.com/acme/main/pull/10 , https://github.com/acme/main/pull/11",
        branch: "feat/pr-10",
      },
      baseOptions,
    );
    expect(session.prs).toHaveLength(2);
    expect(session.prs[0].number).toBe(10);
    expect(session.prs[1].number).toBe(11);
  });

  it("1.9 — owner and repo parsed correctly for each PR", () => {
    const session = sessionFromMetadata(
      "app-1",
      {
        prs: "https://github.com/org-a/repo-x/pull/10,https://github.com/org-b/repo-y/pull/99",
        branch: "feat/pr-10",
      },
      baseOptions,
    );
    expect(session.prs[0].owner).toBe("org-a");
    expect(session.prs[0].repo).toBe("repo-x");
    expect(session.prs[1].owner).toBe("org-b");
    expect(session.prs[1].repo).toBe("repo-y");
  });

  it("1.11 — dependsOn / blockedBy parsed from comma-separated metadata", () => {
    const session = sessionFromMetadata(
      "app-1",
      { dependsOn: "7, app-2", blockedBy: "7" },
      baseOptions,
    );
    expect(session.dependsOn).toEqual(["7", "app-2"]);
    expect(session.blockedBy).toEqual(["7"]);
  });

  it("1.12 — dependsOn / blockedBy omitted when absent or empty", () => {
    const session = sessionFromMetadata("app-1", { dependsOn: "", blockedBy: "" }, baseOptions);
    expect(session.dependsOn).toBeUndefined();
    expect(session.blockedBy).toBeUndefined();
  });

  it("1.12b — parentSessionId parsed from metadata (stacked PRs)", () => {
    const withParent = sessionFromMetadata("app-2", { parentSessionId: "app-1" }, baseOptions);
    expect(withParent.parentSessionId).toBe("app-1");

    const without = sessionFromMetadata("app-3", {}, baseOptions);
    expect(without.parentSessionId).toBeUndefined();
  });

  it("1.13 — blocked session keeps workspacePath null despite fallback", () => {
    const blockedLifecycle = {
      version: 2,
      session: {
        kind: "worker",
        state: "not_started",
        reason: "blocked_by_dependency",
        startedAt: null,
        completedAt: null,
        terminatedAt: null,
        lastTransitionAt: "2025-01-01T00:00:00.000Z",
      },
      pr: { state: "none", reason: "not_created", number: null, url: null, lastObservedAt: null },
      runtime: {
        state: "unknown",
        reason: "spawn_incomplete",
        lastObservedAt: null,
        handle: null,
        tmuxName: null,
      },
    };
    const session = sessionFromMetadata(
      "app-1",
      { lifecycle: JSON.stringify(blockedLifecycle), dependsOn: "7", blockedBy: "7" },
      { projectId: "my-app", workspacePathFallback: "/repo/path" },
    );
    expect(session.workspacePath).toBeNull();
    expect(session.runtimeHandle).toBeNull();
    expect(session.blockedBy).toEqual(["7"]);
  });

  it("1.14 — non-blocked session still uses workspacePathFallback", () => {
    const session = sessionFromMetadata(
      "app-1",
      { status: "working" },
      { projectId: "my-app", workspacePathFallback: "/repo/path" },
    );
    expect(session.workspacePath).toBe("/repo/path");
  });

  it("1.10 — duplicate prs entries are deduplicated by owner, repo, and number", () => {
    const session = sessionFromMetadata(
      "app-1",
      {
        prs: [
          "https://github.com/acme/main/pull/10",
          "https://github.com/acme/main/pull/10",
          "https://github.com/acme/sub/pull/10",
        ].join(","),
        branch: "feat/pr-10",
      },
      baseOptions,
    );
    expect(session.prs.map((pr) => pr.url)).toEqual([
      "https://github.com/acme/main/pull/10",
      "https://github.com/acme/sub/pull/10",
    ]);
    expect(session.pr).toBe(session.prs[0]);
  });
});
