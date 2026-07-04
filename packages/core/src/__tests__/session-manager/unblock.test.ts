import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
import { createSessionManager } from "../../session-manager.js";
import {
  createInitialCanonicalLifecycle,
  deriveLegacyStatus,
} from "../../lifecycle-state.js";
import { writeMetadata, readMetadataRaw } from "../../metadata.js";
import type {
  OrchestratorConfig,
  PluginRegistry,
  Runtime,
  Agent,
  SessionMetadata,
} from "../../types.js";
import {
  setupTestContext,
  teardownTestContext,
  type TestContext,
} from "../test-utils.js";

let ctx: TestContext;
let sessionsDir: string;
let mockRuntime: Runtime;
let mockAgent: Agent;
let mockRegistry: PluginRegistry;
let config: OrchestratorConfig;

beforeEach(() => {
  ctx = setupTestContext();
  ({ sessionsDir, mockRuntime, mockAgent, mockRegistry, config } = ctx);
});

afterEach(() => {
  teardownTestContext(ctx);
});

/**
 * Persist a held (blocked-by-dependency) session whose prerequisites have all
 * resolved (empty `blockedBy`) but whose launch was deferred — it is held only
 * by its lifecycle pre-state. This is the resolved-but-deferred shape the
 * scheduler produces under a full `maxConcurrent`.
 */
function writeHeldSession(sessionId: string): void {
  const createdAt = new Date("2024-01-01T00:00:00.000Z");
  const lifecycle = createInitialCanonicalLifecycle("worker", createdAt);
  lifecycle.session.reason = "blocked_by_dependency";
  writeMetadata(sessionsDir, sessionId, {
    worktree: "",
    branch: `feat/${sessionId}`,
    status: deriveLegacyStatus(lifecycle),
    lifecycle,
    project: "my-app",
    agent: "mock-agent",
    createdAt: createdAt.toISOString(),
    dependsOn: ["dep-a", "dep-b"],
    blockedBy: [],
    userPrompt: "do the thing",
    displayName: "My Custom Title",
    displayNameUserSet: "true",
  } as unknown as SessionMetadata);
}

