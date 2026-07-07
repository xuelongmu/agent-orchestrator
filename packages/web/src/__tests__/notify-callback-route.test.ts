import { describe, it, expect, vi, beforeEach } from "vitest";
import {
  signCallbackToken,
  SessionNotFoundError,
  NOTIFY_CALLBACK_MESSAGES,
  type SessionManager,
  type OrchestratorConfig,
} from "@aoagents/ao-core";

const SECRET = "route-test-secret";

// Record activity events without touching the real SQLite layer.
const recordActivityEvent = vi.fn();
vi.mock("@aoagents/ao-core", async (importOriginal) => {
  const actual = (await importOriginal()) as Record<string, unknown>;
  return {
    ...actual,
    recordActivityEvent: (event: unknown) => recordActivityEvent(event),
  };
});

const mockConfig: OrchestratorConfig = {
  configPath: "/tmp/agent-orchestrator.yaml",
  port: 3000,
  readyThresholdMs: 300_000,
  defaults: { runtime: "tmux", agent: "claude-code", workspace: "worktree", notifiers: [] },
  projects: {},
  notifiers: {},
  notificationRouting: { urgent: [], action: [], warning: [], info: [] },
  reactions: {},
} as unknown as OrchestratorConfig;

const send = vi.fn(async () => {});
const kill = vi.fn(async () => {});
const mockSessionManager = { send, kill } as unknown as SessionManager;

vi.mock("@/lib/services", () => ({
  getServices: vi.fn(async () => ({
    config: mockConfig,
    sessionManager: mockSessionManager,
  })),
}));

import { GET } from "@/app/api/notify-callback/[token]/route";

function makeRequest(): Request {
  return new Request("http://localhost:3000/api/notify-callback/x");
}

function token(action: "approve" | "deny" | "nudge" | "kill", overrides = {}) {
  return signCallbackToken(
    { sessionId: "ao-5", projectId: "ao", action, exp: Date.now() + 60_000, ...overrides },
    SECRET,
  );
}

async function callGet(tok: string) {
  return GET(makeRequest(), { params: Promise.resolve({ token: tok }) });
}

beforeEach(() => {
  vi.clearAllMocks();
  process.env.AO_NOTIFY_CALLBACK_SECRET = SECRET;
});

describe("GET /api/notify-callback/[token]", () => {
  it("approve sends the approval message and records an audit event", async () => {
    const res = await callGet(token("approve"));
    expect(res.status).toBe(200);
    expect(send).toHaveBeenCalledWith("ao-5", NOTIFY_CALLBACK_MESSAGES.approve);
    expect(kill).not.toHaveBeenCalled();
    expect(recordActivityEvent).toHaveBeenCalledWith(
      expect.objectContaining({ kind: "api.notify_callback.approve", sessionId: "ao-5" }),
    );
    expect(res.headers.get("content-type")).toContain("text/html");
  });

  it("deny sends the denial message", async () => {
    await callGet(token("deny"));
    expect(send).toHaveBeenCalledWith("ao-5", NOTIFY_CALLBACK_MESSAGES.deny);
  });

  it("nudge sends the nudge message", async () => {
    await callGet(token("nudge"));
    expect(send).toHaveBeenCalledWith("ao-5", NOTIFY_CALLBACK_MESSAGES.nudge);
  });

  it("kill terminates the session instead of sending a message", async () => {
    const res = await callGet(token("kill"));
    expect(res.status).toBe(200);
    expect(kill).toHaveBeenCalledWith("ao-5");
    expect(send).not.toHaveBeenCalled();
  });

  it("rejects an invalid/tampered token with 403", async () => {
    const res = await callGet(`${token("approve")}tampered`);
    expect(res.status).toBe(403);
    expect(send).not.toHaveBeenCalled();
  });

  it("rejects an expired token with 403", async () => {
    const res = await callGet(token("approve", { exp: Date.now() - 1 }));
    expect(res.status).toBe(403);
  });

  it("returns 503 when no callback secret is configured", async () => {
    delete process.env.AO_NOTIFY_CALLBACK_SECRET;
    const res = await callGet(token("approve"));
    expect(res.status).toBe(503);
    expect(send).not.toHaveBeenCalled();
  });

  it("returns 404 when the session no longer exists", async () => {
    send.mockRejectedValueOnce(new SessionNotFoundError("ao-5"));
    const res = await callGet(token("approve"));
    expect(res.status).toBe(404);
    expect(recordActivityEvent).toHaveBeenCalledWith(
      expect.objectContaining({ kind: "api.notify_callback.failed", level: "error" }),
    );
  });

  it("returns 400 for a malformed session id in a validly-signed token", async () => {
    const res = await callGet(token("approve", { sessionId: "bad id!" }));
    expect(res.status).toBe(400);
    expect(send).not.toHaveBeenCalled();
  });
});
