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
  decisionEpisodePatch,
  isResolvingCallbackAction,
  releaseDecision,
  NOTIFY_DECISION_METADATA_KEYS,
} from "../notify-decision.js";
import { AGENT_REPORT_METADATA_KEYS } from "../agent-report.js";
import { mutateMetadata, readMetadataRaw } from "../metadata.js";
import type { Session } from "../types.js";

const SESSION_ID = "ao-5";
const PROJECT_ID = "ao";

/**
 * Give the session a metadata file, as a real session always has by the time a
 * callback can reach it: the route only consumes after `get` resolved the session
 * from this very file. `consumeDecision` deliberately does not create one — that
 * would fabricate metadata for a session that does not exist.
 */
function seedSessionMetadata(dir: string): void {
  mutateMetadata(dir, SESSION_ID, (existing) => ({ ...existing, sessionName: SESSION_ID }), {
    createIfMissing: true,
  });
}

const EPISODE_AT = "2026-07-17T09:00:00.000Z";

function makeSession(overrides: {
  at?: string;
  state?: string;
  lifecycleState?: string;
  episodeAt?: string | null;
}): Session {
  const {
    at = new Date().toISOString(),
    state = "needs_input",
    lifecycleState,
    episodeAt = EPISODE_AT,
  } = overrides;
  return {
    id: SESSION_ID,
    projectId: PROJECT_ID,
    status: "working",
    activity: "waiting_input",
    metadata: {
      [AGENT_REPORT_METADATA_KEYS.STATE]: state,
      [AGENT_REPORT_METADATA_KEYS.AT]: at,
      ...(episodeAt ? { [NOTIFY_DECISION_METADATA_KEYS.EPISODE_AT]: episodeAt } : {}),
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
  seedSessionMetadata(sessionsDir);
});

afterEach(() => {
  rmSync(sessionsDir, { recursive: true, force: true });
});

describe("activeDecisionId", () => {
  it("pairs the report instant with the episode while parked on the decision", () => {
    const session = makeSession({ at: LONG_AGO, lifecycleState: "needs_input" });
    // Still parked, so a long-pending decision keeps its identity — a human may
    // take hours to answer.
    expect(activeDecisionId(session)).toBe(`${LONG_AGO}:${EPISODE_AT}`);
  });

  it("is available for a fresh decision not yet reflected in lifecycle", () => {
    const at = new Date().toISOString();
    expect(activeDecisionId(makeSession({ at }))).toBe(`${at}:${EPISODE_AT}`);
  });

  it("is null once the decision is spent — agent resumed and the report went stale", () => {
    expect(activeDecisionId(makeSession({ at: LONG_AGO, episodeAt: null }))).toBeNull();
  });

  it("is null with no episode marker — nothing a human can answer", () => {
    expect(activeDecisionId(makeSession({ episodeAt: null }))).toBeNull();
  });

  it("is null for a non-decision report", () => {
    expect(activeDecisionId(makeSession({ state: "working" }))).toBeNull();
  });

  it("is null when the session has no report at all", () => {
    expect(activeDecisionId({ metadata: {} } as unknown as Session)).toBeNull();
  });

  it("does not let decision A's token answer a bare prompt B inside the freshness window", () => {
    // The sequence the report timestamp alone could not defend against:
    // A is reported and parks the session...
    const reportedAt = new Date().toISOString();
    const parkedOnA = makeSession({
      at: reportedAt,
      lifecycleState: "needs_input",
      episodeAt: "2026-07-17T09:00:00.000Z",
    });
    const mintedForA = activeDecisionId(parkedOnA);
    expect(mintedForA).not.toBeNull();

    // ...A is resolved outside the callback, so nothing is consumed and no new
    // report is written. The agent resumes; the poll ends the episode.
    const resumed = makeSession({
      at: reportedAt,
      lifecycleState: "working",
      episodeAt: "2026-07-17T09:00:00.000Z",
    });
    expect(decisionEpisodePatch(resumed, "2026-07-17T09:01:00.000Z")).toEqual({
      [NOTIFY_DECISION_METADATA_KEYS.EPISODE_AT]: "",
    });

    // ...then the agent stops at an unrelated bare prompt B, WELL INSIDE the
    // report's 5-minute freshness window, so A's report is still present and
    // still counts as active. B opens a new episode.
    const promptB = makeSession({
      at: reportedAt,
      lifecycleState: "needs_input",
      episodeAt: "2026-07-17T09:02:00.000Z",
    });
    expect(activeDecisionId(promptB)).not.toBe(mintedForA);
  });
});

describe("first-entry mint", () => {
  it("has an identity as soon as the episode is stamped — the first notification gets buttons", () => {
    // The poll stamps the episode after the lifecycle is committed and BEFORE
    // anything notifies. Applying that patch must be enough for the very first
    // notification of the episode to mint a nonce: if it minted without one, the
    // buttons would be missing and neither the transition (status unchanged) nor
    // the report reaction (activation identity unchanged) would fire again.
    const reportedAt = new Date().toISOString();
    const justParked = makeSession({
      at: reportedAt,
      lifecycleState: "needs_input",
      episodeAt: null,
    });
    expect(activeDecisionId(justParked)).toBeNull();

    const patch = decisionEpisodePatch(justParked, "2026-07-17T10:00:00.000Z");
    expect(patch).toEqual({
      [NOTIFY_DECISION_METADATA_KEYS.EPISODE_AT]: "2026-07-17T10:00:00.000Z",
    });

    const stamped = {
      ...justParked,
      metadata: { ...justParked.metadata, ...patch },
    } as unknown as Session;
    expect(activeDecisionId(stamped)).toBe(`${reportedAt}:2026-07-17T10:00:00.000Z`);
  });
});

describe("decisionEpisodePatch", () => {
  it("stamps the episode on entry into needs_input", () => {
    const session = makeSession({ lifecycleState: "needs_input", episodeAt: null });
    expect(decisionEpisodePatch(session, "2026-07-17T10:00:00.000Z")).toEqual({
      [NOTIFY_DECISION_METADATA_KEYS.EPISODE_AT]: "2026-07-17T10:00:00.000Z",
    });
  });

  it("does not refresh the marker while the session stays parked", () => {
    // Stability is the point: a marker that moved every poll would invalidate
    // live tokens within seconds, exactly as lastTransitionAt would.
    const session = makeSession({ lifecycleState: "needs_input" });
    expect(decisionEpisodePatch(session, "2026-07-17T10:00:00.000Z")).toBeNull();
  });

  it("clears the marker once the session leaves needs_input", () => {
    const session = makeSession({ lifecycleState: "working" });
    expect(decisionEpisodePatch(session, "2026-07-17T10:00:00.000Z")).toEqual({
      [NOTIFY_DECISION_METADATA_KEYS.EPISODE_AT]: "",
    });
  });

  it("is a no-op when unparked and already unmarked", () => {
    const session = makeSession({ lifecycleState: "working", episodeAt: null });
    expect(decisionEpisodePatch(session, "2026-07-17T10:00:00.000Z")).toBeNull();
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
    seedSessionMetadata(other);
    expect(consumeDecision(PROJECT_ID, SESSION_ID, ID)).toBe(true);
    // getProjectSessionsDir is what scopes the store; a different project resolves
    // to a different directory and therefore a different record.
    const first = sessionsDir;
    sessionsDir = other;
    try {
      expect(consumeDecision("other", SESSION_ID, ID)).toBe(true);
    } finally {
      sessionsDir = first;
      rmSync(other, { recursive: true, force: true });
    }
  });

  it("refuses when the session has no metadata — fail closed, never fabricate one", () => {
    const empty = mkdtempSync(join(tmpdir(), "ao-notify-decision-empty-"));
    const first = sessionsDir;
    sessionsDir = empty;
    try {
      // Unreachable via the route (it only consumes after `get` resolved the
      // session from this file), but must never dispatch if it ever happens.
      expect(consumeDecision(PROJECT_ID, "ghost-session", ID)).toBe(false);
    } finally {
      sessionsDir = first;
      rmSync(empty, { recursive: true, force: true });
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
