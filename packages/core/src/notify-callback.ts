import { createHmac, timingSafeEqual } from "node:crypto";
import type { NotifyAction, OrchestratorEvent } from "./types.js";
import { getNotificationDataV3 } from "./notification-data.js";

/**
 * Actionable "needs your decision" notifications (#13).
 *
 * A decision event (`session.needs_input`/`report.needs_input`,
 * `review.changes_requested`, `merge.ready`) — where the needs-input pair may
 * arrive wrapped in a `reaction.triggered`, carrying the real decision type as
 * its notification `semanticType` — is pushed to an action-capable notifier
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

/**
 * Decision (semantic) types that a human resolves with Approve/Deny/Nudge/Kill.
 * Both the direct `session.needs_input` transition and the report-watcher's
 * `report.needs_input` (reaction key `report-needs-input`, the primary path for
 * agent `needs_input`/`needs_decision` reports) count. These are semantic types,
 * not just raw EventTypes, so this list is `string[]`.
 */
export const NEEDS_INPUT_DECISION_TYPES: readonly string[] = [
  "session.needs_input",
  "report.needs_input",
];

/** Decision types that carry any actionable notification buttons. */
export const NOTIFY_ACTION_EVENT_TYPES: readonly string[] = [
  ...NEEDS_INPUT_DECISION_TYPES,
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
  /**
   * Decision-instance identity at mint time (the session's report-watcher
   * ACTIVE_TRIGGER, which folds in the decision's report timestamp). The
   * callback route requires this to still match the session's current identity,
   * so a token minted for one decision can't answer a later, different one.
   * Absent for decisions with no report-driven trigger (e.g. a permission
   * prompt); the callback then requires the current identity to be absent too.
   */
  nonce?: string;
}

export function isNotifyCallbackAction(value: unknown): value is NotifyCallbackAction {
  return (
    typeof value === "string" && (NOTIFY_CALLBACK_ACTIONS as readonly string[]).includes(value)
  );
}

/** True when an event/semantic type carries actionable decision buttons. */
export function isNotifyActionEvent(type: string): boolean {
  return (NOTIFY_ACTION_EVENT_TYPES as readonly string[]).includes(type);
}

/**
 * The decision type an event actually represents. Reaction-wrapped decisions
 * (e.g. `agent-needs-input`, `approved-and-green`) are notified as
 * `reaction.triggered` events whose real decision type lives in the
 * notification data's `semanticType` — prefer that, falling back to the raw
 * event type for direct transition notifications.
 */
export function resolveDecisionEventType(event: OrchestratorEvent): string {
  return getNotificationDataV3(event.data)?.semanticType ?? event.type;
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
    !isNotifyCallbackAction(candidate.action) ||
    (candidate.nonce !== undefined && typeof candidate.nonce !== "string")
  ) {
    return null;
  }
  if (!Number.isFinite(candidate.exp) || candidate.exp < now) return null;

  return {
    sessionId: candidate.sessionId,
    projectId: candidate.projectId,
    action: candidate.action,
    exp: candidate.exp,
    ...(typeof candidate.nonce === "string" ? { nonce: candidate.nonce } : {}),
  };
}

export interface BuildNotifyActionsOptions {
  /**
   * Shared HMAC secret. Without it, no mutating callback actions are built — but
   * URL-only actions (View PR) still are, so a read-only link survives the default
   * secretless opt-out configuration. (#13 review)
   */
  secret?: string;
  /**
   * Decision-instance identity to bind the tokens to (the session's current
   * ACTIVE_TRIGGER). Persisted into each token as its {@link NotifyCallbackPayload.nonce}.
   */
  nonce?: string;
  /** Token lifetime in ms. Defaults to {@link NOTIFY_CALLBACK_DEFAULT_TTL_MS}. */
  ttlMs?: number;
  /** Injectable clock for tests. */
  now?: number;
}

/**
 * Build the notification buttons for a decision event.
 *
 * Approve / Deny / Nudge / Kill are attached only for a needs-input decision
 * (`session.needs_input` or the report-watcher's `report.needs_input` — see
 * {@link NEEDS_INPUT_DECISION_TYPES}) that is backed by an agent decision
 * report — i.e. `options.nonce` is set to that report's timestamp. That report is a stable, per-decision identity the
 * callback route re-checks, so an old link can't answer a later, different
 * decision. A needs_input with no report identity (e.g. a bare detected prompt)
 * gets no mutating buttons, because there'd be nothing reliable to bind them to.
 * Each button's `callbackEndpoint` is a relative, signed path
 * (`/api/notify-callback/<token>`); action-capable notifiers prepend their own
 * public base URL. `review.changes_requested` and `merge.ready` get only a
 * `View PR` link. A `View PR` url button is appended for any decision event that
 * carries a PR URL.
 *
 * Returns `[]` for non-decision events, so callers can pass any event through.
 */
