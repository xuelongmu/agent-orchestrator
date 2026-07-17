import {
  getNotificationDataV3,
  normalizeCallbackBaseUrl,
  resolveCallbackUrl,
  type PluginModule,
  type Notifier,
  type OrchestratorEvent,
  type NotifyAction,
  type NotifyContext,
  type EventPriority,
  type NotificationDataV3,
} from "@aoagents/ao-core";

export const manifest = {
  name: "telegram",
  slot: "notifier" as const,
  description: "Notifier plugin: Telegram bot push with actionable inline buttons",
  version: "0.1.0",
};

/** Telegram caps message text at 4096 chars. */
const TELEGRAM_TEXT_MAX = 4096;
/** Max escaped length for the free-form message line, leaving room for fields. */
const MESSAGE_MAX_ESCAPED = 3500;
/** Max raw length for a single field value before escaping. */
const FIELD_VALUE_MAX_RAW = 300;
/** Inline keyboard buttons per row for the action grid. */
const BUTTONS_PER_ROW = 2;

interface TelegramButton {
  text: string;
  url: string;
}

interface TelegramReplyMarkup {
  inline_keyboard: TelegramButton[][];
}

interface Tone {
  emoji: string;
  label: string;
}

const SUCCESS_TONE: Tone = { emoji: "\u{2705}", label: "Complete" };

const PRIORITY_TONE: Record<EventPriority, Tone> = {
  urgent: { emoji: "\u{1F6A8}", label: "Urgent" },
  action: { emoji: "\u{1F449}", label: "Action required" },
  warning: { emoji: "\u{26A0}\u{FE0F}", label: "Warning" },
  info: { emoji: "\u{2139}\u{FE0F}", label: "Information" },
};

/** Escape the five characters Telegram's HTML parse mode is sensitive to. */
function escapeHtml(value: string): string {
  return value.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
}

/** Truncate raw (unescaped) text with an ellipsis. Safe to escape afterwards. */
function truncate(value: string, maxLength: number): string {
  return value.length > maxLength ? `${value.slice(0, maxLength - 1)}…` : value;
}

/**
 * Clamp an already-escaped HTML string to `max`, backing off so the cut never
 * lands inside an entity (e.g. `&amp;`). Used for tag-free content (the message
 * line, `post()`); tag balance is preserved separately by whole-line assembly.
 */
function clampEscapedHtml(escaped: string, max: number): string {
  if (escaped.length <= max) return escaped;
  let cut = max;
  const amp = escaped.lastIndexOf("&", cut - 1);
  if (amp !== -1) {
    const semi = escaped.indexOf(";", amp);
    if (semi === -1 || semi >= cut) cut = amp; // cut is inside an entity → back off
  }
  return escaped.slice(0, cut);
}

/**
 * Join HTML lines up to `max` chars, dropping whole trailing lines that don't
 * fit. Because each line carries balanced `<b>` tags and complete entities,
 * whole-line truncation can never split a tag or entity (which Telegram would
 * reject as malformed HTML).
 */
function joinLinesWithinLimit(lines: string[], max: number): string {
  const out: string[] = [];
  let len = 0;
  for (const line of lines) {
    const addition = (out.length > 0 ? 1 : 0) + line.length;
    if (len + addition > max) break;
    out.push(line);
    len += addition;
  }
  return out.join("\n");
}

function titleCase(value: string): string {
  return value
    .split(/[_\s.-]+/)
    .filter(Boolean)
    .map((part) => `${part.slice(0, 1).toUpperCase()}${part.slice(1)}`)
    .join(" ");
}

/**
 * The decision type an event actually represents.
 *
 * A reaction that handles a transition suppresses the direct notification and
 * delivers a `reaction.triggered` event instead, carrying the real type in
 * `data.semanticType`. Switching on the raw `event.type` would title the primary
 * mobile alert "Reaction Triggered" for exactly the events that matter most —
 * `agent-needs-input` and `approved-and-green`. (#13 review)
 */
function semanticEventType(event: OrchestratorEvent, data: NotificationDataV3 | null): string {
  return data?.semanticType ?? event.type;
}

