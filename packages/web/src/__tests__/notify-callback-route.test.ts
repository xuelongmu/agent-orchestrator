import { describe, it, expect, vi, beforeEach } from "vitest";
import {
  signCallbackToken,
  SessionNotFoundError,
  NOTIFY_CALLBACK_MESSAGES,
  AGENT_REPORT_METADATA_KEYS,
  type Session,
  type SessionManager,
  type OrchestratorConfig,
} from "@aoagents/ao-core";

// Build session metadata carrying an agent report, whose timestamp is the
// decision-instance identity the callback nonce is checked against.
function reportMeta(atIso: string, state = "needs_input"): Record<string, string> {
  return {
    [AGENT_REPORT_METADATA_KEYS.STATE]: state,
    [AGENT_REPORT_METADATA_KEYS.AT]: atIso,
  };
}

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

function makeSession(overrides: Partial<Session> = {}): Session {
  // Minimal shape the route + isTerminalSession read. A waiting_input,
  // non-terminal session is the "decision still pending" default.
  return {
    id: "ao-5",
    projectId: "ao",
    status: "working",
    activity: "waiting_input",
    metadata: {},
    ...overrides,
  } as unknown as Session;
}

// isTerminalSession reads lifecycle.session/pr/runtime.state, so all three
// branches must exist. Defaults are non-terminal.
function makeLifecycle(sessionState: string): Session["lifecycle"] {
  return {
    session: { state: sessionState },
    pr: { state: "none" },
    runtime: { state: "alive" },
  } as unknown as Session["lifecycle"];
}

const send = vi.fn(async () => {});
const kill = vi.fn(async () => {});
const get = vi.fn(async () => makeSession());
const mockSessionManager = { send, kill, get } as unknown as SessionManager;

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
    get.mockResolvedValueOnce(null);
    const res = await callGet(token("approve"));
    expect(res.status).toBe(404);
    expect(send).not.toHaveBeenCalled();
  });

  it("returns 404 when send races a just-removed session", async () => {
    send.mockRejectedValueOnce(new SessionNotFoundError("ao-5"));
    const res = await callGet(token("approve"));
    expect(res.status).toBe(404);
    expect(recordActivityEvent).toHaveBeenCalledWith(
      expect.objectContaining({ kind: "api.notify_callback.failed", level: "error" }),
    );
  });

  it("rejects a stale action when the session is working again (409)", async () => {
    get.mockResolvedValueOnce(makeSession({ activity: "active" }));
    const res = await callGet(token("approve"));
    expect(res.status).toBe(409);
    expect(send).not.toHaveBeenCalled();
    expect(recordActivityEvent).toHaveBeenCalledWith(
      expect.objectContaining({ kind: "api.notify_callback.stale" }),
    );
  });

  it("rejects a stale kill when the session is already terminal (409)", async () => {
    get.mockResolvedValueOnce(makeSession({ activity: "exited" }));
    const res = await callGet(token("kill"));
    expect(res.status).toBe(409);
    expect(kill).not.toHaveBeenCalled();
  });

  it("rejects when the human already answered and the agent went back to idle (409)", async () => {
    // Codex scenario: link tapped later in its TTL after the decision resolved.
    get.mockResolvedValueOnce(makeSession({ activity: "idle" }));
    const res = await callGet(token("approve"));
    expect(res.status).toBe(409);
    expect(send).not.toHaveBeenCalled();
  });

  it("applies when the canonical state is needs_input even if activity is idle", async () => {
    get.mockResolvedValueOnce(makeSession({ activity: "idle", lifecycle: makeLifecycle("needs_input") }));
    const res = await callGet(token("approve"));
    expect(res.status).toBe(200);
    expect(send).toHaveBeenCalledWith("ao-5", NOTIFY_CALLBACK_MESSAGES.approve);
  });

  it("rejects when the decision nonce no longer matches the session (409)", async () => {
    // Token minted for the decision reported at A; session has since reported a
    // newer decision at B.
    get.mockResolvedValueOnce(makeSession({ metadata: reportMeta("2026-07-08T00:00:00.000Z") }));
    const res = await callGet(token("approve", { nonce: "2026-07-07T00:00:00.000Z" }));
    expect(res.status).toBe(409);
    expect(send).not.toHaveBeenCalled();
    expect(recordActivityEvent).toHaveBeenCalledWith(
      expect.objectContaining({ kind: "api.notify_callback.stale" }),
    );
  });

  it("applies when the decision nonce matches the reported decision instant", async () => {
    get.mockResolvedValueOnce(makeSession({ metadata: reportMeta("2026-07-07T00:00:00.000Z") }));
    const res = await callGet(token("approve", { nonce: "2026-07-07T00:00:00.000Z" }));
    expect(res.status).toBe(200);
    expect(send).toHaveBeenCalledWith("ao-5", NOTIFY_CALLBACK_MESSAGES.approve);
  });

  it("rejects a blocked (error/stuck) session — not a human prompt (409)", async () => {
    get.mockResolvedValueOnce(makeSession({ activity: "blocked" }));
    const res = await callGet(token("approve"));
    expect(res.status).toBe(409);
    expect(send).not.toHaveBeenCalled();
  });

  it("rejects a cross-project session id collision (404)", async () => {
    // Token signed for project "ao"; the unscoped lookup resolved a same-id
    // session in a different project.
    get.mockResolvedValueOnce(makeSession({ projectId: "other-project" }));
    const res = await callGet(token("approve"));
    expect(res.status).toBe(404);
    expect(send).not.toHaveBeenCalled();
  });

  it("returns 400 for a malformed session id in a validly-signed token", async () => {
    const res = await callGet(token("approve", { sessionId: "bad id!" }));
    expect(res.status).toBe(400);
    expect(send).not.toHaveBeenCalled();
  });
});