export function buildNotifyActions(
  event: OrchestratorEvent,
  options: BuildNotifyActionsOptions,
): NotifyAction[] {
  const decisionType = resolveDecisionEventType(event);
  if (!isNotifyActionEvent(decisionType)) return [];

  const now = options.now ?? Date.now();
  const exp = now + (options.ttlMs ?? NOTIFY_CALLBACK_DEFAULT_TTL_MS);

  const actions: NotifyAction[] = [];

  if (
    options.secret &&
    NEEDS_INPUT_DECISION_TYPES.includes(decisionType) &&
    options.nonce !== undefined
  ) {
    for (const action of NOTIFY_CALLBACK_ACTIONS) {
      const token = signCallbackToken(
        { sessionId: event.sessionId, projectId: event.projectId, action, exp, nonce: options.nonce },
        options.secret,
      );
      actions.push({
        label: NOTIFY_CALLBACK_LABELS[action],
        callbackEndpoint: `/api/notify-callback/${token}`,
      });
    }
  }

  const prUrl = getNotificationDataV3(event.data)?.subject.pr?.url;
  if (prUrl) actions.push({ label: "View PR", url: prUrl });

  return actions;
}

/** Whether a callback endpoint is already an absolute http(s) URL. */
function isAbsoluteHttpCallback(endpoint: string | undefined): endpoint is string {
  return typeof endpoint === "string" && /^https?:\/\//i.test(endpoint);
}

/**
 * The subset of `actions` a notifier may render, given whether it can resolve a
 * RELATIVE callback endpoint (`/api/notify-callback/<token>`) into a working URL.
 *
 * Notifiers that resolve them (Telegram/desktop prepend their configured base URL;
 * the dashboard is same-origin) receive everything. Notifiers that cannot (Slack
 * turns the endpoint into an interaction value, OpenClaw into a relative markdown
 * link — neither reaches the AO route) must NOT be handed the mutating callback
 * actions, or the human sees Approve/Deny/Nudge/Kill controls that can never
 * resolve the decision. Ordinary URL actions (View PR) and any already-absolute
 * callback endpoint pass through to every notifier. (#13 review)
 */
export function actionsForNotifier(
  actions: NotifyAction[],
  resolvesActionCallbacks: boolean | undefined,
): NotifyAction[] {
  if (resolvesActionCallbacks) return actions;
  return actions.filter((action) => action.url || isAbsoluteHttpCallback(action.callbackEndpoint));
}

/**
 * A configured public base URL an action-capable notifier prepends to relative
 * callback endpoints, normalized (trailing slashes stripped), or `null` when the
 * value is missing or not a valid absolute http(s) URL. A malformed base (e.g.
 * `localhost:3000`) must be treated exactly like an unset one: notifiers then omit
 * callback actions and still deliver the plain human alert, rather than building
 * an invalid button URL that the transport rejects wholesale. (#13 review)
 */
export function normalizeCallbackBaseUrl(value: unknown): string | null {
  if (typeof value !== "string") return null;
  const trimmed = value.trim();
  if (trimmed.length === 0) return null;
  let parsed: URL;
  try {
    parsed = new URL(trimmed);
  } catch {
    return null;
  }
  if (parsed.protocol !== "http:" && parsed.protocol !== "https:") return null;
  return trimmed.replace(/\/+$/, "");
}

/**
 * Resolve a relative callback endpoint into a final absolute URL through the
 * configured public base, PRESERVING the base's path so a reverse-proxy prefix
 * (`https://host/ao`) survives (`https://host/ao/api/...`) — `new URL(rel, base)`
 * would discard it. Returns the endpoint unchanged if it is already absolute
 * http(s), or `null` when the base is invalid or the endpoint is not a safe
 * root-relative path. Shared by every actionable notifier so the base-URL rule
 * cannot diverge. (#13 review)
 */
export function resolveCallbackUrl(
  base: string | null | undefined,
  endpoint: string | undefined,
): string | null {
  if (!endpoint) return null;
  if (isAbsoluteHttpCallback(endpoint)) return endpoint;
  if (!endpoint.startsWith("/")) return null;
  const normalizedBase = normalizeCallbackBaseUrl(base);
  if (!normalizedBase) return null;
  return `${normalizedBase}${endpoint}`;
}