describe("unblock", () => {
  it("preserves the user-set title and original dependency graph when launching", async () => {
    writeHeldSession("app-1");
    const sm = createSessionManager({ config, registry: mockRegistry });

    await sm.unblock("app-1");

    expect(mockRuntime.create).toHaveBeenCalled();
    const raw = readMetadataRaw(sessionsDir, "app-1");
    expect(raw).not.toBeNull();
    // Finding 5: the full launch writeMetadata must not clobber the user's title
    // or the recorded dependency history.
    expect(raw?.["displayName"]).toBe("My Custom Title");
    expect(raw?.["displayNameUserSet"]).toBe("true");
    expect(raw?.["dependsOn"]).toContain("dep-a");
    expect(raw?.["dependsOn"]).toContain("dep-b");
    // The session is now launched, no longer held.
    expect(raw?.["blockedBy"]).toBeFalsy();
  });

  it("restores the held record when a post-launch step fails (rollback)", async () => {
    writeHeldSession("app-1");
    // Force a failure AFTER the launch's writeMetadata has overwritten the held
    // record with a launched lifecycle (Finding 4).
    mockAgent.postLaunchSetup = vi.fn().mockRejectedValue(new Error("post-launch boom"));
    const sm = createSessionManager({ config, registry: mockRegistry });

    await expect(sm.unblock("app-1")).rejects.toThrow("post-launch boom");

    const raw = readMetadataRaw(sessionsDir, "app-1");
    expect(raw).not.toBeNull();
    // The held record is restored verbatim so the scheduler can retry.
    const lifecycle = JSON.parse(raw?.["lifecycle"] ?? "{}");
    expect(lifecycle.session?.reason).toBe("blocked_by_dependency");
    expect(raw?.["displayName"]).toBe("My Custom Title");
    expect(raw?.["displayNameUserSet"]).toBe("true");
    // Launch pointers from the torn-down attempt are gone.
    expect(raw?.["runtimeHandle"]).toBeUndefined();
    expect(raw?.["worktree"]).toBeFalsy();
    // The torn-down runtime was destroyed during rollback.
    expect(mockRuntime.destroy).toHaveBeenCalled();
  });

  it("is a no-op when the session is no longer held", async () => {
    // A launched (not-held) session: unblock returns it unchanged.
    const createdAt = new Date("2024-01-01T00:00:00.000Z");
    const lifecycle = createInitialCanonicalLifecycle("worker", createdAt);
    writeMetadata(sessionsDir, "app-1", {
      worktree: "/tmp/ws",
      branch: "feat/app-1",
      status: deriveLegacyStatus(lifecycle),
      lifecycle,
      project: "my-app",
      agent: "mock-agent",
      createdAt: createdAt.toISOString(),
    } as unknown as SessionMetadata);
    const sm = createSessionManager({ config, registry: mockRegistry });

    await sm.unblock("app-1");

    expect(mockRuntime.create).not.toHaveBeenCalled();
  });

  /** Held stacked child whose persisted baseRef points at its parent's branch. */
  function writeHeldStackedChild(sessionId: string): void {
    const createdAt = new Date("2024-01-01T00:00:00.000Z");
    const lifecycle = createInitialCanonicalLifecycle("worker", createdAt);
    lifecycle.session.reason = "blocked_by_dependency";
    writeMetadata(sessionsDir, sessionId, {
      worktree: "",
      branch: `feat/${sessionId}`,
      status: deriveLegacyStatus(lifecycle),
      lifecycle,
      project: "my-app",
      agent: "mock-agent",
      createdAt: createdAt.toISOString(),
      dependsOn: ["app-1"],
      blockedBy: [],
      parentSessionId: "app-1",
      baseRef: "feat/parent",
    } as unknown as SessionMetadata);
  }

  it("re-resolves a held stacked child off the default base when its parent has merged (#11)", async () => {
    // Parent app-1 is absent (merged + auto-cleaned). The persisted baseRef must
    // NOT be replayed — its branch is gone; the child branches off the default.
    writeHeldStackedChild("app-2");
    const sm = createSessionManager({ config, registry: mockRegistry });

    await sm.unblock("app-2");

    expect(ctx.mockWorkspace.create).toHaveBeenCalledTimes(1);
    const createCfg = vi.mocked(ctx.mockWorkspace.create).mock.calls[0]![0];
    expect(createCfg.baseRef).toBeUndefined();
    // The stale baseRef is dropped from metadata too.
    expect(readMetadataRaw(sessionsDir, "app-2")?.["baseRef"]).toBeFalsy();
  });

  it("re-resolves a held stacked child onto the parent's current branch when it is still open (#11)", async () => {
    // Parent app-1 still open with its branch present.
    const parentCreatedAt = new Date("2024-01-01T00:00:00.000Z");
    const parentLifecycle = createInitialCanonicalLifecycle("worker", parentCreatedAt);
    writeMetadata(sessionsDir, "app-1", {
      worktree: "/tmp/ws-1",
      branch: "feat/parent",
      status: deriveLegacyStatus(parentLifecycle),
      lifecycle: parentLifecycle,
      project: "my-app",
      agent: "mock-agent",
      createdAt: parentCreatedAt.toISOString(),
    } as unknown as SessionMetadata);
    writeHeldStackedChild("app-2");
    const sm = createSessionManager({ config, registry: mockRegistry });

    await sm.unblock("app-2");

    const createCfg = vi.mocked(ctx.mockWorkspace.create).mock.calls[0]![0];
    expect(createCfg.baseRef).toBe("feat/parent");
    expect(readMetadataRaw(sessionsDir, "app-2")?.["baseRef"]).toBe("feat/parent");
  });

  it("re-resolves a held stacked child onto the parent's merged-into base for a middle stack (#11)", async () => {
    // Parent app-1 has merged; it was itself stacked on feat/grandparent. The
    // child must branch off the base the parent merged INTO, not the merged
    // parent branch nor the project default.
    const createdAt = new Date("2024-01-01T00:00:00.000Z");
    const mergedLifecycle = createInitialCanonicalLifecycle("worker", createdAt);
    mergedLifecycle.pr.state = "merged";
    mergedLifecycle.pr.reason = "merged";
    writeMetadata(sessionsDir, "app-1", {
      worktree: "/tmp/ws-1",
      branch: "feat/parent",
      status: deriveLegacyStatus(mergedLifecycle),
      lifecycle: mergedLifecycle,
      project: "my-app",
      agent: "mock-agent",
      createdAt: createdAt.toISOString(),
      baseRef: "feat/grandparent",
    } as unknown as SessionMetadata);
    writeHeldStackedChild("app-2");
    const sm = createSessionManager({ config, registry: mockRegistry });

    await sm.unblock("app-2");

    const createCfg = vi.mocked(ctx.mockWorkspace.create).mock.calls[0]![0];
    expect(createCfg.baseRef).toBe("feat/grandparent");
    expect(readMetadataRaw(sessionsDir, "app-2")?.["baseRef"]).toBe("feat/grandparent");
  });
});
