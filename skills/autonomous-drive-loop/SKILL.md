---
name: autonomous-drive-loop
description: Run a long-lived orchestration loop that drives PRs through an automated reviewer to merge — reviewer-signal handling, merge gates, anti-treadmill review policy, and state-file discipline for self-scheduled agents.
trigger: An agent (or human operator) is asked to autonomously drive one or more PRs through a bot-review→fix→merge cycle over hours or days, especially via self-scheduled wake-ups.
---

# Autonomous Drive-Loop Skill

How to run a multi-day "drive PRs to merge" loop without the failure modes we hit dogfooding AO on itself (2026-06/07): half-day stalls from missed reviewer signals, nine-round patch treadmills, and state corrupted by the loop's own prompts.

## 1. The three-store discipline (never keep state in prompts)

A recurring loop touches three kinds of content with opposite lifecycles. Keep them in three places:

| Content | Store | Write discipline |
|---|---|---|
| **Policy** — merge bar, safety rules, do-not-touch list | A versioned file (this skill / a project HANDOFF doc) | Edited deliberately and rarely; never rewritten per-cycle |
| **Volatile state** — decisions made, dispatch log, finding ledger, current phase | A machine-written `STATE.json` (see `STATE.template.json`) | Updated ONLY from command output, never from recollection |
| **Everything derivable** — CI status, HEAD SHA, review lists, session states | Nowhere. | Re-derive fresh every cycle from `gh` / the API |

**Why:** carrying state inside a self-scheduled prompt mutates it by paraphrase — a lossy copy with no integrity checks. Observed: round counts drifted, and one loop prompt recorded a deliverable as "done" that was never sent; the next cycle believed its own forged record.

The wake-up prompt itself stays ~5 lines of **pointers, not values**: "read policy file + STATE.json, run the cycle checklist, update STATE.json from command output, deliver anything owed to the user, reschedule with this same prompt."

## 2. The cycle checklist

1. **Health first.** Verify the orchestrator daemon answers (`GET /api/sessions` → 200). If down, restart per runbook, then re-adopt/free orphaned worktrees BEFORE any `--claim-pr` (stale worktrees holding PR branches cause junk-branch fallbacks).
2. **Derive fresh.** PR **state (open/merged/closed) first**, then HEAD, CI rollup, mergeable, review threads, reviewer verdicts — from source, every cycle. Never trust a carried value. (A merged PR often shows `mergeable: UNKNOWN` — checking HEAD/CI without checking state once caused a restructure to be dispatched against an already-merged PR.)
3. **Act per policy** (sections 3–5 below).
4. **Update STATE.json** from what commands actually returned.
5. **Deliver.** If anything is owed to a human (answer, summary, decision), it goes in this turn's output. **Never end a cycle that owes a deliverable with only a reschedule.**
6. **Reschedule** with the same slim prompt (short interval while active; long interval while blocked on an external party).

Expect gaps: self-scheduled wake-ups don't fire while the host machine sleeps. On wake, reconstruct from source + STATE.json — never from stale recall.

## 3. Reviewer-bot signal handling (worked example: Codex)

The reviewer bot (`chatgpt-codex-connector[bot]`) never submits a formal GitHub `APPROVED` review. Treat ANY of these as approval — but **every signal must be bound to the current HEAD** before it satisfies the gate:

- a body containing **"Didn't find any major issues"** / **"Chef's kiss"** whose `Reviewed commit:` equals the current HEAD;
- a review of the current HEAD with **zero new inline findings**;
- a **👍 / +1 reaction** by the bot — reactions carry no commit reference, so a reaction counts ONLY if no push has occurred since the reaction's `created_at` (compare against the HEAD commit's `committedDate`). A 👍 left on an older head does not approve the commits pushed after it.

Hard-won gotchas (each cost a real half-day stall):

1. **Check BOTH channels.** Clean verdicts can post as **issue comments**, not review submissions. Poll `pulls/N/reviews` AND `issues/N/comments` every cycle.
2. **Verify `Reviewed commit == current HEAD`** before trusting any verdict — the bot auto-reviews on push and can lag the head.
3. **Count ≠ content.** Read finding **bodies** each cycle. A large unresolved-thread count is usually stale bookkeeping (bots don't click "Resolve") — a fresh clean verdict on the exact HEAD supersedes it. Conversely, an unchanged count can hide N-resolved/N-new churn.
4. **👀 = looking, not approval.** If the bot reacts 👀 but posts nothing for ~20–24 min, re-trigger (`@codex review`) automatically — don't ask the human.
5. **Quota exhaustion is a distinct state.** A "you have reached your usage limits" comment means the reviewer is capped: back off to long intervals, notify the human (credits needed), and re-trigger once topped up. Don't silently wait.

## 4. Merge gate

Merge only when **(reviewer approval signal on current HEAD OR explicit human approval) AND CI fully green AND mergeable**. Then squash-merge, delete the branch, fast-forward local main, and advance to the next roadmap item. Keep a do-not-touch list (e.g. release-bot PRs) in policy. If a safety layer blocks a merge, surface it — never work around.

## 5. Anti-treadmill review policy

The failure mode: reviewer finds real edge cases → agent patches the named sites → each patch opens a sibling path missing the same guarantee → steady ~3 findings/round forever. Two dogfood PRs each burned 9 rounds this way.

- **Keep a finding-class ledger** in STATE.json: per finding `{round, file, classTag, fixCommit}` plus the per-round count trajectory.
- **Same class ≥3 rounds → stop patching.** The next dispatch changes kind: *"name the invariant this class violates; enforce it at ONE chokepoint; delete the per-site predicates; the findings become test cases."* A net reduction in branches is the goal.
- **Sibling-path sweep** in every fix dispatch: after fixing finding X, enumerate all same-shaped code paths and apply the same guarantee before pushing.
- **Out-of-scope deflection:** a finding owned by a different subsystem gets an issue filed + the thread resolved with the link — reviewer feedback becomes backlog, not round count.
- **Surface, don't grind:** genuine non-convergence (no dominant class, oscillation) after ~6–8 rounds is a human decision — offer merge-green / one-more-round / restructure / pause with a recommendation.

## 6. Dispatch hygiene

- Long messages sent into agent terminals can corrupt in transit (Windows ConPTY drops characters under bulk paste). Always include the **GitHub comment IDs** and instruct the agent to fetch exact bodies via `gh api`; treat the inline text as a summary only.
- Include the PR's **contract invariants** in every dispatch and require the agent to state which invariant each fix preserves — or which missing invariant the finding reveals (then propose the chokepoint, not a patch).
- Before nudging an "idle" agent, read its actual terminal: long reasoning turns misreport as idle/ready. A spinner or advancing output means working — don't interrupt.

## 7. Related roadmap issues

Executable counterparts to this playbook: #54 (reviewer-signal ingestion), #60 (finding-class ledger + simplification-round escalation), #61 (per-PR design contract + out-of-band verify), #28 (durable lifecycle state), #39 (idle-agent dispatch), #43 (agent usage limits), #53 (send corruption), #55 (activity misclassification), #56 (crash recovery), #57 (merged-PR cleanup).
