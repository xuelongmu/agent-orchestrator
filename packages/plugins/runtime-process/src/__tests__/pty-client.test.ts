import { EventEmitter } from "node:events";
import { connect, type Socket } from "node:net";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { MSG_TERMINAL_INPUT, MessageParser } from "../pty-host.js";
import { ptyHostSendMessage } from "../pty-client.js";

vi.mock("node:net", () => ({
  connect: vi.fn(),
}));

class MockSocket extends EventEmitter {
  readonly writes: Array<{ frame: Buffer; at: number }> = [];
  end = vi.fn();
  destroy = vi.fn();

  write(frame: Buffer, callback?: (err?: Error | null) => void): boolean {
    this.writes.push({ frame: Buffer.from(frame), at: Date.now() });
    callback?.(null);
    return true;
  }
}

describe("ptyHostSendMessage", () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
    vi.resetAllMocks();
  });

  it("chunks a 7 KB multiline paste and delays one separate Enter frame", async () => {
    const socket = new MockSocket();
    vi.mocked(connect).mockImplementation(() => {
      queueMicrotask(() => socket.emit("connect"));
      return socket as unknown as Socket;
    });
    const message = `${"review context\n".repeat(500)}operator prompt`;
    expect(message.length).toBeGreaterThan(7_000);

    const sending = ptyHostSendMessage("\\\\.\\pipe\\ao-pty-test", message);
    await vi.runAllTimersAsync();
    await sending;

    const inputs: Array<{ payload: string; at: number }> = [];
    for (const write of socket.writes) {
      const parser = new MessageParser((type, payload) => {
        expect(type).toBe(MSG_TERMINAL_INPUT);
        inputs.push({ payload: payload.toString("utf-8"), at: write.at });
      });
      parser.feed(write.frame);
    }

    const textFrames = inputs.slice(0, -1);
    expect(textFrames.length).toBeGreaterThan(1);
    expect(textFrames.every(({ payload }) => payload.length <= 512)).toBe(true);
    expect(textFrames.map(({ payload }) => payload).join("")).toBe(message);
    expect(inputs.at(-1)?.payload).toBe("\r");
    expect(inputs.filter(({ payload }) => payload === "\r")).toHaveLength(1);
    expect(inputs.at(-1)!.at - inputs.at(-2)!.at).toBeGreaterThanOrEqual(1_000);
    expect(socket.end).toHaveBeenCalledTimes(1);
  });
});
