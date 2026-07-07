import { createHmac, timingSafeEqual } from "node:crypto";
import type { EventType, NotifyAction, OrchestratorEvent } from "./types.js";
import { getNotificationDataV3 } from "./notification-data.js";

/**
 * Actionable "needs your decision" notifications (#13).
 *
 * A decision event (`session.needs_input`, `review.changes_requested`,
 * `merge.ready`) is pushed to a mobile, action-capable notifier (e.g. Telegram)
 * with buttons. Each button is a signed, expiring URL back into the AO web
 * server's `/api/notify-callback/<token>` route. Tapping it resolves the
 * decision by sending a message to the session (or killing it) and records the
 * action in the audit trail.
 *
 * The signing secret is shared between the signer (core, when building actions)
 * and the verifier (the web callback route) via the `AO_NOTIFY_CALLBACK_SECRET`
 * environment variable. When unset, no callback actions are built and notifiers
 * fall back to plain notifications — the feature is opt-in.
 */

/** Environment variable holding the shared HMAC secret for callback tokens. */
export const NOTIFY_CALLBACK_SECRET_ENV = "AO_NOTIFY_CALLBACK_SECRET";

/** Default token lifetime — a decision left unanswered past this expires. */
export const NOTIFY_CALLBACK_DEFAULT_TTL_MS = 24 * 60 * 60 * 1000;

/** Actions a human can take from a decision notification. */
export const NOTIFY_CALLBACK_ACTIONS = ["approve", "deny", "nudge", "kill"] as const;

export type NotifyCallbackAction = (typeof NOTIFY_CALLBACK_ACTIONS)[number];

/** Event types that carry actionable decision buttons. */
export const NOTIFY_ACTION_EVENT_TYPES: readonly EventType[] = [
  "session.needs_input",
  "review.changes_requested",
  "merge.ready",
];

/**
 * Human-facing labels for each callback action button.
 */
export const NOTIFY_CALLBACK_LABELS: Record<NotifyCallbackAction, string> = {
  approve: "Approve",
  deny: "Deny",
  nudge: "Nudge",
  kill: "Kill",
};

/**
 * Messages sent back into the session when an approve/deny/nudge action fires.
 * Kept plain ASCII so they pass cleanly through shell/PTY-based runtimes on all
 * platforms. `kill` has no message — it terminates the session instead.
 */
export const NOTIFY_CALLBACK_MESSAGES: Record<Exclude<NotifyCallbackAction, "kill">, string> = {
  approve: "Approved from your phone -- please proceed.",
  deny: "Denied from your phone -- do not proceed with that action; wait for further instructions or choose a safe alternative.",
  nudge: "Checking in from your phone -- what is your current status? Continue if you can, otherwise report what is blocking you.",
};

export interface NotifyCallbackPayload {
  /** Session the action targets. */
  sessionId: string;
  /** Project the session belongs to. */
  projectId: string;
  /** Action to perform. */
  action: NotifyCallbackAction;
  /** Expiry, epoch milliseconds. */
  exp: number;
}

export function isNotifyCallbackAction(value: unknown): value is NotifyCallbackAction {
  return (
    typeof value === "string" && (NOTIFY_CALLBACK_ACTIONS as readonly string[]).includes(value)
  );
}

/** True when an event type carries actionable decision buttons. */
export function isNotifyActionEvent(type: EventType): boolean {
  return NOTIFY_ACTION_EVENT_TYPES.includes(type);
}

/** Read the shared callback secret from the environment (trimmed, non-empty). */
export function getNotifyCallbackSecret(env: NodeJS.ProcessEnv = process.env): string | null {
  const raw = env[NOTIFY_CALLBACK_SECRET_ENV];
  const trimmed = typeof raw === "string" ? raw.trim() : "";
  return trimmed.length > 0 ? trimmed : null;
}

