import { describe, it, expect } from "vitest";
import { createHmac } from "node:crypto";
import {
  NOTIFY_CALLBACK_ACTIONS,
  NOTIFY_CALLBACK_SECRET_ENV,
  buildNotifyActions,
  getNotifyCallbackSecret,
  isNotifyActionEvent,
  isNotifyCallbackAction,
  resolveDecisionEventType,
  signCallbackToken,
  verifyCallbackToken,
  type NotifyCallbackPayload,
} from "../notify-callback.js";
import type { EventType, OrchestratorEvent } from "../types.js";
import { NOTIFICATION_DATA_SCHEMA_VERSION } from "../notification-data.js";

const SECRET = "test-secret-abc123";

function makePayload(overrides: Partial<NotifyCallbackPayload> = {}): NotifyCallbackPayload {
  return {
    sessionId: "my-app-1",
    projectId: "my-app",
    action: "approve",
    exp: Date.now() + 60_000,
    ...overrides,
  };
}

function makeEvent(overrides: Partial<OrchestratorEvent> = {}): OrchestratorEvent {
  return {
    id: "evt-1",
    type: "session.needs_input",
    priority: "action",
    sessionId: "my-app-1",
    projectId: "my-app",
    timestamp: new Date(),
    message: "Agent needs a decision",
    data: {},
    ...overrides,
  };
}

