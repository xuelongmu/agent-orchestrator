---
"@aoagents/ao-plugin-notifier-telegram": minor
"@aoagents/ao-core": minor
"@aoagents/ao-cli": minor
"@aoagents/ao-web": minor
---

feat(notifier,web): native mobile push + actionable approve/deny callbacks

Deliver actionable "needs your decision" notifications to your phone and let you
resolve them from the notification — closing the loop back into AO (#13).

- New `notifier-telegram` plugin: instant native push via a Telegram bot, with
  inline buttons. `notifyWithActions` renders Approve / Deny / Nudge / Kill as
  tappable URL buttons (plus a View PR link when a PR is attached). Configure
  `notifiers.telegram.botToken` (or `TELEGRAM_BOT_TOKEN`), `chatId`, and
  `callbackBaseUrl` (your dashboard's public URL).
- Core now builds those actions for decision events and routes them through
  `notifyWithActions` when a notifier supports it. Approve/Deny/Nudge/Kill are
  attached to `session.needs_input` (the genuine pending decision the callback
  resolves); `review.changes_requested` and `merge.ready` get a View PR link.
  Each button is an HMAC-signed, expiring token bound to the session's current
  decision instance and minted with the shared `AO_NOTIFY_CALLBACK_SECRET`;
  without the secret set, notifications behave exactly as before (opt-in). New
  core exports: `buildNotifyActions`, `signCallbackToken`, `verifyCallbackToken`,
  `getNotifyCallbackSecret`, `isNotifyActionEvent`, `resolveDecisionEventType`,
  and the `NOTIFY_CALLBACK_*` constants.
- New web route `GET /api/notify-callback/:token` verifies the token, resolves
  the decision (Approve/Deny/Nudge answer back into the session via
  `sessionManager.send`; Kill terminates it), and records the action in the
  audit trail.
