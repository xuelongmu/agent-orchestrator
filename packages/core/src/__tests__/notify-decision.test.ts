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
  clearSpentDecision,
  clearedDecisionMetadata,
  consumeDecision,
  decisionEpisodeTransition,
  isNudgeBlocked,
  isResolvingCallbackAction,
  releaseDecision,
  storedDecisionId,
  NOTIFY_DECISION_METADATA_KEYS,
} from "../notify-decision.js";
import { AGENT_REPORT_METADATA_KEYS } from "../agent-report.js";
import { mutateMetadata, readMetadataRaw } from "../metadata.js";
import type { Session } from "../types.js";

const SESSION_ID = "ao-5";
const PROJECT_ID = "ao";

/** The decision instance the seeded session is parked on. */
const STORED_AT = "2026-07-17T08:00:00.000Z";
const STORED_EPISODE = "2026-07-17T08:00:05.000Z";
const STORED_ID = `${STORED_AT}:${STORED_EPISODE}`;

/**
 * Give the session a metadata file carrying a real decision record, as it always
 * has by the time a callback can reach it: the route only consumes after `get`
 * resolved the session from this very file, and consumption re-checks the stored
 * identity under the lock. `consumeDecision` deliberately never creates the file —
 * that would fabricate metadata for a session that does not exist.
 */
function seedSessionMetadata(
  dir: string,
  record: { at?: string; episodeAt?: string; state?: string } = {},
): void {
  const { at = STORED_AT, episodeAt = STORED_EPISODE, state = "needs_input" } = record;
  mutateMetadata(
    dir,
    SESSION_ID,
    (existing) => ({
      ...existing,
      sessionName: SESSION_ID,
      [AGENT_REPORT_METADATA_KEYS.STATE]: state,
      [AGENT_REPORT_METADATA_KEYS.AT]: at,
      [NOTIFY_DECISION_METADATA_KEYS.EPISODE_AT]: episodeAt,
    }),
    { createIfMissing: true },
  );
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
    expect(decisionEpisodeTransition(resumed, "2026-07-17T09:01:00.000Z")).toEqual({
      kind: "retire",
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

    const transition = decisionEpisodeTransition(justParked, "2026-07-17T10:00:00.000Z");
    expect(transition).toEqual({
      kind: "stamp",
      patch: { [NOTIFY_DECISION_METADATA_KEYS.EPISODE_AT]: "2026-07-17T10:00:00.000Z" },
    });
    const patch = transition?.kind === "stamp" ? transition.patch : {};

    const stamped = {
      ...justParked,
      metadata: { ...justParked.metadata, ...patch },
    } as unknown as Session;
    expect(activeDecisionId(stamped)).toBe(`${reportedAt}:2026-07-17T10:00:00.000Z`);
  });
});

