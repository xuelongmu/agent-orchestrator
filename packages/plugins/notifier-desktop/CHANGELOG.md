# @aoagents/ao-plugin-notifier-desktop

## 0.10.0

### Minor Changes

- 2caaec2: feat(notifier,web): native mobile push + actionable approve/deny callbacks

  Deliver actionable "needs your decision" notifications to your phone and let you
  resolve them from the notification — closing the loop back into AO (#13).
  - New `notifier-telegram` plugin: instant native push via a Telegram bot, with
    inline buttons. `notifyWithActions` renders Approve / Deny / Nudge / Kill as
    tappable URL buttons (plus a View PR link when a PR is attached). Configure
    `notifiers.telegram.botToken` (or `TELEGRAM_BOT_TOKEN`), `chatId`, and
    `callbackBaseUrl` (your dashboard's public URL).
  - Core now builds those actions for decision events and routes them through
    `notifyWithActions` when a notifier supports it. The relative mutating callbacks
    (Approve/Deny/Nudge/Kill) are passed only to notifiers that declare the new
    `Notifier.resolvesActionCallbacks` capability — Telegram, and the desktop backend
    only when the actionable AO Notifier.app is selected; notifiers that cannot turn a
    relative endpoint into a working URL (e.g. Slack, OpenClaw, and the dashboard,
    whose UI renders only `action.url`) never receive them. Ordinary URL actions (View PR) are delivered generically to
    every notifier. Approve/Deny/Nudge/Kill are attached to a report-backed
    `session.needs_input` (the genuine pending decision the callback resolves);
    `review.changes_requested` and `merge.ready` get a View PR link. Each button is an HMAC-signed, expiring token bound to the decision
    report's timestamp and minted with the shared `AO_NOTIFY_CALLBACK_SECRET`;
    without the secret set, notifications behave exactly as before (opt-in). New
    core exports: `buildNotifyActions`, `signCallbackToken`, `verifyCallbackToken`,
    `getNotifyCallbackSecret`, `isNotifyActionEvent`, `resolveDecisionEventType`,
    and the `NOTIFY_CALLBACK_*` constants.
  - New web route `/api/notify-callback/:token`. `GET` is inert: it verifies the
    token and renders a confirmation page whose form submits a `POST` — a signed URL
    proves AO minted it but not that a human tapped it, so link scanners, URL
    unfurlers, and browser prefetch must not be able to trigger the action. The
    `POST` is where the decision is resolved (Approve/Deny/Nudge answer back into the
    session via `sessionManager.send`; Kill terminates it) and recorded in the audit
    trail.

### Patch Changes

- Updated dependencies [669ed4c]
- Updated dependencies [1b9718a]
- Updated dependencies [2d456c4]
- Updated dependencies [2caaec2]
- Updated dependencies [c0ef32c]
  - @aoagents/ao-core@0.10.0

## 0.9.3

### Patch Changes

- Updated dependencies [cd45a7c]
  - @aoagents/ao-core@0.9.3

## 0.9.1

### Patch Changes

- 2d4c457: Fix canary nightly to include all publishable packages and fix Next.js import.meta.url build path issue
- Updated dependencies [2d4c457]
  - @aoagents/ao-core@0.9.1

## 0.9.0

### Patch Changes

- 2980570: Add the notifier test harness, dashboard notifications, and desktop notifier setup.
- Updated dependencies [73bed33]
- Updated dependencies [a610601]
- Updated dependencies [7d9b862]
- Updated dependencies [6d48022]
- Updated dependencies [fcedb25]
- Updated dependencies [94981dc]
- Updated dependencies [2980570]
- Updated dependencies [d5d0f07]
  - @aoagents/ao-core@0.9.0

## 0.8.0

### Patch Changes

- Updated dependencies
  - @aoagents/ao-core@0.8.0

## 0.7.0

### Minor Changes

- 0f5ae0b: feat: native Windows support

  AO now runs natively on Windows. The default runtime on Windows is `process`
  (ConPTY via `node-pty` + named pipes — no tmux, no WSL); the dashboard,
  agents (claude-code, codex, kimicode, aider, opencode, cursor), `ao doctor`,
  and `ao update` all work out of the box. Each session gets a small detached
  pty-host helper that wraps a ConPTY behind `\\.\pipe\ao-pty-<sessionId>`,
  registered so `ao stop` can reach it.

  A new cross-platform abstraction layer (`packages/core/src/platform.ts`)
  centralises every platform branch behind helpers like `isWindows()`,
  `getDefaultRuntime()`, `getShell()`, `killProcessTree()`, `findPidByPort()`,
  and `getEnvDefaults()`. Path comparison uses `pathsEqual` /
  `canonicalCompareKey` to handle NTFS case-insensitivity. PATH wrappers for
  agent plugins (`gh`, `git`) ship as `.cjs` + `.cmd` shims on Windows;
  `script-runner` runs `.ps1` siblings of `.sh` scripts via PowerShell. New
  `ao-doctor.ps1` / `ao-update.ps1` shipped.

  `ao open` is now cross-platform: it sources sessions from `sm.list()`
  instead of `tmux list-sessions` (so `runtime-process` sessions on Windows
  appear), and the open action branches per OS — `open-iterm-tab` stays the
  macOS path, native handling on Windows and Linux.

  Behaviour on macOS and Linux is unchanged. Every Windows path is gated
  behind `isWindows()`; `runtime-tmux` and the bash hook flows are untouched.

  See `docs/CROSS_PLATFORM.md` for the developer reference (helper inventory,
  EPERM-vs-ESRCH gotcha, PowerShell-vs-bash differences, pre-merge checklist).
  The Windows runtime architecture (pty-host, pipe protocol, registry, sweep,
  mux WS Windows branch) is documented in `docs/ARCHITECTURE.md`.

### Patch Changes

- Updated dependencies [0f5ae0b]
- Updated dependencies [fe33bb7]
- Updated dependencies [7c46dc9]
  - @aoagents/ao-core@0.7.0

## 0.6.0

### Patch Changes

- Updated dependencies
- Updated dependencies [40aeb78]
- Updated dependencies
- Updated dependencies
  - @aoagents/ao-core@0.6.0

## 0.5.0

### Patch Changes

- Updated dependencies [dd07b6b]
  - @aoagents/ao-core@0.5.0

## 0.4.0

### Patch Changes

- Updated dependencies [2306078]
- Updated dependencies [faaddb1]
- Updated dependencies [f330a1e]
- Updated dependencies [a862327]
- Updated dependencies [331f1ce]
- Updated dependencies [703d584]
- Updated dependencies [f674422]
- Updated dependencies [62353eb]
- Updated dependencies [bd36c7b]
- Updated dependencies [e7ad928]
- Updated dependencies [ca8c4cc]
- Updated dependencies [7b82374]
- Updated dependencies [4701122]
- Updated dependencies [c8af50f]
- Updated dependencies [bcdda4b]
- Updated dependencies [1cbf657]
- Updated dependencies [c447c7c]
- Updated dependencies [a45eb32]
- Updated dependencies [7072143]
- Updated dependencies [ed2dcea]
  - @aoagents/ao-core@0.4.0

## 0.2.0

### Patch Changes

- Updated dependencies [3a650b0]
  - @composio/ao-core@0.2.0
