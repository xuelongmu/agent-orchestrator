/**
 * Activity JSONL log — shared utilities for agents that don't have native JSONL.
 *
 * Agents like Aider and OpenCode use this to write activity observations
 * (derived from terminal output) to `{workspacePath}/.ao/activity.jsonl`.
 * Their `getActivityState()` then reads from this file, enabling detection
 * of states like `waiting_input` and `blocked` that terminal-only parsing
 * couldn't surface through the deprecated `detectActivity()` path.
 *
 * Agents with native JSONL (Claude Code, Codex) don't use this — they read
 * richer data directly from their own session files.
 */
import { appendFile, mkdir } from "node:fs/promises";
import { join, dirname } from "node:path";
import type { ActivityState, ActivityLogEntry, ActivityDetection } from "./types.js";

/**
 * @deprecated Actionable states no longer decay on wallclock. Retained until
 * the activity-reducer cleanup removes the old activity-log module.
 */
export const ACTIVITY_INPUT_STALENESS_MS = 5 * 60 * 1000; // 5 minutes

/**
 * Get the path to the activity JSONL log for a session.
 * Location: `{workspacePath}/.ao/activity.jsonl`
 */
export function getActivityLogPath(workspacePath: string): string {
  return join(workspacePath, ".ao", "activity.jsonl");
}

/**
 * Append an activity observation to the session's JSONL log.
 * Creates the `.ao/` directory if it doesn't exist.
 */
export async function appendActivityEntry(
  workspacePath: string,
  state: ActivityState,
  source: "terminal" | "native" | "hook",
  trigger?: string,
  fingerprint?: string,
): Promise<void> {
  const logPath = getActivityLogPath(workspacePath);
  await mkdir(dirname(logPath), { recursive: true });

  const isActionable = state === "waiting_input" || state === "blocked";
  const entry: ActivityLogEntry = {
    ts: new Date().toISOString(),
    state,
    source,
    ...(trigger !== undefined && isActionable && { trigger }),
    ...(fingerprint !== undefined && isActionable && { fingerprint }),
  };

  await appendFile(logPath, JSON.stringify(entry) + "\n", "utf-8");
}

/**
 * Read the last activity entry from the session's JSONL log.
 * Returns the parsed entry with the file's modification time, or null if
 * the file doesn't exist or is empty.
 */
export async function readLastActivityEntry(
  workspacePath: string,
): Promise<{ entry: ActivityLogEntry; modifiedAt: Date } | null> {
  const logPath = getActivityLogPath(workspacePath);

  try {
    const { open } = await import("node:fs/promises");
    const handle = await open(logPath, "r");
    try {
      const fileStat = await handle.stat();
      if (fileStat.size === 0) return null;

      // Read last 4KB — more than enough for a single JSON line
      const tailSize = Math.min(fileStat.size, 4096);
      const offset = Math.max(0, fileStat.size - tailSize);
      const buffer = Buffer.alloc(tailSize);
      const { bytesRead } = await handle.read(buffer, 0, tailSize, offset);
      if (bytesRead === 0) return null;
      const content = buffer.subarray(0, bytesRead).toString("utf-8");

      // Find the last non-empty line. If we read from a non-zero offset,
      // the first line may be truncated — drop it.
      let lines = content.split("\n").filter((l) => l.trim());
      if (offset > 0 && lines.length > 1) lines = lines.slice(1);
      if (lines.length === 0) return null;

      // Try lines from the end — skip any that fail to parse (e.g. truncated)
      let parsed: unknown = null;
      for (let i = lines.length - 1; i >= 0; i--) {
        try {
          parsed = JSON.parse(lines[i]!);
          break;
        } catch {
          continue;
        }
      }
      if (parsed === null) return null;
      if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) return null;

      const record = parsed as Record<string, unknown>;
      const validStates = new Set(["active", "ready", "idle", "waiting_input", "blocked", "exited"]);
      const validSources = new Set(["terminal", "native", "hook"]);
      if (
        typeof record.ts !== "string" ||
        typeof record.state !== "string" ||
        typeof record.source !== "string" ||
        !validStates.has(record.state) ||
        !validSources.has(record.source)
      ) {
        return null;
      }

      const entry: ActivityLogEntry = {
        ts: record.ts,
        state: record.state as ActivityLogEntry["state"],
        source: record.source as ActivityLogEntry["source"],
        ...(typeof record.trigger === "string" && { trigger: record.trigger }),
        ...(typeof record.fingerprint === "string" && { fingerprint: record.fingerprint }),
      };
      return { entry, modifiedAt: fileStat.mtime };
    } finally {
      await handle.close();
    }
  } catch {
    return null;
  }
}

/**
 * Check the AO activity JSONL for actionable states only.
 *
 * Only returns `waiting_input`/`blocked`.
 * Non-critical states (`active`, `ready`, `idle`) always return `null` so
 * callers fall through to their native signals (git commits, chat history,
 * API queries, native JSONL). This prevents the lifecycle manager's
 * `recordActivity` writes (which refresh `mtime` every poll cycle) from
 * shadowing those richer detection methods and breaking stuck-detection.
 */
export function checkActivityLogState(
  activityResult: { entry: ActivityLogEntry; modifiedAt: Date } | null,
): ActivityDetection | null {
  if (!activityResult) return null;

  const { entry } = activityResult;

  if (entry.state === "waiting_input" || entry.state === "blocked") {
    const entryTs = new Date(entry.ts);
    if (Number.isNaN(entryTs.getTime())) return null;
    return { state: entry.state, timestamp: entryTs };
  }

  // Non-critical states fall through to native signals
  return null;
}

