import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { NotifyAction, OrchestratorEvent } from "@aoagents/ao-core";
import { create, manifest } from "../index.js";

const BOT_TOKEN = "123:ABC";
const CHAT_ID = "987654";
const BASE_URL = "https://ao.example.com";

function makeEvent(overrides: Partial<OrchestratorEvent> = {}): OrchestratorEvent {
  return {
    id: "evt-1",
    type: "session.needs_input",
    priority: "action",
    sessionId: "ao-5",
    projectId: "ao",
    timestamp: new Date("2026-03-20T12:00:00Z"),
    message: "Agent needs a decision",
    data: {},
    ...overrides,
  };
}

function okFetch() {
  return vi.fn().mockResolvedValue({ ok: true, status: 200, text: async () => "" });
}

function lastBody(fetchMock: ReturnType<typeof vi.fn>): Record<string, unknown> {
  return JSON.parse(fetchMock.mock.calls.at(-1)![1].body);
}

const CALLBACK_ACTIONS: NotifyAction[] = [
  { label: "Approve", callbackEndpoint: "/api/notify-callback/tok-approve" },
  { label: "Deny", callbackEndpoint: "/api/notify-callback/tok-deny" },
  { label: "View PR", url: "https://github.com/acme/x/pull/7" },
];

describe("notifier-telegram", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    delete process.env.TELEGRAM_BOT_TOKEN;
    delete process.env.TELEGRAM_CHAT_ID;
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("has correct manifest", () => {
    expect(manifest.name).toBe("telegram");
    expect(manifest.slot).toBe("notifier");
  });

  it("posts to the Telegram sendMessage endpoint", async () => {
    const fetchMock = okFetch();
    vi.stubGlobal("fetch", fetchMock);

    const notifier = create({ botToken: BOT_TOKEN, chatId: CHAT_ID });
    await notifier.notify(makeEvent());

    expect(fetchMock).toHaveBeenCalledOnce();
    expect(fetchMock.mock.calls[0][0]).toBe(
      `https://api.telegram.org/bot${BOT_TOKEN}/sendMessage`,
    );
    const body = lastBody(fetchMock);
    expect(body.chat_id).toBe(CHAT_ID);
    expect(body.parse_mode).toBe("HTML");
    expect(String(body.text)).toContain("Agent needs your decision");
    expect(body.reply_markup).toBeUndefined();
  });

  it("no-ops without botToken or chatId", async () => {
    const fetchMock = okFetch();
    vi.stubGlobal("fetch", fetchMock);
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});

    const notifier = create({});
    await notifier.notify(makeEvent());

    expect(fetchMock).not.toHaveBeenCalled();
    expect(warn).toHaveBeenCalled();
  });

  it("reads token and chat id from the environment", async () => {
    const fetchMock = okFetch();
    vi.stubGlobal("fetch", fetchMock);
    process.env.TELEGRAM_BOT_TOKEN = BOT_TOKEN;
    process.env.TELEGRAM_CHAT_ID = CHAT_ID;

    const notifier = create({});
    await notifier.notify(makeEvent());

    expect(fetchMock).toHaveBeenCalledOnce();
    expect(fetchMock.mock.calls[0][0]).toContain(BOT_TOKEN);
  });

  it("escapes HTML-sensitive characters in the message", async () => {
    const fetchMock = okFetch();
    vi.stubGlobal("fetch", fetchMock);

    const notifier = create({ botToken: BOT_TOKEN, chatId: CHAT_ID });
    await notifier.notify(makeEvent({ message: "a <b> & c > d" }));

    const text = String(lastBody(fetchMock).text);
    expect(text).toContain("a &lt;b&gt; &amp; c &gt; d");
  });

  it("renders inline keyboard buttons with absolute callback URLs", async () => {
    const fetchMock = okFetch();
    vi.stubGlobal("fetch", fetchMock);

    const notifier = create({ botToken: BOT_TOKEN, chatId: CHAT_ID, callbackBaseUrl: BASE_URL });
    await notifier.notifyWithActions!(makeEvent(), CALLBACK_ACTIONS);

    const markup = lastBody(fetchMock).reply_markup as {
      inline_keyboard: { text: string; url: string }[][];
    };
    const flat = markup.inline_keyboard.flat();
    expect(flat).toEqual([
      { text: "Approve", url: `${BASE_URL}/api/notify-callback/tok-approve` },
      { text: "Deny", url: `${BASE_URL}/api/notify-callback/tok-deny` },
      { text: "View PR", url: "https://github.com/acme/x/pull/7" },
    ]);
  });

  it("lays out action buttons two per row", async () => {
    const fetchMock = okFetch();
    vi.stubGlobal("fetch", fetchMock);

    const notifier = create({ botToken: BOT_TOKEN, chatId: CHAT_ID, callbackBaseUrl: BASE_URL });
    await notifier.notifyWithActions!(makeEvent(), CALLBACK_ACTIONS);

    const markup = lastBody(fetchMock).reply_markup as { inline_keyboard: unknown[][] };
    expect(markup.inline_keyboard[0]).toHaveLength(2);
  });

  it("omits callback buttons when no callbackBaseUrl is set (keeps url buttons)", async () => {
    const fetchMock = okFetch();
    vi.stubGlobal("fetch", fetchMock);
    vi.spyOn(console, "warn").mockImplementation(() => {});

    const notifier = create({ botToken: BOT_TOKEN, chatId: CHAT_ID });
    await notifier.notifyWithActions!(makeEvent(), CALLBACK_ACTIONS);

    const markup = lastBody(fetchMock).reply_markup as {
      inline_keyboard: { text: string; url: string }[][];
    };
    const flat = markup.inline_keyboard.flat();
    expect(flat).toEqual([{ text: "View PR", url: "https://github.com/acme/x/pull/7" }]);
  });

  it("sends no reply_markup when there are no renderable buttons", async () => {
    const fetchMock = okFetch();
    vi.stubGlobal("fetch", fetchMock);
    vi.spyOn(console, "warn").mockImplementation(() => {});

    const notifier = create({ botToken: BOT_TOKEN, chatId: CHAT_ID });
    await notifier.notifyWithActions!(makeEvent(), [
      { label: "Approve", callbackEndpoint: "/api/notify-callback/tok" },
    ]);

    expect(lastBody(fetchMock).reply_markup).toBeUndefined();
  });

  it("trims a trailing slash from callbackBaseUrl", async () => {
    const fetchMock = okFetch();
    vi.stubGlobal("fetch", fetchMock);

    const notifier = create({
      botToken: BOT_TOKEN,
      chatId: CHAT_ID,
      callbackBaseUrl: `${BASE_URL}/`,
    });
    await notifier.notifyWithActions!(makeEvent(), [
      { label: "Approve", callbackEndpoint: "/api/notify-callback/tok" },
    ]);

    const markup = lastBody(fetchMock).reply_markup as {
      inline_keyboard: { text: string; url: string }[][];
    };
    expect(markup.inline_keyboard.flat()[0].url).toBe(`${BASE_URL}/api/notify-callback/tok`);
  });

  it("throws on a non-ok Telegram response", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValue({ ok: false, status: 400, text: async () => "bad request" });
    vi.stubGlobal("fetch", fetchMock);

    const notifier = create({ botToken: BOT_TOKEN, chatId: CHAT_ID });
    await expect(notifier.notify(makeEvent())).rejects.toThrow(/Telegram sendMessage failed \(400\)/);
  });
});