function base64url(input: Buffer): string {
  return input.toString("base64").replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

function base64urlDecode(input: string): Buffer {
  const normalized = input.replace(/-/g, "+").replace(/_/g, "/");
  const padded = normalized.padEnd(Math.ceil(normalized.length / 4) * 4, "=");
  return Buffer.from(padded, "base64");
}

function sign(body: string, secret: string): string {
  return base64url(createHmac("sha256", secret).update(body).digest());
}

/**
 * Produce a compact, URL-safe, HMAC-signed token encoding the callback payload.
 * Format: `<base64url(json)>.<base64url(hmac)>`.
 */
export function signCallbackToken(payload: NotifyCallbackPayload, secret: string): string {
  const body = base64url(Buffer.from(JSON.stringify(payload), "utf-8"));
  return `${body}.${sign(body, secret)}`;
}

/**
 * Verify a callback token's signature and expiry. Returns the payload when
 * valid, or `null` for any tampering, malformed input, or expiry.
 */
export function verifyCallbackToken(
  token: string,
  secret: string,
  now: number = Date.now(),
): NotifyCallbackPayload | null {
  if (typeof token !== "string" || token.length === 0) return null;
  const dot = token.indexOf(".");
  if (dot <= 0 || dot === token.length - 1) return null;

  const body = token.slice(0, dot);
  const providedSig = token.slice(dot + 1);
  const expectedSig = sign(body, secret);

  const providedBuf = Buffer.from(providedSig, "utf-8");
  const expectedBuf = Buffer.from(expectedSig, "utf-8");
  if (providedBuf.length !== expectedBuf.length) return null;
  if (!timingSafeEqual(providedBuf, expectedBuf)) return null;

  let parsed: unknown;
  try {
    parsed = JSON.parse(base64urlDecode(body).toString("utf-8"));
  } catch {
    return null;
  }

  if (!parsed || typeof parsed !== "object") return null;
  const candidate = parsed as Record<string, unknown>;
  if (
    typeof candidate.sessionId !== "string" ||
    typeof candidate.projectId !== "string" ||
    typeof candidate.exp !== "number" ||
    !isNotifyCallbackAction(candidate.action)
  ) {
    return null;
  }
  if (!Number.isFinite(candidate.exp) || candidate.exp < now) return null;

  return {
    sessionId: candidate.sessionId,
    projectId: candidate.projectId,
    action: candidate.action,
    exp: candidate.exp,
  };
}

export interface BuildNotifyActionsOptions {
  /** Shared HMAC secret. Required — without it no callback actions are built. */
  secret: string;
  /** Token lifetime in ms. Defaults to {@link NOTIFY_CALLBACK_DEFAULT_TTL_MS}. */
  ttlMs?: number;
  /** Injectable clock for tests. */
  now?: number;
}

/**
 * Build the actionable buttons for a decision event. Each of Approve / Deny /
 * Nudge / Kill becomes a {@link NotifyAction} whose `callbackEndpoint` is a
 * relative, signed path (`/api/notify-callback/<token>`). Action-capable
 * notifiers prepend their own public base URL to form the tappable link. A
 * `View PR` url button is appended when the event carries a PR URL.
 *
 * Returns `[]` for non-decision events, so callers can pass any event through.
 */
export function buildNotifyActions(
  event: OrchestratorEvent,
  options: BuildNotifyActionsOptions,
): NotifyAction[] {
  if (!isNotifyActionEvent(event.type)) return [];

  const now = options.now ?? Date.now();
  const exp = now + (options.ttlMs ?? NOTIFY_CALLBACK_DEFAULT_TTL_MS);

  const actions: NotifyAction[] = NOTIFY_CALLBACK_ACTIONS.map((action) => {
    const token = signCallbackToken(
      { sessionId: event.sessionId, projectId: event.projectId, action, exp },
      options.secret,
    );
    return {
      label: NOTIFY_CALLBACK_LABELS[action],
      callbackEndpoint: `/api/notify-callback/${token}`,
    };
  });

  const prUrl = getNotificationDataV3(event.data)?.subject.pr?.url;
  if (prUrl) actions.push({ label: "View PR", url: prUrl });

  return actions;
}
