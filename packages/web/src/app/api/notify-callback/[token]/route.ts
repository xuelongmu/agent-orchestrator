import { type NextRequest } from "next/server";
import { getServices } from "@/lib/services";
import {
  ACTIVITY_STATE,
  SessionNotFoundError,
  isValidSessionIdComponent,
  activeDecisionId,
  consumeDecision,
  getNotifyCallbackSecret,
  isNudgeBlocked,
  isResolvingCallbackAction,
  isTerminalSession,
  releaseDecision,
  SessionSendNotDeliveredError,
  verifyCallbackToken,
  NOTIFY_CALLBACK_LABELS,
  NOTIFY_CALLBACK_MESSAGES,
  recordActivityEvent,
  type NotifyCallbackAction,
  type NotifyCallbackPayload,
} from "@aoagents/ao-core";
import {
  getCorrelationId,
  recordApiObservation,
  resolveProjectIdForSessionId,
} from "@/lib/observability";

/**
 * /api/notify-callback/:token — resolve a decision from a mobile notification
 * action button (#13).
 *
 * The token is an HMAC-signed, expiring payload minted by core when it builds
 * Approve/Deny/Nudge/Kill buttons for a decision event. Tapping a button opens
 * this URL in the phone's browser.
 *
 * GET IS INERT: it only renders a confirmation page. The signature proves the URL
 * was minted by AO, but it CANNOT prove a human tapped it — Telegram link
 * scanning, URL unfurling/expansion, and browser prefetch all issue the GET on
 * their own. Mutating there would approve, deny, or kill a session nobody
 * confirmed. The mutation lives in POST, which those scanners do not perform, and
 * which a human reaches only by pressing the button on the rendered page.
 * (#13 review)
 *
 * POST re-verifies the token from scratch — it trusts nothing from the GET.
 */

/**
 * Per-decision in-process serialization. AO's web dashboard is a single
 * long-running process (the same assumption the durable claim relies on), so
 * chaining the authorize-and-dispatch section by decision id prevents a Nudge
 * from passing its "not yet consumed" check, releasing the metadata lock, and
 * then dispatching AFTER a concurrent resolving action has consumed and answered
 * the same decision — a contradictory post-resolution nudge. Requests for
 * DIFFERENT decisions never contend. (#13 review)
 */
const decisionChains = new Map<string, Promise<unknown>>();
function serializeByDecision<T>(key: string, fn: () => Promise<T>): Promise<T> {
  const prev = decisionChains.get(key) ?? Promise.resolve();
  const result = prev.then(fn, fn);
  // Tail never rejects, so one request's failure can't break the chain; it also
  // evicts the map entry once nothing is queued behind it, bounding growth.
  const chain: Promise<void> = result.then(
    () => {
      if (decisionChains.get(key) === chain) decisionChains.delete(key);
    },
    () => {
      if (decisionChains.get(key) === chain) decisionChains.delete(key);
    },
  );
  decisionChains.set(key, chain);
  return result;
}

function escapeHtml(value: string): string {
  return value.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
}

/**
 * Render a small standalone confirmation page. This HTML is served to an
 * external phone browser (from a tapped Telegram button), not to the dashboard
 * app, so it can't use the app's Tailwind bundle — styles live in a `<style>`
 * block scoped to this response.
 */
