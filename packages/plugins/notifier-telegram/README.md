# notifier-telegram

Telegram bot notifier for AO. Delivers instant native push to your phone and —
for "needs your decision" events — renders **Approve / Deny / Nudge / Kill**
buttons you can tap to resolve the decision from the notification, closing the
loop back into AO.

## Setup

1. Create a bot with [@BotFather](https://t.me/BotFather) and copy its token.
2. Start a chat with your bot (or add it to a group), then find your `chatId`
   (e.g. message the bot and read `https://api.telegram.org/bot<token>/getUpdates`).
3. Add to `agent-orchestrator.yaml`:

```yaml
defaults:
  notifiers:
    - desktop
    - telegram

notifiers:
  telegram:
    plugin: telegram
    botToken: "123456:ABC-DEF..."      # or set TELEGRAM_BOT_TOKEN
    chatId: "987654321"                # or set TELEGRAM_CHAT_ID
    callbackBaseUrl: https://ao.example.com   # your dashboard's public URL
```

## Actionable callbacks

To make the Approve/Deny/Nudge/Kill buttons work end-to-end:

- Set `callbackBaseUrl` to a URL where the AO web dashboard is reachable **from
  your phone** (a public HTTPS URL — use a tunnel like Cloudflare Tunnel or
  ngrok for local dev). Buttons that call back into AO are omitted when this is
  unset; url buttons (like "View PR") still render.
- Set the shared secret `AO_NOTIFY_CALLBACK_SECRET` in the environment of both
  the orchestrator and the web dashboard. Each button is an HMAC-signed,
  expiring link; without the secret AO sends plain notifications (no buttons).

When you tap a button, the browser opens `/api/notify-callback/<token>`, which
verifies the signature and:

| Button  | Effect |
|---------|--------|
| Approve | Sends an approval message back into the session — the agent proceeds |
| Deny    | Sends a denial message back into the session |
| Nudge   | Asks the agent for a status update |
| Kill    | Terminates the session |

Every action is recorded in the AO audit trail.

## Config options

| Option | Default | Description |
|--------|---------|-------------|
| `botToken` | (required, or `TELEGRAM_BOT_TOKEN`) | Bot API token from BotFather |
| `chatId` | (required, or `TELEGRAM_CHAT_ID`) | Target chat/group id |
| `callbackBaseUrl` | (none) | Public base URL of the AO dashboard for action buttons |

Buttons are attached only for decision events (`session.needs_input`,
`review.changes_requested`, `merge.ready`); other events send a plain message.
