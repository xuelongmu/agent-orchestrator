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
  Workspace,
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
let mockWorkspace: Workspace;
let mockRegistry: PluginRegistry;
let config: OrchestratorConfig;

beforeEach(() => {
  ctx = setupTestContext();
  ({ sessionsDir, mockRuntime, mockAgent, mockWorkspace, mockRegistry, config } = ctx);
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
});
