import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

// The decision store lives in the session's own metadata. Point it at a temp
// directory so the real file-locked read-modify-write is exercised without
// touching a real project.
let sessionsDir = "";
vi.mock("../paths.js", async (importOriginal) => {
  const actual = (await importOriginal()) as Record<string, unknown>;
  return { ...actual, getProjectSessionsDir: () => sessionsDir };
});

import {
  activeDecisionId,
  consumeDecision,
  isResolvingCallbackAction,
  releaseDecision,
  NOTIFY_DECISION_METADATA_KEYS,
} from "../notify-decision.js";
import { AGENT_REPORT_METADATA_KEYS } from "../agent-report.js";
import { readMetadataRaw } from "../metadata.js";
import type { Session } from "../types.js";

const SESSION_ID = "ao-5";
const PROJECT_ID = "ao";

function makeSession(overrides: {
  at?: string;
  state?: string;
  lifecycleState?: string;
}): Session {
  const { at = new Date().toISOString(), state = "needs_input", lifecycleState } = overrides;
  return {
    id: SESSION_ID,
    projectId: PROJECT_ID,
    status: "working",
    activity: "waiting_input",
    metadata: {
      [AGENT_REPORT_METADATA_KEYS.STATE]: state,
      [AGENT_REPORT_METADATA_KEYS.AT]: at,
    },
    ...(lifecycleState
      ? {
          lifecycle: {
            session: { state: lifecycleState },
            pr: { state: "none" },
            runtime: { state: "alive" },
          },
        }
      : {}),
  } as unknown as Session;
}

/** Comfortably outside AGENT_REPORT_FRESHNESS_MS (5 min). */
const LONG_AGO = new Date(Date.now() - 60 * 60 * 1000).toISOString();

beforeEach(() => {
  sessionsDir = mkdtempSync(join(tmpdir(), "ao-notify-decision-"));
});

afterEach(() => {
  rmSync(sessionsDir, { recursive: true, force: true });
});

describe("activeDecisionId", () => {
  it("is the report timestamp while the session is parked on the decision", () => {
    const session = makeSession({ at: LONG_AGO, lifecycleState: "needs_input" });
    // Still parked, so a long-pending decision keeps its identity — a human may
    // take hours to answer.
    expect(activeDecisionId(session)).toBe(LONG_AGO);
  });

  it("is the report timestamp for a fresh decision not yet reflected in lifecycle", () => {
    const at = new Date().toISOString();
    expect(activeDecisionId(makeSession({ at }))).toBe(at);
  });

  it("is null once the decision is spent — agent resumed and the report went stale", () => {
    // Decision A resolved without a new report: the agent is working again and
    // the report has aged out. Its identity must not survive to authorise a token
    // against some later, unrelated prompt.
    expect(activeDecisionId(makeSession({ at: LONG_AGO }))).toBeNull();
  });

  it("is null for a non-decision report", () => {
    expect(activeDecisionId(makeSession({ state: "working" }))).toBeNull();
  });

  it("is null when the session has no report at all", () => {
    expect(activeDecisionId({ metadata: {} } as unknown as Session)).toBeNull();
  });
});

describe("isResolvingCallbackAction", () => {
  it("treats approve/deny/kill as resolving", () => {
    expect(isResolvingCallbackAction("approve")).toBe(true);
    expect(isResolvingCallbackAction("deny")).toBe(true);
    expect(isResolvingCallbackAction("kill")).toBe(true);
  });

  it("does not treat nudge as resolving — it leaves the choice outstanding", () => {
    expect(isResolvingCallbackAction("nudge")).toBe(false);
  });
});

describe("consumeDecision", () => {
  const ID = "2026-07-17T00:00:00.000Z";

  it("claims once and refuses every later attempt on the same instance", () => {
    expect(consumeDecision(PROJECT_ID, SESSION_ID, ID)).toBe(true);
    expect(consumeDecision(PROJECT_ID, SESSION_ID, ID)).toBe(false);
    expect(consumeDecision(PROJECT_ID, SESSION_ID, ID)).toBe(false);
  });

  it("persists consumption to metadata, so a restart cannot re-dispatch", () => {
    expect(consumeDecision(PROJECT_ID, SESSION_ID, ID)).toBe(true);
    // Read back through a fresh metadata read — nothing in-process is consulted.
    const raw = readMetadataRaw(sessionsDir, SESSION_ID);
    expect(raw?.[NOTIFY_DECISION_METADATA_KEYS.CONSUMED_ID]).toBe(ID);
    expect(consumeDecision(PROJECT_ID, SESSION_ID, ID)).toBe(false);
  });

  it("lets a different decision instance be claimed independently", () => {
    expect(consumeDecision(PROJECT_ID, SESSION_ID, ID)).toBe(true);
    expect(consumeDecision(PROJECT_ID, SESSION_ID, "2026-07-17T01:00:00.000Z")).toBe(true);
  });

  it("scopes consumption to the project, so a same-id session elsewhere is unaffected", () => {
    const other = mkdtempSync(join(tmpdir(), "ao-notify-decision-other-"));
    expect(consumeDecision(PROJECT_ID, SESSION_ID, ID)).toBe(true);
    // getProjectSessionsDir is what scopes the store; a different project resolves
    // to a different directory and therefore a different record.
    sessionsDir = other;
    try {
      expect(consumeDecision("other", SESSION_ID, ID)).toBe(true);
    } finally {
      rmSync(other, { recursive: true, force: true });
    }
  });

  it("releases only the named instance so a failed dispatch is retryable", () => {
    expect(consumeDecision(PROJECT_ID, SESSION_ID, ID)).toBe(true);
    releaseDecision(PROJECT_ID, SESSION_ID, ID);
    expect(consumeDecision(PROJECT_ID, SESSION_ID, ID)).toBe(true);
  });

  it("release is a no-op for an instance that is not the consumed one", () => {
    expect(consumeDecision(PROJECT_ID, SESSION_ID, ID)).toBe(true);
    releaseDecision(PROJECT_ID, SESSION_ID, "some-other-decision");
    // The real claim must survive a release aimed at a different instance.
    expect(consumeDecision(PROJECT_ID, SESSION_ID, ID)).toBe(false);
  });
});
