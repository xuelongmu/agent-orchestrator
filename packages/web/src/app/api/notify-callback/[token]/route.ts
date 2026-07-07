import { type NextRequest } from "next/server";
import { getServices } from "@/lib/services";
import { validateIdentifier } from "@/lib/validation";
import {
  SessionNotFoundError,
  getNotifyCallbackSecret,
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
    const resolvedProjectId = resolveProjectIdForSessionId(config, sessionId) ?? projectId;

    if (action === "kill") {
      await sessionManager.kill(sessionId);
    } else {
      await sessionManager.send(sessionId, NOTIFY_CALLBACK_MESSAGES[action]);
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