/**
 * Derive an activity state from the JSONL entry with age-based decay.
 *
 * Unlike `checkActivityLogState` (which only returns actionable states),
 * this returns any state — but reclassifies `active`/`ready` entries as
 * `ready`/`idle` if they've aged past the active window / threshold.
 * Use this as a last-resort fallback when native signals are unavailable.
 */
export function getActivityFallbackState(
  activityResult: { entry: ActivityLogEntry; modifiedAt: Date } | null,
  activeWindowMs: number,
  thresholdMs: number,
): ActivityDetection | null {
  if (!activityResult) return null;

  const { entry } = activityResult;
  const entryTs = new Date(entry.ts);
  if (Number.isNaN(entryTs.getTime())) return null;

  if (entry.state === "waiting_input" || entry.state === "blocked") {
    return { state: entry.state, timestamp: entryTs };
  }

  // Age-based decay: active→ready→idle, but never promote past the
  // entry's detected state (e.g. a fresh "idle" entry stays "idle").
  const ageMs = Math.max(0, Date.now() - entryTs.getTime());
  let ageState: ActivityState;
  if (ageMs <= activeWindowMs) ageState = "active";
  else if (ageMs <= thresholdMs) ageState = "ready";
  else ageState = "idle";

  const activityRank: Record<string, number> = { active: 0, ready: 1, idle: 2 };
  const entryRank = activityRank[entry.state] ?? 2;
  const ageRank = activityRank[ageState] ?? 2;
  const finalState = ageRank >= entryRank ? ageState : entry.state;

  return { state: finalState, timestamp: entryTs };
}

/**
 * Build the arguments for `appendActivityEntry` from terminal output.
 *
 * Classifies terminal output via the provided `detectActivity` function and
 * returns the state + trigger. Plugins call `appendActivityEntry` themselves
 * (keeping it mockable in tests).
 */
export function classifyTerminalActivity(
  terminalOutput: string,
  detectActivity: (output: string) => ActivityState,
): { state: ActivityState; trigger: string | undefined; fingerprint: string | undefined } {
  const state = detectActivity(terminalOutput);
  const isActionable = state === "waiting_input" || state === "blocked";
  const trigger = isActionable
    ? terminalOutput.trim().split("\n").slice(-3).join("\n")
    : undefined;
  // Fingerprint the FULL terminal context, not just the prompt's last lines. Two
  // text-identical prompts share a `trigger`, so trigger alone can never tell a
  // repeated observation of one live prompt from an identical prompt reached after
  // intervening work. The wider context differs once the agent has produced output
  // between them, so the fingerprint advances the boundary for the genuinely new
  // prompt while staying stable for a static blocked screen. (round 18 review)
  const fingerprint = isActionable ? fingerprintTerminalContext(terminalOutput) : undefined;
  return { state, trigger, fingerprint };
}

/**
 * Deterministic FNV-1a hash of the trimmed terminal context, as a compact hex
 * string. Pure (no Date/random), so the same screen always fingerprints
 * identically across polls.
 */
function fingerprintTerminalContext(terminalOutput: string): string {
  const s = terminalOutput.trim();
  let h = 0x811c9dc5;
  for (let i = 0; i < s.length; i++) {
    h ^= s.charCodeAt(i);
    h = Math.imul(h, 0x01000193);
  }
  return (h >>> 0).toString(16);
}

/**
 * Shared `recordActivity` implementation for all agents.
 *
 * Classifies terminal output, deduplicates writes, and appends to the JSONL.
 *
 * Two dedup rules, keyed on whether the state is actionable:
 *  - Non-actionable (active/ready/idle/exited): skip when the state hasn't
 *    changed and the last entry is recent (<20s) — pure I/O reduction.
 *  - Actionable (waiting_input/blocked): skip when the state AND the terminal-
 *    context fingerprint are both unchanged. A repeated observation of the SAME
 *    live prompt every poll must NOT re-append, because the entry `ts` is what
 *    the durable episode boundary reads (via `getActivityState`): advancing it
 *    each poll makes a genuinely-blocked decision look like a fresh re-park and
 *    retires a live decision (#13 review). The fingerprint hashes the broader
 *    terminal context, so a text-identical prompt reached after intervening work
 *    fingerprints differently and DOES append — giving the genuinely new prompt
 *    its own boundary — while a static blocked screen stays stable. A state change
 *    always appends. (round 18 review)
 */
export async function recordTerminalActivity(
  workspacePath: string,
  terminalOutput: string,
  detectActivity: (output: string) => ActivityState,
): Promise<void> {
  const { state, trigger, fingerprint } = classifyTerminalActivity(terminalOutput, detectActivity);
  const isActionable = state === "waiting_input" || state === "blocked";

  const lastEntry = await readLastActivityEntry(workspacePath);
  if (lastEntry && lastEntry.entry.state === state) {
    if (isActionable) {
      // Same actionable state on the same terminal context = a repeated
      // observation, not a new boundary. The fingerprint covers the whole screen,
      // so a resolved-then-reparked (or intervening-work-then-identical) prompt
      // hashes differently and appends anew. When the last entry predates
      // fingerprints (legacy, trigger-only) fall back to comparing triggers, so an
      // upgrade doesn't spuriously append and retire a still-live decision.
      const lastKey =
        lastEntry.entry.fingerprint !== undefined
          ? lastEntry.entry.fingerprint
          : (lastEntry.entry.trigger ?? undefined);
      const currentKey =
        lastEntry.entry.fingerprint !== undefined ? fingerprint : (trigger ?? undefined);
      if (lastKey === currentKey) return;
    } else {
      const entryAgeMs = Date.now() - lastEntry.modifiedAt.getTime();
      if (entryAgeMs < 20_000) return;
    }
  }

  await appendActivityEntry(workspacePath, state, "terminal", trigger, fingerprint);
}
