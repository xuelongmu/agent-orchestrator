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

function makeMergedSession(id: string, issueId: string, projectId = "proj"): Session {
  return {
    id,
    projectId,
    issueId,
    status: "working",
    metadata: {},
    lifecycle: { pr: { state: "merged" } },
  } as unknown as Session;
}

interface Harness {
  services: BacklogServices;
  listIssues: ReturnType<typeof vi.fn>;
  updateIssue: ReturnType<typeof vi.fn>;
  getIssue: ReturnType<typeof vi.fn>;
  spawn: ReturnType<typeof vi.fn>;
}

function makeHarness(opts: {
  backlogIssues: Issue[];
  sessions?: Session[];
  maxConcurrentAgents?: number;
  /** Labels the tracker reports for any `getIssue` lookup (cross-process dedupe). */
  existingLabels?: string[];
}): Harness {
  const listIssues = vi.fn(async (filters: { labels?: string[] }) => {
    if (filters.labels?.includes("agent:backlog")) return opts.backlogIssues;
    return [];
  });
  const updateIssue = vi.fn(async () => undefined);
  const getIssue = vi.fn(async (id: string) => ({
    ...makeIssue(id),
    labels: opts.existingLabels ?? [],
  }));
  const spawn = vi.fn(async () => ({ id: "spawned" }));

  const tracker = { name: "github", listIssues, updateIssue, getIssue };

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

  return { services, listIssues, updateIssue, getIssue, spawn };
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

  it("requests up to availableSlots backlog issues so caps above ten are honored", async () => {
    const harness = makeHarness({
      backlogIssues: [],
      maxConcurrentAgents: 25,
    });
    const poller = createBacklogPoller({
      resolveServices: async () => harness.services,
      lockPath: null,
    });

    await poller.pollOnce();

    expect(harness.listIssues).toHaveBeenCalledWith(
      expect.objectContaining({ labels: ["agent:backlog"], limit: 25 }),
      expect.anything(),
    );
  });

  it("labels merged issues for verification when not yet labeled", async () => {
    const harness = makeHarness({
      backlogIssues: [],
      sessions: [makeMergedSession("ao-1", "42")],
      existingLabels: [],
    });
    const poller = createBacklogPoller({
      resolveServices: async () => harness.services,
      lockPath: null,
    });

    await poller.pollOnce();

    expect(harness.updateIssue).toHaveBeenCalledWith(
      "42",
      expect.objectContaining({ labels: ["merged-unverified"] }),
      expect.anything(),
    );
  });

  it("skips re-posting verification when the tracker already shows merged-unverified", async () => {
    // Simulates the peer process having already labeled the issue: our
    // in-memory dedupe set is empty but the tracker's state is authoritative.
    const harness = makeHarness({
      backlogIssues: [],
      sessions: [makeMergedSession("ao-1", "42")],
      existingLabels: ["merged-unverified"],
    });
    const poller = createBacklogPoller({
      resolveServices: async () => harness.services,
      lockPath: null,
    });

    await poller.pollOnce();

    expect(harness.getIssue).toHaveBeenCalledWith("42", expect.anything());
    expect(harness.updateIssue).not.toHaveBeenCalled();
  });

  it("stop() resolves only after an in-flight poll settles", async () => {
    const lockPath = join(tmp, "backlog-poll.lock");
    let releaseSpawn: () => void = () => {};
    const harness = makeHarness({
      backlogIssues: [makeIssue("1")],
      maxConcurrentAgents: 5,
    });
    let spawnSettled = false;
    (harness.services.sessionManager.spawn as ReturnType<typeof vi.fn>).mockImplementation(
      async () => {
        await new Promise<void>((resolve) => {
          releaseSpawn = () => {
            spawnSettled = true;
            resolve();
          };
        });
        return { id: "spawned" };
      },
    );

    const poller = createBacklogPoller({
      resolveServices: async () => harness.services,
      lockPath,
    });

    poller.start();
    // Let the immediate poll reach the blocked spawn.
    await new Promise((resolve) => setTimeout(resolve, 0));

    const stopPromise = poller.stop();
    let stopResolved = false;
    void stopPromise.then(() => {
      stopResolved = true;
    });

    // stop() must not resolve while the spawn is still in flight.
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(stopResolved).toBe(false);

    releaseSpawn();
    await stopPromise;
    expect(spawnSettled).toBe(true);
    expect(stopResolved).toBe(true);
  });

  it("keeps awaiting the original poll when interval ticks fire mid-poll", async () => {
    const lockPath = join(tmp, "backlog-poll.lock");
    let releaseSpawn: () => void = () => {};
    let spawnSettled = false;
    const harness = makeHarness({
      backlogIssues: [makeIssue("1")],
      maxConcurrentAgents: 5,
    });
    (harness.services.sessionManager.spawn as ReturnType<typeof vi.fn>).mockImplementation(
      async () => {
        await new Promise<void>((resolve) => {
          releaseSpawn = () => {
            spawnSettled = true;
            resolve();
          };
        });
        return { id: "spawned" };
      },
    );

    const poller = createBacklogPoller({
      resolveServices: async () => harness.services,
      lockPath,
      intervalMs: 5,
    });

    poller.start();
    // Let several interval ticks fire while the first poll is blocked on spawn.
    // The activePoll guard must keep them from replacing the original poll.
    await new Promise((resolve) => setTimeout(resolve, 30));

    const stopPromise = poller.stop();
    let stopResolved = false;
    void stopPromise.then(() => {
      stopResolved = true;
    });

    await new Promise((resolve) => setTimeout(resolve, 5));
    expect(stopResolved).toBe(false); // still awaiting the original blocked spawn

    releaseSpawn();
    await stopPromise;
    expect(spawnSettled).toBe(true);
    expect(stopResolved).toBe(true);
    // Only one poll ever acquired the lock and spawned.
    expect(harness.spawn).toHaveBeenCalledTimes(1);
  });

  it("does not spawn a duplicate worker for the same issue across projects sharing a tracker", async () => {
    const issue = makeIssue("42"); // identical url across both project entries
    const listIssues = vi.fn(async (filters: { labels?: string[] }) =>
      filters.labels?.includes("agent:backlog") ? [issue] : [],
    );
    // Claim update fails, so the issue keeps its agent:backlog label and the
    // second project would re-list it without cross-cycle dedup.
    const updateIssue = vi.fn(async () => {
      throw new Error("claim update failed");
    });
    const getIssue = vi.fn(async (id: string) => ({ ...makeIssue(id), labels: [] }));
    const spawn = vi.fn(async () => ({ id: "spawned" }));
    const tracker = { name: "github", listIssues, updateIssue, getIssue };

    const project = (name: string, prefix: string) => ({
      name,
      path: `/tmp/${name}`,
      defaultBranch: "main",
      sessionPrefix: prefix,
      tracker: { plugin: "github" },
    });
    const services: BacklogServices = {
      config: {
        projects: { projA: project("projA", "a"), projB: project("projB", "b") },
      } as unknown as BacklogServices["config"],
      registry: {
        get: vi.fn((slot: string) => (slot === "tracker" ? tracker : null)),
      } as unknown as BacklogServices["registry"],
      sessionManager: {
        list: vi.fn(async () => []),
        spawn,
      } as unknown as BacklogServices["sessionManager"],
    };

    const poller = createBacklogPoller({
      resolveServices: async () => services,
      lockPath: null,
    });

    await poller.pollOnce();

    expect(spawn).toHaveBeenCalledTimes(1);
  });
});