describe("decisionEpisodeTransition", () => {
  it("stamps the episode on entry into needs_input", () => {
    const session = makeSession({ lifecycleState: "needs_input", episodeAt: null });
    expect(decisionEpisodeTransition(session, "2026-07-17T10:00:00.000Z")).toEqual({
      kind: "stamp",
      patch: { [NOTIFY_DECISION_METADATA_KEYS.EPISODE_AT]: "2026-07-17T10:00:00.000Z" },
    });
  });

  it("does not refresh the marker while the session stays parked", () => {
    // Stability is the point: a marker that moved every poll would invalidate
    // live tokens within seconds, exactly as lastTransitionAt would.
    const session = makeSession({ lifecycleState: "needs_input" });
    expect(decisionEpisodeTransition(session, "2026-07-17T10:00:00.000Z")).toBeNull();
  });

  it("clears the marker once the session leaves needs_input", () => {
    const session = makeSession({ lifecycleState: "working" });
    // Retiring is not a patch: the clear must go through the locked compare.
    expect(decisionEpisodeTransition(session, "2026-07-17T10:00:00.000Z")).toEqual({
      kind: "retire",
    });
  });

  it("is a no-op when unparked and already unmarked", () => {
    const session = makeSession({ lifecycleState: "working", episodeAt: null });
    expect(decisionEpisodeTransition(session, "2026-07-17T10:00:00.000Z")).toBeNull();
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

describe("storedDecisionId", () => {
  it("is the identity recorded in metadata", () => {
    expect(
      storedDecisionId({
        [AGENT_REPORT_METADATA_KEYS.STATE]: "needs_input",
        [AGENT_REPORT_METADATA_KEYS.AT]: STORED_AT,
        [NOTIFY_DECISION_METADATA_KEYS.EPISODE_AT]: STORED_EPISODE,
      }),
    ).toBe(STORED_ID);
  });

  it("is null without an episode, or for a non-decision report", () => {
    expect(
      storedDecisionId({
        [AGENT_REPORT_METADATA_KEYS.STATE]: "needs_input",
        [AGENT_REPORT_METADATA_KEYS.AT]: STORED_AT,
      }),
    ).toBeNull();
    expect(
      storedDecisionId({
        [AGENT_REPORT_METADATA_KEYS.STATE]: "working",
        [AGENT_REPORT_METADATA_KEYS.AT]: STORED_AT,
        [NOTIFY_DECISION_METADATA_KEYS.EPISODE_AT]: STORED_EPISODE,
      }),
    ).toBeNull();
  });

  it("agrees with activeDecisionId for a live record", () => {
    // The lock-side and session-side derivations must not drift apart while the
    // decision is live. (They diverge by design once it is not: storedDecisionId
    // omits the liveness half, which needs the enriched lifecycle.)
    const session = makeSession({
      at: STORED_AT,
      episodeAt: STORED_EPISODE,
      lifecycleState: "needs_input",
    });
    expect(storedDecisionId(session.metadata)).toBe(activeDecisionId(session));
    expect(activeDecisionId(session)).toBe(STORED_ID);
  });

  it("normalizes a non-canonical timestamp to match the signed nonce", () => {
    // readAgentReport (and so activeDecisionId / the minted nonce) normalizes the
    // stored `at` via Date.parse → toISOString. storedDecisionId must apply the
    // same rule, or a valid but non-canonical `at` would never equal the nonce and
    // every Approve/Deny/Kill would 409.
    const nonCanonical = "2026-07-17T08:00:00Z"; // no milliseconds
    const canonical = "2026-07-17T08:00:00.000Z";
    const session = makeSession({
      at: nonCanonical,
      episodeAt: STORED_EPISODE,
      lifecycleState: "needs_input",
    });
    const nonce = activeDecisionId(session);
    expect(nonce).toBe(`${canonical}:${STORED_EPISODE}`);
    expect(
      storedDecisionId({
        [AGENT_REPORT_METADATA_KEYS.STATE]: "needs_input",
        [AGENT_REPORT_METADATA_KEYS.AT]: nonCanonical,
        [NOTIFY_DECISION_METADATA_KEYS.EPISODE_AT]: STORED_EPISODE,
      }),
    ).toBe(nonce);
  });

  it("is null for an unparseable timestamp", () => {
    expect(
      storedDecisionId({
        [AGENT_REPORT_METADATA_KEYS.STATE]: "needs_input",
        [AGENT_REPORT_METADATA_KEYS.AT]: "not-a-date",
        [NOTIFY_DECISION_METADATA_KEYS.EPISODE_AT]: STORED_EPISODE,
      }),
    ).toBeNull();
  });
});

describe("isNudgeBlocked", () => {
  const ID = STORED_ID;

  it("allows a nudge for the current, unconsumed decision", () => {
    expect(isNudgeBlocked(PROJECT_ID, SESSION_ID, ID)).toBe(false);
  });

  it("blocks a nudge once a resolving action has consumed the decision", () => {
    expect(consumeDecision(PROJECT_ID, SESSION_ID, ID)).toBe(true);
    expect(isNudgeBlocked(PROJECT_ID, SESSION_ID, ID)).toBe(true);
  });

  it("blocks a nudge whose decision was superseded after the route's get()", () => {
    // A new report landed between the route reading the session and this locked
    // check, so the stored identity no longer equals the token's. A stale nudge
    // must not be delivered into the successor decision.
    seedSessionMetadata(sessionsDir, { at: "2026-07-17T08:15:00.000Z" });
    expect(isNudgeBlocked(PROJECT_ID, SESSION_ID, ID)).toBe(true);
  });

  it("blocks a nudge when the decision record is gone entirely", () => {
    seedSessionMetadata(sessionsDir, { state: "working", at: STORED_AT });
    expect(isNudgeBlocked(PROJECT_ID, SESSION_ID, ID)).toBe(true);
  });
});

describe("consumeDecision", () => {
  const ID = STORED_ID;

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

  it("refuses a claim whose decision was superseded after the caller validated it", () => {
    // The race: the route validated the nonce against the session it read, then a
    // NEW decision was reported before the claim landed. Claiming the old id here
    // would stamp it onto the new decision and dispatch into it.
    const supersededId = "2026-07-17T07:00:00.000Z:2026-07-17T07:00:01.000Z";
    expect(consumeDecision(PROJECT_ID, SESSION_ID, supersededId)).toBe(false);
    const raw = readMetadataRaw(sessionsDir, SESSION_ID);
    expect(raw?.[NOTIFY_DECISION_METADATA_KEYS.CONSUMED_ID]).toBeFalsy();
    // The live decision is untouched and still answerable.
    expect(consumeDecision(PROJECT_ID, SESSION_ID, STORED_ID)).toBe(true);
  });

  it("refuses a claim once the episode has moved on", () => {
    // Same report, new episode — the token belongs to the previous episode.
    seedSessionMetadata(sessionsDir, { episodeAt: "2026-07-17T08:30:00.000Z" });
    expect(consumeDecision(PROJECT_ID, SESSION_ID, STORED_ID)).toBe(false);
  });

  it("lets a genuinely new decision instance be claimed", () => {
    expect(consumeDecision(PROJECT_ID, SESSION_ID, ID)).toBe(true);
    // A new `ao report` writes a new instant; its identity is claimable on its own.
    const nextAt = "2026-07-17T08:10:00.000Z";
    seedSessionMetadata(sessionsDir, { at: nextAt });
    expect(consumeDecision(PROJECT_ID, SESSION_ID, `${nextAt}:${STORED_EPISODE}`)).toBe(true);
  });

  it("scopes consumption to the project, so a same-id session elsewhere is unaffected", () => {
    const other = mkdtempSync(join(tmpdir(), "ao-notify-decision-other-"));
    const first = sessionsDir;
    sessionsDir = other;
    seedSessionMetadata(other);
    sessionsDir = first;
    expect(consumeDecision(PROJECT_ID, SESSION_ID, ID)).toBe(true);
    // getProjectSessionsDir is what scopes the store; a different project resolves
    // to a different directory and therefore a different record.
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

describe("clearSpentDecision", () => {
  it("clears the whole record the poll observed and returns that patch", () => {
    const applied = clearSpentDecision(PROJECT_ID, SESSION_ID, {
      state: "needs_input",
      at: STORED_AT,
    });
    // The returned patch is the exact set of keys touched — callers mirror it.
    expect(applied).not.toBeNull();
    expect(Object.keys(applied!).sort()).toEqual(
      Object.keys(clearedDecisionMetadata()).sort(),
    );
    const raw = readMetadataRaw(sessionsDir, SESSION_ID);
    expect(raw?.[AGENT_REPORT_METADATA_KEYS.STATE]).toBeFalsy();
    expect(raw?.[AGENT_REPORT_METADATA_KEYS.AT]).toBeFalsy();
    expect(raw?.[NOTIFY_DECISION_METADATA_KEYS.EPISODE_AT]).toBeFalsy();
  });

  it("never deletes a fresher report written after the poll's snapshot", () => {
    // The race: the poll loaded the session, judged the decision spent, and only
    // then did the agent write a new `ao report`. Clearing on the stale snapshot
    // would destroy the new decision and its callback identity.
    const fresherAt = "2026-07-17T08:20:00.000Z";
    seedSessionMetadata(sessionsDir, { at: fresherAt });

    expect(
      clearSpentDecision(PROJECT_ID, SESSION_ID, { state: "needs_input", at: STORED_AT }),
    ).toBeNull();

    const raw = readMetadataRaw(sessionsDir, SESSION_ID);
    expect(raw?.[AGENT_REPORT_METADATA_KEYS.AT]).toBe(fresherAt);
    expect(raw?.[AGENT_REPORT_METADATA_KEYS.STATE]).toBe("needs_input");
    expect(storedDecisionId(raw!)).toBe(`${fresherAt}:${STORED_EPISODE}`);
  });

  it("does not clear when the reported state changed under it", () => {
    seedSessionMetadata(sessionsDir, { state: "needs_decision" });
    expect(
      clearSpentDecision(PROJECT_ID, SESSION_ID, { state: "needs_input", at: STORED_AT }),
    ).toBeNull();
    expect(readMetadataRaw(sessionsDir, SESSION_ID)?.[AGENT_REPORT_METADATA_KEYS.STATE]).toBe(
      "needs_decision",
    );
  });

  describe("observed === null (episode marker lingered, no decision seen)", () => {
    it("preserves a successor non-decision report, clearing only the identity markers", () => {
      // The agent resolved the decision with `ao report working`. The next poll
      // sees the session unparked with a leftover episode marker and retires with
      // observed=null. The successor report's state/timestamp MUST survive.
      seedSessionMetadata(sessionsDir, { state: "working", at: STORED_AT });

      const applied = clearSpentDecision(PROJECT_ID, SESSION_ID, null);
      expect(applied).not.toBeNull();
      // Only identity markers are in the patch — never the report fields.
      expect(Object.keys(applied!).sort()).toEqual(
        [
          NOTIFY_DECISION_METADATA_KEYS.CONSUMED_ID,
          NOTIFY_DECISION_METADATA_KEYS.EPISODE_AT,
        ].sort(),
      );
      const raw = readMetadataRaw(sessionsDir, SESSION_ID);
      expect(raw?.[AGENT_REPORT_METADATA_KEYS.STATE]).toBe("working");
      expect(raw?.[AGENT_REPORT_METADATA_KEYS.AT]).toBe(STORED_AT);
      expect(raw?.[NOTIFY_DECISION_METADATA_KEYS.EPISODE_AT]).toBeFalsy();
    });

    it("leaves a successor DECISION report untouched — it owns its own identity", () => {
      // A brand-new decision B was reported after the snapshot. Its episode marker
      // is its own; retirement of the old episode must not touch it.
      seedSessionMetadata(sessionsDir, {
        state: "needs_input",
        at: "2026-07-17T08:40:00.000Z",
        episodeAt: "2026-07-17T08:40:05.000Z",
      });
      expect(clearSpentDecision(PROJECT_ID, SESSION_ID, null)).toBeNull();
      const raw = readMetadataRaw(sessionsDir, SESSION_ID);
      expect(raw?.[NOTIFY_DECISION_METADATA_KEYS.EPISODE_AT]).toBe("2026-07-17T08:40:05.000Z");
    });
  });
});