describe("callback token signing", () => {
  it("round-trips a valid payload", () => {
    const payload = makePayload();
    const token = signCallbackToken(payload, SECRET);
    expect(verifyCallbackToken(token, SECRET)).toEqual(payload);
  });

  it("round-trips an optional nonce", () => {
    const payload = makePayload({ nonce: "agent_needs_input:needs_decision:2026-07-07T00:00:00Z" });
    const token = signCallbackToken(payload, SECRET);
    expect(verifyCallbackToken(token, SECRET)).toEqual(payload);
  });

  it("rejects a token signed with a different secret", () => {
    const token = signCallbackToken(makePayload(), SECRET);
    expect(verifyCallbackToken(token, "other-secret")).toBeNull();
  });

  it("rejects a tampered payload body", () => {
    const token = signCallbackToken(makePayload({ action: "approve" }), SECRET);
    const [, sig] = token.split(".");
    // Swap in a different (validly-encoded) body but keep the old signature.
    const forgedBody = Buffer.from(JSON.stringify(makePayload({ action: "kill" })))
      .toString("base64")
      .replace(/\+/g, "-")
      .replace(/\//g, "_")
      .replace(/=+$/, "");
    expect(verifyCallbackToken(`${forgedBody}.${sig}`, SECRET)).toBeNull();
  });

  it("rejects an expired token", () => {
    const token = signCallbackToken(makePayload({ exp: Date.now() - 1 }), SECRET);
    expect(verifyCallbackToken(token, SECRET)).toBeNull();
  });

  it("honors the injected clock for expiry", () => {
    const exp = 10_000;
    const token = signCallbackToken(makePayload({ exp }), SECRET);
    expect(verifyCallbackToken(token, SECRET, 9_999)).not.toBeNull();
    expect(verifyCallbackToken(token, SECRET, 10_001)).toBeNull();
  });

  it("rejects malformed tokens", () => {
    expect(verifyCallbackToken("", SECRET)).toBeNull();
    expect(verifyCallbackToken("no-dot", SECRET)).toBeNull();
    expect(verifyCallbackToken(".sig", SECRET)).toBeNull();
    expect(verifyCallbackToken("body.", SECRET)).toBeNull();
    expect(verifyCallbackToken("not-base64!.sig", SECRET)).toBeNull();
  });

  it("rejects a validly-signed token whose action is out of range", () => {
    // Hand-craft a token with a correct signature but an unknown action, to
    // prove the payload-shape check runs independently of signature checks.
    const crafted = signRawBody({ ...makePayload(), action: "explode" }, SECRET);
    expect(verifyCallbackToken(crafted, SECRET)).toBeNull();
  });
});

// Sign an arbitrary (possibly malformed) payload with a valid HMAC, mirroring
// the module's internal token format. Used to exercise payload-shape rejection.
function signRawBody(payload: unknown, secret: string): string {
  const b64 = (buf: Buffer) =>
    buf.toString("base64").replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  const body = b64(Buffer.from(JSON.stringify(payload), "utf-8"));
  const sig = b64(createHmac("sha256", secret).update(body).digest());
  return `${body}.${sig}`;
}

describe("isNotifyCallbackAction", () => {
  it("accepts known actions and rejects others", () => {
    for (const action of NOTIFY_CALLBACK_ACTIONS) {
      expect(isNotifyCallbackAction(action)).toBe(true);
    }
    expect(isNotifyCallbackAction("nope")).toBe(false);
    expect(isNotifyCallbackAction(123)).toBe(false);
  });
});

describe("isNotifyActionEvent", () => {
  it("flags decision events", () => {
    const decision: EventType[] = [
      "session.needs_input",
      "review.changes_requested",
      "merge.ready",
    ];
    for (const type of decision) expect(isNotifyActionEvent(type)).toBe(true);
  });

  it("does not flag non-decision events", () => {
    const other: EventType[] = ["ci.failing", "pr.created", "summary.all_complete"];
    for (const type of other) expect(isNotifyActionEvent(type)).toBe(false);
  });
});

describe("resolveDecisionEventType", () => {
  it("prefers data.semanticType over the raw event type", () => {
    const event = makeEvent({
      type: "reaction.triggered",
      data: {
        schemaVersion: NOTIFICATION_DATA_SCHEMA_VERSION,
        semanticType: "merge.ready",
        subject: { session: { id: "ao-5", projectId: "ao" } },
      },
    });
    expect(resolveDecisionEventType(event)).toBe("merge.ready");
  });

  it("falls back to the raw event type when there is no v3 data", () => {
    expect(resolveDecisionEventType(makeEvent({ type: "session.needs_input", data: {} }))).toBe(
      "session.needs_input",
    );
  });
});

describe("getNotifyCallbackSecret", () => {
  it("returns trimmed non-empty secret", () => {
    expect(getNotifyCallbackSecret({ [NOTIFY_CALLBACK_SECRET_ENV]: "  s3cret  " })).toBe("s3cret");
  });
  it("returns null when unset or blank", () => {
    expect(getNotifyCallbackSecret({})).toBeNull();
    expect(getNotifyCallbackSecret({ [NOTIFY_CALLBACK_SECRET_ENV]: "   " })).toBeNull();
  });
});

describe("buildNotifyActions", () => {
  it("builds Approve/Deny/Nudge/Kill for a decision event", () => {
    const actions = buildNotifyActions(makeEvent(), { secret: SECRET });
    expect(actions.map((a) => a.label)).toEqual(["Approve", "Deny", "Nudge", "Kill"]);
    for (const action of actions) {
      expect(action.callbackEndpoint).toMatch(/^\/api\/notify-callback\//);
      const token = action.callbackEndpoint!.replace("/api/notify-callback/", "");
      expect(verifyCallbackToken(token, SECRET)).not.toBeNull();
    }
  });

  it("encodes the session, project, and action in each token", () => {
    const actions = buildNotifyActions(
      makeEvent({ sessionId: "sess-9", projectId: "proj-9" }),
      { secret: SECRET },
    );
    const approve = actions.find((a) => a.label === "Approve")!;
    const payload = verifyCallbackToken(
      approve.callbackEndpoint!.replace("/api/notify-callback/", ""),
      SECRET,
    );
    expect(payload).toMatchObject({ sessionId: "sess-9", projectId: "proj-9", action: "approve" });
  });

  it("gives review.changes_requested only a View PR link, no action buttons", () => {
    const event = makeEvent({
      type: "review.changes_requested",
      data: {
        schemaVersion: NOTIFICATION_DATA_SCHEMA_VERSION,
        subject: {
          session: { id: "my-app-1", projectId: "my-app" },
          pr: { number: 7, url: "https://github.com/acme/x/pull/7" },
        },
      },
    });
    const actions = buildNotifyActions(event, { secret: SECRET });
    expect(actions).toEqual([{ label: "View PR", url: "https://github.com/acme/x/pull/7" }]);
  });

  it("gives merge.ready only a View PR link, no action buttons", () => {
    const event = makeEvent({
      type: "merge.ready",
      data: {
        schemaVersion: NOTIFICATION_DATA_SCHEMA_VERSION,
        subject: {
          session: { id: "my-app-1", projectId: "my-app" },
          pr: { number: 7, url: "https://github.com/acme/x/pull/7" },
        },
      },
    });
    expect(buildNotifyActions(event, { secret: SECRET })).toEqual([
      { label: "View PR", url: "https://github.com/acme/x/pull/7" },
    ]);
  });

  it("appends a View PR link after the action buttons for needs_input with a PR", () => {
    const event = makeEvent({
      data: {
        schemaVersion: NOTIFICATION_DATA_SCHEMA_VERSION,
        subject: {
          session: { id: "my-app-1", projectId: "my-app" },
          pr: { number: 7, url: "https://github.com/acme/x/pull/7" },
        },
      },
    });
    const actions = buildNotifyActions(event, { secret: SECRET });
    expect(actions.map((a) => a.label)).toEqual(["Approve", "Deny", "Nudge", "Kill", "View PR"]);
  });

  it("binds the token to the nonce when provided", () => {
    const actions = buildNotifyActions(makeEvent(), { secret: SECRET, nonce: "trigger:abc:123" });
    const approve = actions.find((a) => a.label === "Approve")!;
    const payload = verifyCallbackToken(
      approve.callbackEndpoint!.replace("/api/notify-callback/", ""),
      SECRET,
    );
    expect(payload?.nonce).toBe("trigger:abc:123");
  });

  it("omits the nonce field when none is provided", () => {
    const actions = buildNotifyActions(makeEvent(), { secret: SECRET });
    const approve = actions.find((a) => a.label === "Approve")!;
    const payload = verifyCallbackToken(
      approve.callbackEndpoint!.replace("/api/notify-callback/", ""),
      SECRET,
    );
    expect(payload?.nonce).toBeUndefined();
  });

  it("returns no actions for non-decision events", () => {
    expect(buildNotifyActions(makeEvent({ type: "ci.failing" }), { secret: SECRET })).toEqual([]);
  });

  it("builds actions for a reaction-wrapped decision via data.semanticType", () => {
    // agent-needs-input / approved-and-green are notified as `reaction.triggered`
    // events whose real decision type lives in data.semanticType.
    const event = makeEvent({
      type: "reaction.triggered",
      data: {
        schemaVersion: NOTIFICATION_DATA_SCHEMA_VERSION,
        semanticType: "session.needs_input",
        subject: { session: { id: "ao-5", projectId: "ao" } },
      },
    });
    const actions = buildNotifyActions(event, { secret: SECRET });
    expect(actions.map((a) => a.label)).toEqual(["Approve", "Deny", "Nudge", "Kill"]);
  });

  it("returns no actions for a reaction wrapping a non-decision semanticType", () => {
    const event = makeEvent({
      type: "reaction.triggered",
      data: {
        schemaVersion: NOTIFICATION_DATA_SCHEMA_VERSION,
        semanticType: "ci.failing",
        subject: { session: { id: "ao-5", projectId: "ao" } },
      },
    });
    expect(buildNotifyActions(event, { secret: SECRET })).toEqual([]);
  });

  it("respects a custom ttl", () => {
    const now = 1_000_000;
    const actions = buildNotifyActions(makeEvent(), { secret: SECRET, ttlMs: 5_000, now });
    const token = actions[0].callbackEndpoint!.replace("/api/notify-callback/", "");
    expect(verifyCallbackToken(token, SECRET, now + 4_999)).not.toBeNull();
    expect(verifyCallbackToken(token, SECRET, now + 5_001)).toBeNull();
  });
});
