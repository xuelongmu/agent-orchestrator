import { describe, expect, it } from "vitest";
import { formatPtyOutputTail } from "../pty-host.js";

describe("formatPtyOutputTail", () => {
  it("includes the live Windows PTY partial editor line in captured output", () => {
    const partialLine = "› [Pasted Content 7096 chars]";

    expect(formatPtyOutputTail(["Codex ready\n"], partialLine, 50)).toBe(
      `Codex ready\n${partialLine}`,
    );
    expect(formatPtyOutputTail(["Codex ready\n"], partialLine, 1)).toBe(partialLine);
  });
});
