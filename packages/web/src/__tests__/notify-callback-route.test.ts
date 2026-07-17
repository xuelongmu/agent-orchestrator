import { describe, it, expect, vi, beforeEach } from "vitest";
import {
  signCallbackToken,
  SessionNotFoundError,
  SessionSendNotDeliveredError,
  NOTIFY_CALLBACK_MESSAGES,
  AGENT_REPORT_METADATA_KEYS,
  NOTIFY_DECISION_METADATA_KEYS,
  type Session,
  type SessionManager,
  type OrchestratorConfig,
} from "@aoagents/ao-core";

// When the session entered its current needs_input episode. The decision
// identity pairs this with the report instant, so a token minted in one episode
// cannot answer a prompt raised in the next.
const EPISODE_AT = "2026-07-17T09:00:00.000Z";

// Build session metadata carrying an agent report plus its episode marker —
// together these form the decision identity the callback nonce is checked against.
function reportMeta(atIso: string, state = "needs_input"): Record<string, string> {
  return {
    [AGENT_REPORT_METADATA_KEYS.STATE]: state,
    [AGENT_REPORT_METADATA_KEYS.AT]: atIso,
    [NOTIFY_DECISION_METADATA_KEYS.EPISODE_AT]: EPISODE_AT,
  };
}

/** The decision identity for a report made at `atIso` in the current episode. */
const identityFor = (atIso: string) => `${atIso}:${EPISODE_AT}`;

const SECRET = "route-test-secret";
// Default decision instant: a report-backed needs_input at this timestamp, with
// the token's nonce bound to it, is the "decision still pending & matching" case.
// Kept FRESH: a decision identity exists only while the decision is an active
// block, so a fixed past timestamp would read as an already-spent decision.
const DECISION_AT = new Date().toISOString();

// Record activity events without touching the real SQLite layer.
const recordActivityEvent = vi.fn();

// Stand in for the durable, metadata-backed decision store with an equivalent
// in-memory one, so the route's consume/release wiring is exercised without
// touching a real project's session metadata on disk. The store deliberately
// OUTLIVES each request — that is what makes the double-tap assertions meaningful.
const consumedDecisions = new Set<string>();
const consumeKey = (projectId: string, sessionId: string, decisionId: string) =>
  `${projectId}:${sessionId}:${decisionId}`;
