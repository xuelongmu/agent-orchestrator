import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { mkdtempSync, openSync, closeSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { createBacklogPoller, type BacklogServices } from "../backlog-poller.js";
import type { Issue, Session } from "../types.js";

function makeIssue(id: string): Issue {
  return {
    id,
    title: `Issue ${id}`,
    description: "",
    url: `https://github.com/test/test/issues/${id}`,
    state: "open",
    labels: ["agent:backlog"],
  } as Issue;
}

function makeWorkerSession(id: string, issueId: string, projectId = "proj"): Session {
  return {
    id,
    projectId,
    issueId,
    status: "working",
    metadata: {},
    lifecycle: { pr: { state: "none" } },
  } as unknown as Session;
}

interface Harness {
  services: BacklogServices;
  listIssues: ReturnType<typeof vi.fn>;
  updateIssue: ReturnType<typeof vi.fn>;
  spawn: ReturnType<typeof vi.fn>;
}

function makeHarness(opts: {
  backlogIssues: Issue[];
  sessions?: Session[];
  maxConcurrentAgents?: number;
}): Harness {
  const listIssues = vi.fn(async (filters: { labels?: string[] }) => {
    if (filters.labels?.includes("agent:backlog")) return opts.backlogIssues;
    return [];
  });
  const updateIssue = vi.fn(async () => undefined);
  const spawn = vi.fn(async () => ({ id: "spawned" }));

  const tracker = { name: "github", listIssues, updateIssue };

  const services: BacklogServices = {
    config: {
      projects: {
        proj: {
          name: "proj",
          path: "/tmp/proj",
          defaultBranch: "main",
          sessionPrefix: "ao",
          tracker: { plugin: "github" },
          ...(opts.maxConcurrentAgents !== undefined
            ? { maxConcurrentAgents: opts.maxConcurrentAgents }
            : {}),
        },
      },
    } as unknown as BacklogServices["config"],
    registry: {
      get: vi.fn((slot: string) => (slot === "tracker" ? tracker : null)),
    } as unknown as BacklogServices["registry"],
    sessionManager: {
      list: vi.fn(async () => opts.sessions ?? []),
      spawn,
    } as unknown as BacklogServices["sessionManager"],
  };

  return { services, listIssues, updateIssue, spawn };
}

describe("backlog poller", () => {
  let tmp: string;

  beforeEach(() => {
    tmp = mkdtempSync(join(tmpdir(), "ao-backlog-"));
  });

  afterEach(() => {
    rmSync(tmp, { recursive: true, force: true });
  });

  it("spawns up to the configured concurrency cap and transitions the label", async () => {
    const harness = makeHarness({
      backlogIssues: [makeIssue("1"), makeIssue("2"), makeIssue("3")],
      maxConcurrentAgents: 2,
    });
    const poller = createBacklogPoller({
      resolveServices: async () => harness.services,
      lockPath: null,
    });

    await poller.pollOnce();

    expect(harness.spawn).toHaveBeenCalledTimes(2);
    expect(harness.updateIssue).toHaveBeenCalledWith(
      "1",
      {
        labels: ["agent:in-progress"],
        removeLabels: ["agent:backlog"],
        comment: "Claimed by agent orchestrator — session spawned.",
      },
      expect.objectContaining({ tracker: { plugin: "github" } }),
    );
  });

  it("defaults the cap to 5 when maxConcurrentAgents is unset", async () => {
    const harness = makeHarness({
      backlogIssues: Array.from({ length: 7 }, (_, i) => makeIssue(String(i + 1))),
    });
    const poller = createBacklogPoller({
      resolveServices: async () => harness.services,
      lockPath: null,
    });

    await poller.pollOnce();

    expect(harness.spawn).toHaveBeenCalledTimes(5);
  });

  it("counts existing worker sessions against the per-project cap", async () => {
    const harness = makeHarness({
      backlogIssues: [makeIssue("10"), makeIssue("11")],
      sessions: [makeWorkerSession("ao-1", "9")],
      maxConcurrentAgents: 2,
    });
    const poller = createBacklogPoller({
      resolveServices: async () => harness.services,
      lockPath: null,
    });

    await poller.pollOnce();

    // Cap 2, one worker already running → only one slot left.
    expect(harness.spawn).toHaveBeenCalledTimes(1);
  });

  it("skips issues already claimed by a live worker session", async () => {
    const harness = makeHarness({
      backlogIssues: [makeIssue("5")],
      sessions: [makeWorkerSession("ao-1", "5")],
      maxConcurrentAgents: 5,
    });
    const poller = createBacklogPoller({
      resolveServices: async () => harness.services,
      lockPath: null,
    });

    await poller.pollOnce();

    expect(harness.spawn).not.toHaveBeenCalled();
  });

  it("skips the cycle when another process holds the lock", async () => {
    const lockPath = join(tmp, "backlog-poll.lock");
    const fd = openSync(lockPath, "wx"); // simulate another process holding a fresh lock
    try {
      const harness = makeHarness({ backlogIssues: [makeIssue("1")] });
      const poller = createBacklogPoller({
        resolveServices: async () => harness.services,
        lockPath,
      });

      await poller.pollOnce();

      expect(harness.spawn).not.toHaveBeenCalled();
    } finally {
      closeSync(fd);
      rmSync(lockPath, { force: true });
    }
  });

  it("acquires and releases the lock so consecutive cycles both run", async () => {
    const lockPath = join(tmp, "backlog-poll.lock");
    const harness = makeHarness({
      backlogIssues: [makeIssue("1")],
      maxConcurrentAgents: 5,
    });
    const poller = createBacklogPoller({
      resolveServices: async () => harness.services,
      lockPath,
    });

    await poller.pollOnce();
    await poller.pollOnce(); // lock released after the first cycle → this runs too

    expect(harness.spawn).toHaveBeenCalledTimes(2);
  });
});
