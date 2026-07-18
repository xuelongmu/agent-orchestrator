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
  classifyCurrentActivity,
  clearSpentDecision,
  clearedDecisionMetadata,
  consumeDecision,
  decisionEpisodeTransition,
  isDecisionCallbackAnswerable,
  isNudgeBlocked,
  isResolvingCallbackAction,
  storedDecisionId,
  NOTIFY_DECISION_METADATA_KEYS,
} from "../notify-decision.js";
import { AGENT_REPORT_METADATA_KEYS } from "../agent-report.js";
import { mutateMetadata, readMetadataRaw } from "../metadata.js";
import type { ActivitySignal, ActivitySignalState, ActivityState, Session } from "../types.js";

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
  activity?: string;
  activitySignal?: ActivitySignal;
}): Session {
  const {
    at = new Date().toISOString(),
    state = "needs_input",
    lifecycleState,
    episodeAt = EPISODE_AT,
    activity = "waiting_input",
    activitySignal,
  } = overrides;
  return {
    id: SESSION_ID,
    projectId: PROJECT_ID,
    status: "working",
    activity,
    ...(activitySignal ? { activitySignal } : {}),
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

/** A structured activity signal for tests. */
function signal(state: ActivitySignalState, activity: ActivityState | null): ActivitySignal {
  return { state, activity, source: "native" };
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

  it("is null when live activity is active — work resumed before the poll persisted it", () => {
    // Stale canonical still says needs_input and the nonce still matches, but the
    // agent has resumed (activity active), so no destructive callback may land.
    const at = new Date().toISOString();
    expect(
      activeDecisionId(makeSession({ at, lifecycleState: "needs_input", activity: "active" })),
    ).toBeNull();
  });

  it("stays answerable for parked/ready/idle activity", () => {
    const at = new Date().toISOString();
    for (const activity of ["waiting_input", "ready", "idle"]) {
      expect(activeDecisionId(makeSession({ at, activity }))).toBe(`${at}:${EPISODE_AT}`);
    }
  });

  it("suppresses only for a VALID active signal — a stale active signal keeps controls", () => {
    // Finding #1 (round 18): a valid activitySignal saying "active" is authoritative
    // (agent demonstrably resumed) and strips controls. A STALE active signal is
    // weak evidence and must NOT strip controls from a fresh decision report.
    const at = new Date().toISOString();
    expect(
      activeDecisionId(makeSession({ at, activitySignal: signal("valid", "active") })),
    ).toBeNull();
    expect(
      activeDecisionId(makeSession({ at, activitySignal: signal("stale", "active") })),
    ).toBe(`${at}:${EPISODE_AT}`);
  });

  it("prefers the current valid signal over a stale persisted active activity", () => {
    // Persisted session.activity lags at "active", but the fresh signal says the
    // agent is parked at the prompt — the decision stays answerable.
    const at = new Date().toISOString();
    expect(
      activeDecisionId(
        makeSession({ at, activity: "active", activitySignal: signal("valid", "waiting_input") }),
      ),
    ).toBe(`${at}:${EPISODE_AT}`);
  });

  it("falls back to persisted activity when there is no structured signal", () => {
    // No activitySignal at all (e.g. a metadata-only reconstruction): the safe
    // pre-signal behaviour — persisted active suppresses, persisted waiting keeps.
    const at = new Date().toISOString();
    expect(activeDecisionId(makeSession({ at, activity: "active" }))).toBeNull();
    expect(activeDecisionId(makeSession({ at, activity: "waiting_input" }))).toBe(
      `${at}:${EPISODE_AT}`,
    );
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
    expect(decisionEpisodeTransition(resumed, null, "2026-07-17T09:01:00.000Z")).toEqual({
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

    const transition = decisionEpisodeTransition(justParked, null, "2026-07-17T10:00:00.000Z");
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
    expect(decisionEpisodeTransition(session, null, "2026-07-17T10:00:00.000Z")).toEqual({
      kind: "stamp",
      patch: { [NOTIFY_DECISION_METADATA_KEYS.EPISODE_AT]: "2026-07-17T10:00:00.000Z" },
    });
  });

  it("stamps with the durable agent boundary when one is available", () => {
    // The marker is bound to the agent's own last-activity instant, not poll time,
    // so the between-polls advance check below compares like against like.
    const session = makeSession({ lifecycleState: "needs_input", episodeAt: null });
    expect(
      decisionEpisodeTransition(session, "2026-07-17T09:59:50.000Z", "2026-07-17T10:00:00.000Z"),
    ).toEqual({
      kind: "stamp",
      patch: { [NOTIFY_DECISION_METADATA_KEYS.EPISODE_AT]: "2026-07-17T09:59:50.000Z" },
    });
  });

  it("does not refresh the marker while the session stays parked", () => {
    // Stability is the point: a marker that moved every poll would invalidate
    // live tokens within seconds, exactly as lastTransitionAt would. A boundary
    // that has NOT advanced past the marker (agent still blocked at the prompt)
    // is a no-op.
    const session = makeSession({ lifecycleState: "needs_input", episodeAt: EPISODE_AT });
    expect(decisionEpisodeTransition(session, EPISODE_AT, "2026-07-17T10:00:00.000Z")).toBeNull();
    expect(decisionEpisodeTransition(session, null, "2026-07-17T10:00:00.000Z")).toBeNull();
  });

  it("retires the spent record when the agent boundary advances past the marker (between-polls re-park)", () => {
    // A resolves and the agent re-parks on an unrelated bare prompt B entirely
    // within one poll interval: both snapshots read needs_input, so only the
    // advancing activity boundary reveals that the agent left A's prompt. Retiring
    // clears A's report/episode so A's still-live token cannot answer B.
    const session = makeSession({ lifecycleState: "needs_input", episodeAt: EPISODE_AT });
    const advanced = new Date(Date.parse(EPISODE_AT) + 30_000).toISOString();
    expect(decisionEpisodeTransition(session, advanced, "2026-07-17T10:00:00.000Z")).toEqual({
      kind: "retire",
    });
  });

  it("ignores a boundary at or before the marker, or an unparseable one", () => {
    const session = makeSession({ lifecycleState: "needs_input", episodeAt: EPISODE_AT });
    const before = new Date(Date.parse(EPISODE_AT) - 30_000).toISOString();
    expect(decisionEpisodeTransition(session, before, "2026-07-17T10:00:00.000Z")).toBeNull();
    expect(decisionEpisodeTransition(session, "not-a-date", "2026-07-17T10:00:00.000Z")).toBeNull();
  });

  it("restamps (does NOT retire) when a FRESH successor report B already replaced A", () => {
    // Finding #4 (round 18): the boundary advanced past A's marker, but the metadata
    // already holds a fresh successor decision report B — written after A's episode
    // and by the time the agent re-parked. B is a new decision; retiring it would
    // lose it and its callback identity. Restamp the episode to the advanced
    // boundary so B keeps a live identity.
    const boundaryB = new Date(Date.parse(EPISODE_AT) + 5 * 60_000).toISOString();
    const reportB = new Date(Date.parse(EPISODE_AT) + 4 * 60_000).toISOString();
    const session = makeSession({
      lifecycleState: "needs_input",
      episodeAt: EPISODE_AT,
      at: reportB,
    });
    expect(decisionEpisodeTransition(session, boundaryB, "2026-07-17T10:00:00.000Z")).toEqual({
      kind: "stamp",
      patch: { [NOTIFY_DECISION_METADATA_KEYS.EPISODE_AT]: boundaryB },
    });
  });

  it("still retires when the boundary advances over a bare prompt with only A's spent report", () => {
    // A's own report was written at/before A parked (here, long before the marker),
    // so it is not a successor: a bare prompt B carrying only A's spent report must
    // retire, exactly as before. (round 18)
    const boundaryB = new Date(Date.parse(EPISODE_AT) + 5 * 60_000).toISOString();
    const session = makeSession({
      lifecycleState: "needs_input",
      episodeAt: EPISODE_AT,
      at: LONG_AGO,
    });
    expect(decisionEpisodeTransition(session, boundaryB, "2026-07-17T10:00:00.000Z")).toEqual({
      kind: "retire",
    });
  });

  it("clears the marker once the session leaves needs_input", () => {
    const session = makeSession({ lifecycleState: "working" });
    // Retiring is not a patch: the clear must go through the locked compare.
    expect(decisionEpisodeTransition(session, null, "2026-07-17T10:00:00.000Z")).toEqual({
      kind: "retire",
    });
  });

  it("is a no-op when unparked and already unmarked", () => {
    const session = makeSession({ lifecycleState: "working", episodeAt: null });
    expect(decisionEpisodeTransition(session, null, "2026-07-17T10:00:00.000Z")).toBeNull();
  });
});

describe("classifyCurrentActivity / isDecisionCallbackAnswerable", () => {
  // The dispatch-time gate for mutating callbacks (findings #2 & #5): only a
  // positively-verified answerable activity authorizes; everything else fails
  // CLOSED.
  it("is answerable only for a VALID parked/idle/ready signal", () => {
    for (const activity of ["waiting_input", "idle", "ready"] as const) {
      const session = makeSession({ activitySignal: signal("valid", activity) });
      expect(classifyCurrentActivity(session)).toBe("answerable");
      expect(isDecisionCallbackAnswerable(session)).toBe(true);
    }
  });

  it("rejects a verified active (resumed) agent", () => {
    const session = makeSession({ activitySignal: signal("valid", "active") });
    expect(classifyCurrentActivity(session)).toBe("resumed");
    expect(isDecisionCallbackAnswerable(session)).toBe(false);
  });

  it("rejects a verified blocked (error/stuck) agent", () => {
    const session = makeSession({ activitySignal: signal("valid", "blocked") });
    expect(classifyCurrentActivity(session)).toBe("blocked");
    expect(isDecisionCallbackAnswerable(session)).toBe(false);
  });

  it("fails closed when current activity could not be verified/refreshed", () => {
    // Stale/unavailable/null/probe-failure signals, an exited process, and a
    // metadata-only session with no structured signal all fail closed.
    for (const s of [
      signal("stale", "waiting_input"),
      signal("unavailable", null),
      signal("null", null),
      signal("probe_failure", null),
      signal("valid", "exited"),
    ]) {
      const session = makeSession({ activitySignal: s });
      expect(classifyCurrentActivity(session)).toBe("unverifiable");
      expect(isDecisionCallbackAnswerable(session)).toBe(false);
    }
    const noSignal = makeSession({ activity: "waiting_input" });
    expect(classifyCurrentActivity(noSignal)).toBe("unverifiable");
    expect(isDecisionCallbackAnswerable(noSignal)).toBe(false);
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

  it("stays claimed after a failed dispatch — at-most-once holds across retries", () => {
    // The route no longer reopens a consumed claim on send failure (delivery may
    // have already crossed the boundary), so a retry with the same id is refused.
    expect(consumeDecision(PROJECT_ID, SESSION_ID, ID)).toBe(true);
    expect(consumeDecision(PROJECT_ID, SESSION_ID, ID)).toBe(false);
  });
});

describe("clearSpentDecision", () => {
  it("clears the whole record the poll observed and returns that patch", () => {
    const applied = clearSpentDecision(PROJECT_ID, SESSION_ID, {
      state: "needs_input",
      at: STORED_AT,
    });
    // The returned patch is the exact set of keys touched — the cleared decision
    // fields plus the backfilled acknowledgement marker — so callers mirror it.
    expect(applied).not.toBeNull();
    expect(Object.keys(applied!).sort()).toEqual(
      [
        ...Object.keys(clearedDecisionMetadata()),
        AGENT_REPORT_METADATA_KEYS.ACKNOWLEDGED_AT,
      ].sort(),
    );
    const raw = readMetadataRaw(sessionsDir, SESSION_ID);
    expect(raw?.[AGENT_REPORT_METADATA_KEYS.STATE]).toBeFalsy();
    expect(raw?.[AGENT_REPORT_METADATA_KEYS.AT]).toBeFalsy();
    expect(raw?.[NOTIFY_DECISION_METADATA_KEYS.EPISODE_AT]).toBeFalsy();
  });

  it("backfills the acknowledgement marker from a pre-upgrade report before clearing it", () => {
    // A session restored across an upgrade carries a report but no marker. Clearing
    // the report must leave durable acknowledgement evidence so the SAME poll's
    // checkAcknowledgeTimeout does not falsely fire no_acknowledge. (#13 review)
    const raw0 = readMetadataRaw(sessionsDir, SESSION_ID);
    expect(raw0?.[AGENT_REPORT_METADATA_KEYS.ACKNOWLEDGED_AT]).toBeFalsy(); // pre-upgrade: none

    clearSpentDecision(PROJECT_ID, SESSION_ID, { state: "needs_input", at: STORED_AT });

    const raw = readMetadataRaw(sessionsDir, SESSION_ID);
    expect(raw?.[AGENT_REPORT_METADATA_KEYS.STATE]).toBeFalsy(); // report cleared
    expect(raw?.[AGENT_REPORT_METADATA_KEYS.ACKNOWLEDGED_AT]).toBe(STORED_AT); // backfilled
  });

  it("preserves an existing acknowledgement marker on retirement", () => {
    const earlier = "2026-07-17T07:00:00.000Z";
    seedSessionMetadata(sessionsDir, { at: STORED_AT });
    mutateMetadata(sessionsDir, SESSION_ID, (existing) => ({
      ...existing,
      [AGENT_REPORT_METADATA_KEYS.ACKNOWLEDGED_AT]: earlier,
    }));

    clearSpentDecision(PROJECT_ID, SESSION_ID, { state: "needs_input", at: STORED_AT });

    expect(readMetadataRaw(sessionsDir, SESSION_ID)?.[AGENT_REPORT_METADATA_KEYS.ACKNOWLEDGED_AT]).toBe(
      earlier,
    );
  });

  it("clears a valid but non-canonical stored timestamp (normalized comparison)", () => {
    // The stored `at` is valid but not in toISOString() form; observed.at came
    // from readAgentReport (canonical). A raw compare would never match, so
    // retirement would no-op and a later prompt could revalidate the stale token.
    const nonCanonical = "2026-07-17T08:00:00Z"; // no milliseconds
    const canonical = "2026-07-17T08:00:00.000Z";
    seedSessionMetadata(sessionsDir, { at: nonCanonical });

    const applied = clearSpentDecision(PROJECT_ID, SESSION_ID, {
      state: "needs_input",
      at: canonical,
    });
    expect(applied).not.toBeNull();
    const raw = readMetadataRaw(sessionsDir, SESSION_ID);
    expect(raw?.[AGENT_REPORT_METADATA_KEYS.STATE]).toBeFalsy();
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