vi.mock("@aoagents/ao-core", async (importOriginal) => {
  const actual = (await importOriginal()) as Record<string, unknown>;
  return {
    ...actual,
    recordActivityEvent: (event: unknown) => recordActivityEvent(event),
    consumeDecision: (projectId: string, sessionId: string, decisionId: string) => {
      const key = consumeKey(projectId, sessionId, decisionId);
      if (consumedDecisions.has(key)) return false;
      consumedDecisions.add(key);
      return true;
    },
    // Reopen a consumed claim — the route calls this only for a proven pre-delivery
    // dispatch failure.
    releaseDecision: (projectId: string, sessionId: string, decisionId: string) => {
      consumedDecisions.delete(consumeKey(projectId, sessionId, decisionId));
    },
    // Route wiring only: block a nudge once the decision is consumed. The
    // stored-identity-superseded branch is exercised against real metadata in
    // notify-decision.test.ts.
    isNudgeBlocked: (projectId: string, sessionId: string, decisionId: string) =>
      consumedDecisions.has(consumeKey(projectId, sessionId, decisionId)),
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
    metadata: reportMeta(DECISION_AT),
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
// Mirrors the real project-scoped lookup: a session is only visible to the
// project that owns it, so a token for project B cannot resolve project A's
// same-id session.
const get = vi.fn(async (sessionId: string, projectId?: string) =>
  projectId !== undefined && projectId !== "ao" ? null : makeSession(),
);
const mockSessionManager = { send, kill, get } as unknown as SessionManager;

vi.mock("@/lib/services", () => ({
  getServices: vi.fn(async () => ({
    config: mockConfig,
    sessionManager: mockSessionManager,
  })),
}));

import { GET, POST } from "@/app/api/notify-callback/[token]/route";

function makeRequest(): Request {
  return new Request("http://localhost:3000/api/notify-callback/x");
}

/** GET renders the confirmation page; only POST mutates. */
async function callRawGet(tok: string) {
  return GET(makeRequest(), { params: Promise.resolve({ token: tok }) });
}

function token(action: "approve" | "deny" | "nudge" | "kill", overrides = {}) {
  return signCallbackToken(
    {
      sessionId: "ao-5",
      projectId: "ao",
      action,
      exp: Date.now() + 60_000,
      nonce: identityFor(DECISION_AT),
      ...overrides,
    },
    SECRET,
  );
}

// Every dispatch assertion goes through POST — the mutation path.
async function callGet(tok: string) {
  return POST(makeRequest(), { params: Promise.resolve({ token: tok }) });
}

beforeEach(() => {
  vi.clearAllMocks();
  consumedDecisions.clear();
  process.env.AO_NOTIFY_CALLBACK_SECRET = SECRET;
});

describe("GET /api/notify-callback/[token] — confirmation page only", () => {
  // A signed URL proves AO minted it, never that a human tapped it: Telegram link
  // scanning, URL unfurling and browser prefetch all issue this GET on their own.
  // So GET must change nothing.
  for (const action of ["approve", "deny", "nudge", "kill"] as const) {
    it(`is inert for ${action} — renders a confirm form, dispatches nothing`, async () => {
      const res = await callRawGet(token(action));
      expect(res.status).toBe(200);
      const body = await res.text();
      expect(body).toContain('method="POST"');
      expect(send).not.toHaveBeenCalled();
      expect(kill).not.toHaveBeenCalled();
      expect(consumedDecisions.size).toBe(0);
      // No session lookup either — nothing to observe, nothing to consume.
      expect(get).not.toHaveBeenCalled();
    });
  }

  it("posts back to the current url so a proxy base path survives", async () => {
    const body = await (await callRawGet(token("approve"))).text();
    expect(body).not.toContain('action="/api/');
  });

  it("still fails closed on a tampered token", async () => {
    const res = await callRawGet(`${token("approve")}tampered`);
    expect(res.status).toBe(403);
  });

  it("still reports 503 when no callback secret is configured", async () => {
    delete process.env.AO_NOTIFY_CALLBACK_SECRET;
    expect((await callRawGet(token("approve"))).status).toBe(503);
  });
});

describe("POST /api/notify-callback/[token]", () => {
  it("approve sends the approval message and records an audit event", async () => {
    const res = await callGet(token("approve"));
    expect(res.status).toBe(200);
    expect(send).toHaveBeenCalledWith("ao-5", NOTIFY_CALLBACK_MESSAGES.approve, "ao");
    expect(kill).not.toHaveBeenCalled();
    expect(recordActivityEvent).toHaveBeenCalledWith(
      expect.objectContaining({ kind: "api.notify_callback.approve", sessionId: "ao-5" }),
    );
    expect(res.headers.get("content-type")).toContain("text/html");
  });

  it("deny sends the denial message", async () => {
    await callGet(token("deny"));
    expect(send).toHaveBeenCalledWith("ao-5", NOTIFY_CALLBACK_MESSAGES.deny, "ao");
  });

  it("nudge sends the nudge message", async () => {
    await callGet(token("nudge"));
    expect(send).toHaveBeenCalledWith("ao-5", NOTIFY_CALLBACK_MESSAGES.nudge, "ao");
  });

  it("kill terminates the session instead of sending a message", async () => {
    const res = await callGet(token("kill"));
    expect(res.status).toBe(200);
    expect(kill).toHaveBeenCalledWith("ao-5", { projectId: "ao" });
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
    expect(send).toHaveBeenCalledWith("ao-5", NOTIFY_CALLBACK_MESSAGES.approve, "ao");
  });

  it("rejects every callback once live activity is active, even with stale needs_input (409)", async () => {
    // The agent resumed before the poll persisted the transition: canonical still
    // says needs_input and the nonce still matches, but live activity is active, so
    // no Approve/Deny/Kill may land on resumed work. (#13 review)
    for (const action of ["approve", "deny", "kill"] as const) {
      get.mockResolvedValueOnce(
        makeSession({ activity: "active", lifecycle: makeLifecycle("needs_input") }),
      );
      const res = await callGet(token(action));
      expect(res.status).toBe(409);
    }
    expect(send).not.toHaveBeenCalled();
    expect(kill).not.toHaveBeenCalled();
  });

  it("rejects when the decision nonce no longer matches the session (409)", async () => {
    // Token minted for the decision reported at A; session has since reported a
    // newer decision at B.
    get.mockResolvedValueOnce(makeSession({ metadata: reportMeta(new Date().toISOString()) }));
    const res = await callGet(token("approve", { nonce: "2026-07-07T00:00:00.000Z" }));
    expect(res.status).toBe(409);
    expect(send).not.toHaveBeenCalled();
    expect(recordActivityEvent).toHaveBeenCalledWith(
      expect.objectContaining({ kind: "api.notify_callback.stale" }),
    );
  });

  it("applies when the decision nonce matches the reported decision instant", async () => {
    const at = new Date().toISOString();
    get.mockResolvedValueOnce(makeSession({ metadata: reportMeta(at) }));
    const res = await callGet(token("approve", { nonce: identityFor(at) }));
    expect(res.status).toBe(200);
    expect(send).toHaveBeenCalledWith("ao-5", NOTIFY_CALLBACK_MESSAGES.approve, "ao");
  });

  it("rejects a token from the previous episode against a new prompt (409)", async () => {
    // Decision A resolved outside the callback, the agent resumed, and the poll
    // ended A's episode. A bare prompt B then opens a NEW episode while A's report
    // is still inside its freshness window — A's token must not answer B.
    get.mockResolvedValueOnce(
      makeSession({
        metadata: {
          ...reportMeta(DECISION_AT),
          [NOTIFY_DECISION_METADATA_KEYS.EPISODE_AT]: "2026-07-17T09:30:00.000Z",
        },
      }),
    );
    const res = await callGet(token("approve"));
    expect(res.status).toBe(409);
    expect(send).not.toHaveBeenCalled();
  });

  it("rejects when the session no longer has a decision report (409)", async () => {
    // The decision was resolved (report cleared / moved to a non-decision state),
    // so there is no identity to match even though activity is still waiting.
    get.mockResolvedValueOnce(makeSession({ metadata: reportMeta(DECISION_AT, "working") }));
    const res = await callGet(token("approve"));
    expect(res.status).toBe(409);
    expect(send).not.toHaveBeenCalled();
  });

  it("rejects a second action for the same decision — no double-dispatch (409)", async () => {
    // get default resolves a fresh pending+matching session each call.
    const first = await callGet(token("approve"));
    expect(first.status).toBe(200);
    const second = await callGet(token("deny"));
    expect(second.status).toBe(409);
    expect(send).toHaveBeenCalledTimes(1);
    expect(send).toHaveBeenCalledWith("ao-5", NOTIFY_CALLBACK_MESSAGES.approve, "ao");
    expect(recordActivityEvent).toHaveBeenCalledWith(
      expect.objectContaining({ kind: "api.notify_callback.duplicate" }),
    );
  });

  it("dispatches once for concurrent double-taps of the same decision", async () => {
    // Both requests pass the pending/identity checks before either dispatches —
    // the claim is what serializes them, so exactly one action may fire.
    const [a, b] = await Promise.all([callGet(token("approve")), callGet(token("kill"))]);
    const statuses = [a.status, b.status].sort();
    expect(statuses).toEqual([200, 409]);
    expect(send.mock.calls.length + kill.mock.calls.length).toBe(1);
  });

  it("keeps the claim closed after an ambiguous send failure — no double dispatch", async () => {
    // `send` can write the message to the runtime and THEN reject (an IPC timeout,
    // or a post-delivery confirmation probe error). The decision must stay consumed
    // so a retry cannot deliver the same approval twice. (#13 review)
    let delivered = 0;
    // Once-only: the retry is refused before dispatch, so send falls back to the
    // default resolving impl and never fires again.
    send.mockImplementationOnce(async () => {
      delivered += 1;
      throw new Error("delivered, then the confirmation probe timed out");
    });

    const first = await callGet(token("approve"));
    expect(first.status).toBe(500);
    expect(delivered).toBe(1);

    // Same token, retried after the ambiguous failure: refused, no second delivery.
    const retry = await callGet(token("approve"));
    expect(retry.status).toBe(409);
    expect(delivered).toBe(1);
  });

  it("reopens the claim for a PROVABLY pre-delivery send failure so a retry succeeds", async () => {
    // send reports (via the typed error) that it failed before crossing the
    // delivery boundary — restore/readiness failed, nothing was delivered — so the
    // claim reopens and a retry can deliver exactly once. (#13 review)
    let delivered = 0;
    send.mockImplementationOnce(async () => {
      throw new SessionSendNotDeliveredError("ao-5");
    });
    send.mockImplementationOnce(async () => {
      delivered += 1;
    });

    const first = await callGet(token("approve"));
    expect(first.status).toBe(500);
    expect(delivered).toBe(0);

    const retry = await callGet(token("approve"));
    expect(retry.status).toBe(200);
    expect(delivered).toBe(1);
  });

  it("keeps the claim closed when post-dispatch bookkeeping throws — the action already fired", async () => {
    recordActivityEvent.mockImplementationOnce(() => {
      throw new Error("audit sink down");
    });
    const first = await callGet(token("approve"));
    expect(first.status).toBe(500);
    expect(send).toHaveBeenCalledTimes(1);

    const second = await callGet(token("kill"));
    expect(second.status).toBe(409);
    expect(kill).not.toHaveBeenCalled();
  });

  it("keeps the decision answerable after a nudge (nudge does not consume)", async () => {
    // Nudge only asks for a status update — the underlying choice is still
    // outstanding, so Approve must still land afterwards.
    const nudged = await callGet(token("nudge"));
    expect(nudged.status).toBe(200);
    expect(send).toHaveBeenCalledWith("ao-5", NOTIFY_CALLBACK_MESSAGES.nudge, "ao");

    const approved = await callGet(token("approve"));
    expect(approved.status).toBe(200);
    expect(send).toHaveBeenCalledWith("ao-5", NOTIFY_CALLBACK_MESSAGES.approve, "ao");
  });

  it("still refuses a second resolving action after a nudge", async () => {
    expect((await callGet(token("nudge"))).status).toBe(200);
    expect((await callGet(token("approve"))).status).toBe(200);
    expect((await callGet(token("kill"))).status).toBe(409);
    expect(kill).not.toHaveBeenCalled();
  });

  it("serializes a concurrent resolving action behind an in-flight nudge", async () => {
    // The race: a Nudge passes its not-consumed check and pauses at dispatch; a
    // concurrent Deny must not consume and answer the decision while the Nudge is
    // mid-flight, or the Nudge would land after resolution. Per-decision
    // serialization holds the Deny until the Nudge completes. (#13 review)
    const events: string[] = [];
    let releaseNudge!: () => void;
    const nudgeGate = new Promise<void>((r) => (releaseNudge = r));
    let signalNudgeAtSend!: () => void;
    const nudgeAtSend = new Promise<void>((r) => (signalNudgeAtSend = r));

    try {
      send.mockImplementation((async (_id: string, msg: string) => {
        if (msg === NOTIFY_CALLBACK_MESSAGES.nudge) {
          events.push("nudge-send");
          signalNudgeAtSend();
          await nudgeGate; // hold the decision mid-dispatch
        } else {
          events.push("resolve-send");
        }
      }) as unknown as () => Promise<void>);

      const nudgeP = callGet(token("nudge"));
      await nudgeAtSend; // nudge is now holding the decision at dispatch

      const denyP = callGet(token("deny"));
      // Drain microtasks: with serialization the Deny is queued behind the Nudge
      // and cannot consume/dispatch; without it, "resolve-send" would appear here.
      await new Promise((r) => setTimeout(r, 0));
      expect(events).toEqual(["nudge-send"]);

      releaseNudge();
      const [nudgeRes, denyRes] = await Promise.all([nudgeP, denyP]);
      expect(nudgeRes.status).toBe(200);
      expect(denyRes.status).toBe(200);
      // Nudge fully completed before Deny consumed and dispatched.
      expect(events).toEqual(["nudge-send", "resolve-send"]);
    } finally {
      send.mockImplementation((async () => {}) as unknown as () => Promise<void>);
    }
  });

  it("refuses a repeat action after a dashboard restart — consumption is durable", async () => {
    // The route module is re-imported (fresh module state, as after a restart or
    // route reload) while the agent is still parked on the same decision. A
    // process-local marker would be empty here and would re-dispatch.
    const first = await callGet(token("approve"));
    expect(first.status).toBe(200);

    vi.resetModules();
    const { POST: freshGET } = await import("@/app/api/notify-callback/[token]/route");
    const second = await freshGET(makeRequest(), {
      params: Promise.resolve({ token: token("kill") }),
    });
    expect(second.status).toBe(409);
    expect(kill).not.toHaveBeenCalled();
  });

  it("resolves the session within the token's project, not the first id match", async () => {
    // Two projects can hold the same session id; a token signed for one must never
    // reach the other's session.
    await callGet(token("approve"));
    expect(get).toHaveBeenCalledWith("ao-5", "ao");

    const foreign = await callGet(token("approve", { projectId: "other" }));
    expect(foreign.status).toBe(404);
    expect(get).toHaveBeenLastCalledWith("ao-5", "other");
  });

  it("rejects a nudge once a resolving action has answered the decision", async () => {
    // Deny then Nudge would otherwise send "Continue if you can" into the very
    // decision that was just denied.
    expect((await callGet(token("deny"))).status).toBe(200);
    send.mockClear();

    const nudged = await callGet(token("nudge"));
    expect(nudged.status).toBe(409);
    expect(send).not.toHaveBeenCalled();
  });

  it("dispatches send within the signed project, not the first id match", async () => {
    // Validating the session against the token's project is not enough: unscoped,
    // send/kill rescan projects in config order and could act on project A.
    const res = await callGet(token("approve"));
    expect(res.status).toBe(200);
    expect(send).toHaveBeenCalledWith("ao-5", NOTIFY_CALLBACK_MESSAGES.approve, "ao");
  });

  it("dispatches kill within the signed project, not the first id match", async () => {
    const res = await callGet(token("kill"));
    expect(res.status).toBe(200);
    expect(kill).toHaveBeenCalledWith("ao-5", { projectId: "ao" });
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
