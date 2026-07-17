import { type NextRequest } from "next/server";
import { getServices } from "@/lib/services";
import { validateIdentifier } from "@/lib/validation";
import {
  ACTIVITY_STATE,
  SessionNotFoundError,
  activeDecisionId,
  consumeDecision,
  getNotifyCallbackSecret,
  isResolvingCallbackAction,
  isTerminalSession,
  releaseDecision,
  verifyCallbackToken,
  NOTIFY_CALLBACK_MESSAGES,
  recordActivityEvent,
  type NotifyCallbackAction,
} from "@aoagents/ao-core";
import {
  getCorrelationId,
  recordApiObservation,
  resolveProjectIdForSessionId,
} from "@/lib/observability";

/**
 * GET /api/notify-callback/:token — resolve a decision from a mobile
 * notification action button (#13).
 *
 * The token is an HMAC-signed, expiring payload minted by core when it builds
 * Approve/Deny/Nudge/Kill buttons for a decision event. Tapping a button opens
 * this URL in the phone's browser; we verify the signature (so an attacker
 * cannot forge an action — no CSRF token needed, the URL itself is the secret),
 * then answer back into the session and record the action in the audit trail.
 */

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

export async function GET(
  request: NextRequest,
  { params }: { params: Promise<{ token: string }> },
) {
  const correlationId = getCorrelationId(request);
  const startedAt = Date.now();
  const { token } = await params;

  const secret = getNotifyCallbackSecret();
  if (!secret) {
    return htmlResponse(
      503,
      "Callbacks not enabled",
      "This orchestrator has no AO_NOTIFY_CALLBACK_SECRET configured, so notification actions cannot be verified.",
    );
  }

  const payload = verifyCallbackToken(token, secret);
  if (!payload) {
    return htmlResponse(
      403,
      "Link invalid or expired",
      "This action link could not be verified. It may have already been used up, tampered with, or expired.",
    );
  }

  const { sessionId, projectId, action } = payload;

  // The session id came from a token we signed, but validate its shape anyway
  // before it reaches a shell-based runtime (defense in depth).
  if (validateIdentifier(sessionId, "sessionId")) {
    return htmlResponse(400, "Invalid session", "The action link referenced a malformed session.");
  }

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
        method: "GET",
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
    const consumes = isResolvingCallbackAction(action);
    if (consumes && !consumeDecision(projectId, sessionId, decisionId)) {
      recordApiObservation({
        config,
        method: "GET",
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

    // Release the instance only when the dispatch itself failed, so a genuine
    // failure stays retryable. Anything after this point (audit bookkeeping) must
    // NOT release it — the action already fired, and re-opening the decision
    // would let a second tap dispatch it twice.
    // Dispatch inside the SIGNED project. Validating the session against the
    // token's project is not enough on its own: unscoped, `send`/`kill` resolve
    // the session by scanning projects in config order, so with a duplicate
    // session id they could act on a different project's session than the one
    // just validated. (#13, review)
    try {
      if (action === "kill") {
        await sessionManager.kill(sessionId, { projectId });
      } else {
        await sessionManager.send(sessionId, NOTIFY_CALLBACK_MESSAGES[action], projectId);
      }
    } catch (err) {
      if (consumes) releaseDecision(projectId, sessionId, decisionId);
      throw err;
    }

    recordApiObservation({
      config,
      method: "GET",
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
  } catch (err) {
    const msg = err instanceof Error ? err.message : String(err);
    const notFound = err instanceof SessionNotFoundError;
    const { config } = await getServices().catch(() => ({ config: undefined }));
    if (config) {
      recordApiObservation({
        config,
        method: "GET",
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