function toneForEvent(event: OrchestratorEvent, data: NotificationDataV3 | null): Tone {
  const type = semanticEventType(event, data);
  if (type === "merge.ready") return { ...SUCCESS_TONE, label: "Ready to merge" };
  if (type === "summary.all_complete") return { ...SUCCESS_TONE, label: "All complete" };
  if (type === "ci.failing" || type === "session.stuck") return PRIORITY_TONE.urgent;
  if (type === "review.changes_requested") return PRIORITY_TONE.warning;
  return PRIORITY_TONE[event.priority] ?? PRIORITY_TONE.info;
}

function eventTitle(event: OrchestratorEvent, data: NotificationDataV3 | null): string {
  const pr = data?.subject.pr;
  switch (semanticEventType(event, data)) {
    case "ci.failing":
      return pr ? `CI failing on PR #${pr.number}` : "CI failing";
    case "merge.ready":
      return pr ? `PR #${pr.number} ready to merge` : "Pull request ready to merge";
    case "review.changes_requested":
      return pr ? `Changes requested on PR #${pr.number}` : "Review changes requested";
    // Both needs-input decision types: the direct transition and the
    // report-watcher's `report-needs-input`, which is the primary path.
    case "session.needs_input":
    case "report.needs_input":
      return "Agent needs your decision";
    case "session.stuck":
      return "Agent may be stuck";
    case "session.killed":
    case "session.exited":
      return "Agent exited";
    case "pr.closed":
      return pr ? `PR #${pr.number} closed` : "Pull request closed";
    case "summary.all_complete":
      return "All sessions complete";
    default:
      return titleCase(event.type);
  }
}

function field(label: string, value: string | number | undefined | null): string | null {
  if (value === undefined || value === null || value === "") return null;
  return `<b>${escapeHtml(label)}:</b> ${escapeHtml(truncate(String(value), FIELD_VALUE_MAX_RAW))}`;
}

function buildText(event: OrchestratorEvent, data: NotificationDataV3 | null): string {
  const tone = toneForEvent(event, data);
  const pr = data?.subject.pr;
  const issue = data?.subject.issue;
  const branch =
    pr?.branch && pr.baseBranch
      ? `${pr.branch} -> ${pr.baseBranch}`
      : (pr?.branch ?? pr?.baseBranch ?? data?.subject.branch);

  const lines: string[] = [
    `${tone.emoji} <b>${escapeHtml(eventTitle(event, data))}</b>`,
    "",
    // Escape first (produces complete entities), then clamp on an entity
    // boundary so a long message can't split `&amp;` mid-sequence.
    clampEscapedHtml(escapeHtml(event.message), MESSAGE_MAX_ESCAPED),
    "",
    field("Project", event.projectId),
    field("Session", event.sessionId),
    field("Priority", tone.label),
    pr ? field("Pull Request", `#${pr.number}${pr.title ? ` - ${pr.title}` : ""}`) : null,
    field("Branch", branch),
    issue ? field("Issue", `${issue.id}${issue.title ? ` - ${issue.title}` : ""}`) : null,
    data?.ci?.status ? field("CI", titleCase(data.ci.status)) : null,
    data?.review?.decision ? field("Review", titleCase(data.review.decision)) : null,
  ].filter((line): line is string => line !== null);

  // Assemble on whole-line boundaries so no `<b>` tag or entity is ever split.
  return joinLinesWithinLimit(lines, TELEGRAM_TEXT_MAX);
}

/**
 * Resolve an action into an absolute, tappable Telegram URL button.
 * `url` actions pass through; `callbackEndpoint` actions (relative signed paths
 * from core) are prefixed with the notifier's public base URL. Returns `null`
 * when a callback action can't be made absolute (no base URL configured).
 */
function actionToButton(action: NotifyAction, callbackBaseUrl: string | null): TelegramButton | null {
  const text = truncate(action.label, 64);
  if (action.url) return { text, url: action.url };
  const resolved = resolveCallbackUrl(callbackBaseUrl, action.callbackEndpoint);
  return resolved ? { text, url: resolved } : null;
}