function htmlResponse(status: number, heading: string, detail: string): Response {
  const body = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8" />
<meta name="viewport" content="width=device-width, initial-scale=1" />
<meta name="robots" content="noindex" />
<title>Agent Orchestrator</title>
<style>
  body { font-family: system-ui, sans-serif; background: #0b0d12; color: #e6e8ee; margin: 0;
    display: flex; min-height: 100vh; align-items: center; justify-content: center; padding: 24px; }
  .card { max-width: 420px; text-align: center; }
  h1 { font-size: 20px; margin: 0 0 12px; }
  p { font-size: 15px; line-height: 1.5; color: #a9b0c0; margin: 0; }
</style>
</head>
<body>
<div class="card">
<h1>${escapeHtml(heading)}</h1>
<p>${escapeHtml(detail)}</p>
</div>
</body>
</html>`;
  return new Response(body, {
    status,
    headers: { "Content-Type": "text/html; charset=utf-8", "Cache-Control": "no-store" },
  });
}

const ACTION_HEADING: Record<NotifyCallbackAction, string> = {
  approve: "Approved",
  deny: "Denied",
  nudge: "Nudge sent",
  kill: "Session killed",
};

const ACTION_PROMPT: Record<NotifyCallbackAction, string> = {
  approve: "Approve this decision and let the agent proceed?",
  deny: "Deny this decision and tell the agent not to proceed?",
  nudge: "Ask the agent for a status update?",
  kill: "Kill this session? This cannot be undone.",
};

/**
 * The confirmation page GET renders. Its form POSTs back to the same signed URL,
 * which is where the mutation actually happens — so a scanner or prefetcher that
 * only issues the GET changes nothing.
 *
 * The form omits `action` so the browser posts to the CURRENT url. A
 * root-absolute `/api/...` would break behind a reverse proxy serving AO under a
 * base path, and the token is already in the address the page was fetched from.
 */
function confirmationResponse(action: NotifyCallbackAction): Response {
  const body = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8" />
<meta name="viewport" content="width=device-width, initial-scale=1" />
<meta name="robots" content="noindex" />
<title>Agent Orchestrator</title>
<style>
  body { font-family: system-ui, sans-serif; background: #0b0d12; color: #e6e8ee; margin: 0;
    display: flex; min-height: 100vh; align-items: center; justify-content: center; padding: 24px; }
  .card { max-width: 420px; text-align: center; }
  h1 { font-size: 20px; margin: 0 0 12px; }
  p { font-size: 15px; line-height: 1.5; color: #a9b0c0; margin: 0 0 20px; }
  button { font: inherit; font-weight: 600; color: #0b0d12; background: #e6e8ee; border: 0;
    border-radius: 8px; padding: 12px 20px; cursor: pointer; }
</style>
</head>
<body>
<div class="card">
<h1>${escapeHtml(NOTIFY_CALLBACK_LABELS[action])}</h1>
<p>${escapeHtml(ACTION_PROMPT[action])}</p>
<form method="POST">
<button type="submit">${escapeHtml(NOTIFY_CALLBACK_LABELS[action])}</button>
</form>
</div>
</body>
</html>`;
  return new Response(body, {
    status: 200,
    headers: { "Content-Type": "text/html; charset=utf-8", "Cache-Control": "no-store" },
  });
}

/** Verify the token and reject anything malformed, before any side effect. */
function verifyRequest(
  token: string,
):
  | { ok: true; payload: NotifyCallbackPayload }
  | { ok: false; response: Response } {
  const secret = getNotifyCallbackSecret();
  if (!secret) {
    return {
      ok: false,
      response: htmlResponse(
        503,
        "Callbacks not enabled",
        "This orchestrator has no AO_NOTIFY_CALLBACK_SECRET configured, so notification actions cannot be verified.",
      ),
    };
  }

  const payload = verifyCallbackToken(token, secret);
  if (!payload) {
    return {
      ok: false,
      response: htmlResponse(
        403,
        "Link invalid or expired",
        "This action link could not be verified. It may have already been used up, tampered with, or expired.",
      ),
    };
  }

  // The session id came from a token we signed, but validate its shape anyway
  // before it reaches a shell-based runtime (defense in depth). Validate against
  // core's own session-ID contract — the same character set core enforces at
  // creation, and length-unbounded — so a valid ID built from a long
  // sessionPrefix is never rejected here (a generic 128-char identifier cap
  // would 400 a session core can legitimately create). (#13 review)
  if (!isValidSessionIdComponent(payload.sessionId)) {
    return {
      ok: false,
      response: htmlResponse(
        400,
        "Invalid session",
        "The action link referenced a malformed session.",
      ),
    };
  }

  return { ok: true, payload };
}

/**
 * Render the confirmation page. Deliberately free of side effects: no session
 * lookup, no consumption, no dispatch — an automated fetch of this URL must
 * change nothing.
 */
export async function GET(
  _request: NextRequest,
  { params }: { params: Promise<{ token: string }> },
) {
  const { token } = await params;
  const verified = verifyRequest(token);
  if (!verified.ok) return verified.response;
  return confirmationResponse(verified.payload.action);
}

export async function POST(
  request: NextRequest,
  { params }: { params: Promise<{ token: string }> },
) {
  const correlationId = getCorrelationId(request);
  const startedAt = Date.now();
  const { token } = await params;

  const verified = verifyRequest(token);
  if (!verified.ok) return verified.response;
  const { payload } = verified;
  const { sessionId, projectId, action } = payload;

  try {
    const { config, sessionManager } = await getServices();

    // Re-read the session and confirm the decision is still pending before
    // acting. A signed link stays valid for its TTL, so a human could tap an
    // older notification after they already answered and the agent resumed —
    // applying a stale Approve/Deny/Nudge would inject an unrelated instruction,
    // and a stale Kill would terminate resumed work. Scoping the lookup to the
    // token's project also makes project ownership structural rather than a
    // separate guard: `get` can no longer return a same-id session from another
    // project. (#13, review)
    const session = await sessionManager.get(sessionId, projectId);
    const resolvedProjectId =
      session?.projectId ?? resolveProjectIdForSessionId(config, sessionId) ?? projectId;

    if (!session) {
      return htmlResponse(
        404,
        "Session not found",
        "That session no longer exists — it may have already finished or been cleaned up.",
      );
    }

    // Scoping the lookup above should make this unreachable, but this is an auth
    // path: assert the invariant rather than trust a single call site to hold it.
    if (session.projectId !== projectId) {
      return htmlResponse(
        404,
        "Session not found",
        "That session no longer exists — it may have already finished or been cleaned up.",
      );
    }

    // The buttons answer a pending human decision, so require the session to be
    // non-terminal AND still awaiting input. `blocked` is an error/stuck state,
    // not a human prompt, so it does not count.
    const decisionPending =
      !isTerminalSession(session) &&
      (session.lifecycle?.session.state === "needs_input" ||
        session.activity === ACTIVITY_STATE.WAITING_INPUT);

    // Identity comes from the same core chokepoint the token was minted through,
    // so a token is honoured only against the decision instance it names. A
    // resolved/superseded decision — or a bare detected prompt, whose spent report
    // the lifecycle poll retires — yields a different identity or none at all.
    // Tokens are always minted with a nonce, so an absent nonce never matches.
    const decisionId = activeDecisionId(session);
    const decisionMatches = payload.nonce !== undefined && payload.nonce === decisionId;

    // The `decisionId === null` clause is what decisionMatches already implies;
    // stating it here narrows the identity to a string for the dispatch below.
    if (!decisionPending || decisionId === null || !decisionMatches) {
      recordApiObservation({
        config,
        method: "POST",
        path: "/api/notify-callback/[token]",
        correlationId,
        startedAt,
        outcome: "failure",
        statusCode: 409,
        projectId: resolvedProjectId,
        sessionId,
        reason: decisionPending ? "callback decision mismatch" : "stale callback action",
        data: { action, activity: session.activity, status: session.status, decisionMatches },
      });
      recordActivityEvent({
        projectId: resolvedProjectId,
        sessionId,
        source: "api",
        kind: "api.notify_callback.stale",
        level: "warn",
        summary: `notification action "${action}" ignored as stale for session ${sessionId}`,
        data: { action, activity: session.activity, status: session.status, decisionMatches },
      });
      return htmlResponse(
        409,
        "Action no longer applies",
        "This decision is no longer pending — it was already answered, or the session has finished or moved on, so no action was taken.",
      );
    }

    // Consume the decision instance before dispatching, so a double-tap — or an
    // Approve followed by Kill before the agent leaves needs_input — cannot fire a
    // second resolving action. The move from pending to consumed happens under the
    // session metadata's file lock, so it is atomic against a concurrent tap and
    // durable across a dashboard restart (a process-local marker was neither).
    //
    // Nudge is exempt: it only asks the agent for a status update and leaves the
    // choice outstanding, so consuming on a nudge would strand a still-pending
    // decision with no way to answer it. (#13, review)
    // A Nudge does not consume, but it must not walk into a decision that a
    // resolving action already answered (Deny→Nudge would send "continue if you
    // can" into the decision just denied), NOR into a decision that a new report
    // superseded between the get() above and now. The check is a locked read at
    // the central helper, so it authorizes against the stored identity at dispatch
    // time — the same boundary the resolving path uses.
    // Serialize the authorize-and-dispatch section per decision so a Nudge
    // cannot slip in after a concurrent resolving action has consumed it.
    const decisionKey = `${projectId}:${sessionId}:${decisionId}`;
    return await serializeByDecision(decisionKey, async (): Promise<Response> => {
      // Re-fetch and revalidate INSIDE the serialized section. The pending +
      // identity checks above ran on a snapshot taken before this callback may
      // have waited its turn behind another action for the same decision (a
      // queued Approve/Kill behind an in-flight Nudge). While it waited the agent
      // could have resumed (activity → active) or a newer report could have
      // superseded the decision — so consume/isNudgeBlocked/send/kill must
      // authorize against the CURRENT project-scoped session, not the stale
      // snapshot, or a resolving action lands on already-resumed work. (#13 review)
      const fresh = await sessionManager.get(sessionId, projectId);
      const freshPending =
        !!fresh &&
        fresh.projectId === projectId &&
        !isTerminalSession(fresh) &&
        (fresh.lifecycle?.session.state === "needs_input" ||
          fresh.activity === ACTIVITY_STATE.WAITING_INPUT);
      const freshDecisionId = fresh ? activeDecisionId(fresh) : null;
      if (!freshPending || freshDecisionId === null || freshDecisionId !== decisionId) {
        recordApiObservation({
          config,
          method: "POST",
          path: "/api/notify-callback/[token]",
          correlationId,
          startedAt,
          outcome: "failure",
          statusCode: 409,
          projectId: resolvedProjectId,
          sessionId,
          reason: "decision no longer pending at dispatch",
          data: { action },
        });
        recordActivityEvent({
          projectId: resolvedProjectId,
          sessionId,
          source: "api",
          kind: "api.notify_callback.stale",
          level: "warn",
          summary: `notification action "${action}" ignored — decision resolved or agent resumed before dispatch for session ${sessionId}`,
          data: { action },
        });
        return htmlResponse(
          409,
          "Action no longer applies",
          "This decision is no longer pending — it was already answered, or the session has finished or moved on, so no action was taken.",
        );
      }

      const consumes = isResolvingCallbackAction(action);
      if (!consumes && isNudgeBlocked(projectId, sessionId, decisionId)) {
        recordApiObservation({
          config,
          method: "POST",
          path: "/api/notify-callback/[token]",
          correlationId,
          startedAt,
          outcome: "failure",
          statusCode: 409,
          projectId: resolvedProjectId,
          sessionId,
          reason: "decision already resolved or superseded",
          data: { action },
        });
        recordActivityEvent({
          projectId: resolvedProjectId,
          sessionId,
          source: "api",
          kind: "api.notify_callback.duplicate",
          level: "warn",
          summary: `notification action "${action}" ignored — decision already resolved or superseded for session ${sessionId}`,
          data: { action },
        });
        return htmlResponse(
          409,
          "Already handled",
          "This decision was already answered or has moved on, so no further action was taken.",
        );
      }

      if (consumes && !consumeDecision(projectId, sessionId, decisionId)) {
        recordApiObservation({
          config,
          method: "POST",
          path: "/api/notify-callback/[token]",
          correlationId,
          startedAt,
          outcome: "failure",
          statusCode: 409,
          projectId: resolvedProjectId,
          sessionId,
          reason: "duplicate callback action",
          data: { action },
        });
        recordActivityEvent({
          projectId: resolvedProjectId,
          sessionId,
          source: "api",
          kind: "api.notify_callback.duplicate",
          level: "warn",
          summary: `notification action "${action}" ignored as already resolved for session ${sessionId}`,
          data: { action },
        });
        return htmlResponse(
          409,
          "Already handled",
          "This decision was already answered from another tap, so no further action was taken.",
        );
      }

      // Dispatch inside the SIGNED project. Validating the session against the
      // token's project is not enough on its own: unscoped, `send`/`kill` resolve
      // the session by scanning projects in config order, so with a duplicate
      // session id they could act on a different project's session than the one
      // just validated. (#13, review)
      //
      // The consumed claim stays closed once the runtime DELIVERY BOUNDARY is
      // crossed, even on rejection: an at-or-after-delivery failure is only
      // SUSPECTED non-delivery (an IPC timeout, a post-delivery confirmation error),
      // so reopening would let a retry deliver the same action twice. The claim is
      // reopened ONLY for a PROVABLY pre-delivery failure, which `send` reports via
      // the typed SessionSendNotDeliveredError (or SessionNotFoundError) — never
      // inferred from message text — so a restore/readiness failure before dispatch
      // stays retryable. `kill` never reopens (it has no such typed pre-delivery
      // signal and is destructive). (#13 review)
      try {
        if (action === "kill") {
          await sessionManager.kill(sessionId, { projectId });
        } else {
          await sessionManager.send(sessionId, NOTIFY_CALLBACK_MESSAGES[action], projectId);
        }
      } catch (dispatchErr) {
        if (
          dispatchErr instanceof SessionSendNotDeliveredError ||
          dispatchErr instanceof SessionNotFoundError
        ) {
          releaseDecision(projectId, sessionId, decisionId);
        }
        throw dispatchErr;
      }

      recordApiObservation({
        config,
        method: "POST",
        path: "/api/notify-callback/[token]",
        correlationId,
        startedAt,
        outcome: "success",
        statusCode: 200,
        projectId: resolvedProjectId,
        sessionId,
        data: { action },
      });
      recordActivityEvent({
        projectId: resolvedProjectId,
        sessionId,
        source: "api",
        kind: `api.notify_callback.${action}`,
        summary: `notification action "${action}" resolved for session ${sessionId}`,
        data: { action, source: "notify-callback" },
      });

      return htmlResponse(
        200,
        ACTION_HEADING[action],
        `Session ${sessionId} received your "${action}" decision. You can close this page.`,
      );
    });
  } catch (err) {
    const msg = err instanceof Error ? err.message : String(err);
    const notFound = err instanceof SessionNotFoundError;
    const { config } = await getServices().catch(() => ({ config: undefined }));
    if (config) {
      recordApiObservation({
        config,
        method: "POST",
        path: "/api/notify-callback/[token]",
        correlationId,
        startedAt,
        outcome: "failure",
        statusCode: notFound ? 404 : 500,
        projectId,
        sessionId,
        reason: msg,
        data: { action },
      });
    }
    recordActivityEvent({
      projectId,
      sessionId,
      source: "api",
      kind: "api.notify_callback.failed",
      level: "error",
      summary: `notification action "${action}" failed for session ${sessionId}: ${msg}`,
      data: { action, reason: msg },
    });

    if (notFound) {
      return htmlResponse(
        404,
        "Session not found",
        "That session no longer exists — it may have already finished or been cleaned up.",
      );
    }
    return htmlResponse(500, "Action failed", "Something went wrong resolving this decision.");
  }
}