function buildReplyMarkup(
  actions: NotifyAction[],
  callbackBaseUrl: string | null,
): TelegramReplyMarkup | null {
  const buttons = actions
    .map((action) => actionToButton(action, callbackBaseUrl))
    .filter((button): button is TelegramButton => button !== null);
  if (buttons.length === 0) return null;

  const rows: TelegramButton[][] = [];
  for (let i = 0; i < buttons.length; i += BUTTONS_PER_ROW) {
    rows.push(buttons.slice(i, i + BUTTONS_PER_ROW));
  }
  return { inline_keyboard: rows };
}

async function sendMessage(
  botToken: string,
  chatId: string,
  text: string,
  replyMarkup: TelegramReplyMarkup | null,
): Promise<void> {
  const payload: Record<string, unknown> = {
    chat_id: chatId,
    text,
    parse_mode: "HTML",
    disable_web_page_preview: true,
  };
  if (replyMarkup) payload.reply_markup = replyMarkup;

  const response = await fetch(`https://api.telegram.org/bot${botToken}/sendMessage`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(payload),
  });

  if (!response.ok) {
    const body = await response.text();
    throw new Error(`Telegram sendMessage failed (${response.status}): ${body}`);
  }
}


export function create(config?: Record<string, unknown>): Notifier {
  const botToken =
    (config?.botToken as string | undefined) ?? process.env.TELEGRAM_BOT_TOKEN ?? undefined;
  const chatId =
    config?.chatId !== undefined && config?.chatId !== null
      ? String(config.chatId)
      : (process.env.TELEGRAM_CHAT_ID ?? undefined);
  // Accept only an absolute http(s) base. A malformed value (e.g. `localhost:3000`)
  // is treated exactly like an unset one, so callback buttons are omitted and the
  // plain alert still sends — an invalid button URL would make Telegram reject the
  // whole sendMessage and lose the notification. (#13 review)
  const rawCallbackBase =
    typeof config?.callbackBaseUrl === "string" ? config.callbackBaseUrl.trim() : "";
  const callbackBaseUrl = normalizeCallbackBaseUrl(config?.callbackBaseUrl);

  if (!botToken || !chatId) {
    console.warn(
      "[notifier-telegram] Missing botToken or chatId — notifications will be no-ops. " +
        "Set notifiers.telegram.botToken (or TELEGRAM_BOT_TOKEN) and notifiers.telegram.chatId.",
    );
  } else if (rawCallbackBase.length > 0 && !callbackBaseUrl) {
    console.warn(
      "[notifier-telegram] callbackBaseUrl is not a valid absolute http(s) URL — action " +
        "buttons that call back into AO will be omitted. Set notifiers.telegram.callbackBaseUrl " +
        "to your dashboard's public URL (e.g. https://host or https://host/ao).",
    );
  } else if (!callbackBaseUrl) {
    console.warn(
      "[notifier-telegram] No callbackBaseUrl configured — action buttons that call back " +
        "into AO will be omitted. Set notifiers.telegram.callbackBaseUrl to your dashboard's public URL.",
    );
  }

  return {
    name: "telegram",

    // Resolves relative callback endpoints against callbackBaseUrl (and drops the
    // button when unset), so it may receive the mutating callback actions.
    resolvesActionCallbacks: true,

    async notify(event: OrchestratorEvent): Promise<void> {
      if (!botToken || !chatId) return;
      const data = getNotificationDataV3(event.data);
      await sendMessage(botToken, chatId, buildText(event, data), null);
    },

    async notifyWithActions(event: OrchestratorEvent, actions: NotifyAction[]): Promise<void> {
      if (!botToken || !chatId) return;
      const data = getNotificationDataV3(event.data);
      const replyMarkup = buildReplyMarkup(actions, callbackBaseUrl);
      await sendMessage(botToken, chatId, buildText(event, data), replyMarkup);
    },

    async post(message: string, _context?: NotifyContext): Promise<string | null> {
      if (!botToken || !chatId) return null;
      await sendMessage(
        botToken,
        chatId,
        clampEscapedHtml(escapeHtml(message), TELEGRAM_TEXT_MAX),
        null,
      );
      return null;
    },
  };
}

export default { manifest, create } satisfies PluginModule<Notifier>;
