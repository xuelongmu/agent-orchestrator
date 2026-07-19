---
name: autonomous-drive-loop
description: Drive an autonomous pull-request review, fix, verification, and merge loop without prompt-carried state. Use when operating or recovering a recurring AO PR loop, interpreting reviewer-bot signals such as Codex, enforcing a merge bar, or preventing repeated review findings from becoming an unbounded patch treadmill.
---

# Autonomous Drive Loop

Run one evidence-based cycle at a time. Treat the recurring prompt only as a
pointer to this skill, the policy, and the state file.

Read [POLICY.md](POLICY.md) before acting. Its rules are binding until a human
deliberately changes the versioned file. Initialize a new loop by copying
[STATE.template.json](STATE.template.json) to a private `STATE.json` under the AO
data directory, normally `~/.ao/drive-loops/<loop-id>/STATE.json`. Never put
runtime state in the repo.

## Keep three stores separate

| Store              | Contents                                                                                                         | Update rule                                              |
| ------------------ | ---------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------- |
| `POLICY.md`        | Merge bar, signal semantics, safety limits, escalation thresholds                                                | Human-reviewed, versioned edits only                     |
| `STATE.json`       | Target identity plus non-derivable decisions, dispatch receipts, finding-class history, and owed-output receipts | Machine-write only, from observed command/tool results   |
| Fresh observations | HEAD, checks, PR state, reviews, issue comments, unresolved-thread contents, worker health                       | Re-query every cycle; never cache in the prompt or state |

Do not store a mutable `roundCount`, cached HEAD, cached CI status, approval flag,
unresolved count, or a claim that a deliverable was sent. Derive the first five
from authoritative systems. Mark output delivered only after actually sending it
and capturing the delivery result.

The state header pins loop identity, expected base branch, configured reviewer
channels, granted authorities, and the policy revision as integrity metadata.
Treat these values as setup decisions: validate them on recovery and never infer
them from the current provider response or prompt memory.

Keep the event collections limited to facts that cannot safely be reconstructed
after a crash:

- `decisions`: append `{id, recordedAt, kind, summary, evidence[]}`. Cite command
  output, provider URL/ID, or a tool receipt; do not cite memory.
- `dispatchLog`: append `{id, requestedAt, kind, headSha, target, findingIds,
outcome, externalId, evidence}`. Use `outcome: "ambiguous"` when a crash makes
  delivery uncertain.
- `mutationLog`: append write-ahead and receipt events for every non-idempotent
  provider mutation. A prepared event is `{id, intentId, recordedAt,
phase: "prepared", kind, target, headSha, externalMarker, payloadHash}`; a
  terminal event reuses `intentId` with `phase: "succeeded" | "failed" |
"reconciled"` plus the provider receipt/evidence. Derive `intentId` and
  `externalMarker` deterministically from the loop, action kind, target, HEAD,
  and payload hash so recovery can search for the same action.
- `findingClassLedger`: keep one record per normalized root-cause class with
  `{classTag, invariant, rootCauseNote, occurrences[]}`. Each occurrence records
  `{findingId, round, url, file, headSha, disposition, fixCommit, issueUrl}`.
- `owedOutputs`: append `{id, createdAt, audience, summary, status, deliveredAt,
deliveryEvidence}`. Leave `status: "owed"` until delivery occurs.

Use stable event IDs and append history; do not rewrite prior decisions to make a
later outcome look inevitable. Never store credentials, tokens, or raw secrets.

## Run one cycle

### 1. Health check

- Confirm the AO daemon and relevant worker/reviewer sessions are reachable.
  Prefer `ao status` and `ao session get <id>` when those sessions are involved.
- Confirm the state parses, its repository and PR match the requested target,
  and its policy version is understood. Require `policy.gitCommit` and
  `policy.contentSha256` to be populated, recompute the policy file's SHA-256,
  and confirm both the content hash and the commit which last changed the file
  match the pinned values. Stop for operator confirmation before any action if
  either value is missing or mismatched.
- Inspect `owedOutputs` immediately after loading state. Deliver outstanding
  items even when the cycle's only safe action is to wait or stop and no other
  state mutation will occur.
- Check for a prepared mutation without a terminal event and for an `ambiguous`
  dispatch or decision left by a crash. Search the provider by its deterministic
  external marker, target, and payload hash, append a reconciliation event, and
  do not issue another mutation until its outcome is known.
- Stop rather than guess if identity, credentials, repository, or state integrity
  is uncertain.

### 2. Derive live facts

Take a new snapshot. For GitHub, start with commands equivalent to:

```bash
gh pr view "$PR" --repo "$REPO" \
  --json number,url,state,isDraft,headRefOid,baseRefName,mergeStateStatus,statusCheckRollup,reviews,comments
gh api --paginate "repos/$REPO/pulls/$PR/reviews"
gh api --paginate "repos/$REPO/issues/$PR/comments"
```

Also query review threads, including `isResolved` and their comment bodies, via
the GitHub GraphQL API. Paginate every collection. A summary count is not a
substitute for reading the findings: a nonzero count can be stale or
non-actionable, and a zero count does not prove a reviewer supplied a verdict.

Set `H` to the newly observed `headRefOid`. Keep this snapshot in cycle-local
memory or disposable scratch only. If a later action observes a different HEAD,
discard the snapshot and restart the cycle.

### 3. Decide and act once

Apply [POLICY.md](POLICY.md) to the fresh snapshot and choose the smallest safe
next action: wait, dispatch findings, request review, re-trigger a bailed review,
surface non-convergence, or merge. Do not combine actions whose preconditions can
change between them.

Before a fix dispatch, ingest every actionable finding into the class ledger.
Classify by violated invariant/root cause, not by filename or reviewer wording.
Include in the one-shot dispatch:

1. the current findings and their source links;
2. a compact ledger summary by class and round;
3. the relevant design invariant or contract;
4. an instruction to sweep sibling paths with the same shape; and
5. either normal-fix or simplification-round instructions.

When a class has appeared at least three times, use simplification mode: identify
the invariant, enforce it at one chokepoint, remove redundant per-site predicates,
and treat individual findings as regression cases. Do not request another local
symptom patch.

Before every non-idempotent provider mutation, derive its deterministic intent
ID, external marker, and payload hash; append the prepared event to
`mutationLog`; atomically persist and validate the state; then perform exactly
that mutation. Put the marker in the provider-visible payload or idempotency-key
field when supported. This applies to review re-triggers, issue filing, thread
resolution, owed-output delivery, and other provider writes. If a provider
cannot carry a marker, record the exact target and payload fingerprint and use
both during reconciliation. Never perform the mutation if the prepared intent
cannot be made durable.

After the provider responds, append a terminal mutation event and capture the
command/tool result, provider ID or URL, target HEAD, and timestamp. If the
response is lost or the receipt cannot be persisted, leave the durable prepared
intent unresolved so recovery reconciles it before any retry. Never report
success from intent alone.

### 4. Update state from evidence

For ordinary observations and decisions, update `STATE.json` only after the
result is known. Provider mutations are the exception: persist their prepared
intent before acting and their terminal receipt afterward. For every state
write, write a complete new JSON document to a temporary file in the same
directory, parse/validate it, keep the previous valid file as a backup, and
atomically rename the new file. Never patch the live JSON in place.

If the action may have succeeded but its terminal receipt was not persisted, the
next cycle must use the prepared intent's marker and payload fingerprint to
search the provider and append a reconciliation event. Do not blindly repeat the
action. If no valid state or backup exists, stop and ask a human to reconstruct
the non-derivable facts; never recover them from a recurring prompt.

### 5. Deliver owed output

Read `owedOutputs` once from the state loaded at cycle start and again after every
state update. Send each owed human-facing result to its intended audience, then
record the delivery receipt and set it to `delivered`. A prepared message, a
state entry, or a prompt saying `DONE` is not delivery. Retry or surface failures;
do not silently clear them.

### 6. Reschedule with pointers only

If another cycle is needed, use a prompt shaped like:

```text
Continue autonomous drive loop <loop-id>. Read <repo>/skills/autonomous-drive-loop/SKILL.md and POLICY.md. Load <ao-data-dir>/drive-loops/<loop-id>/STATE.json. Derive all live facts fresh, run exactly one cycle, deliver owed output, then reschedule only if policy allows.
```

Do not add HEAD, round counts, verdicts, summaries of delivered work, or copied
state to this prompt. Those fields turn the scheduler into an unaudited database.

## Interpret reviewer-bot signals (Codex example)

Treat signal **channel**, **meaning**, and **target SHA** as separate questions.

1. Capture current HEAD `H` before requesting review and record the dispatch
   receipt against `H`.
2. Fetch both pull-request reviews and issue comments. Codex may put a clean
   verdict in an issue comment rather than a GitHub review.
3. Inspect review-thread contents and resolution state, not only the unresolved
   count.
4. Classify the signal:
   - A GitHub `APPROVED` review or an explicit final Codex message stating that
     the review completed with no actionable findings is a clean-verdict
     candidate.
   - `CHANGES_REQUESTED`, a final message containing actionable findings, or
     unresolved actionable threads is a fix signal.
   - A reaction, acknowledgement, "started reviewing" message, or other
     engagement is not a verdict.
   - A rate-limit, usage-limit, or quota-exhaustion message is reviewer failure,
     not approval and not engagement.
5. Attribute the candidate to `H`. For a review, require its `commit_id` to equal
   `H`. For an issue-comment verdict, require an explicit matching SHA or a
   recorded review dispatch for `H`, a comment after that dispatch, and proof
   that HEAD did not change through the verdict. Otherwise request a new review
   for the current HEAD.
6. Re-read HEAD immediately before using the verdict. Any new commit invalidates
   the prior merge snapshot.

On quota exhaustion, record the failure, create an owed operator notification,
and do not poll indefinitely. Retry after the stated reset or use another
reviewer only when policy explicitly permits it.

If Codex engages and then produces no final verdict, wait for the policy timeout,
confirm HEAD is unchanged and no newer request or result exists, then post a new
review request rather than editing the old one. Record the re-trigger receipt.
After the retry limit, surface non-convergence instead of scheduling more retries.

## Prevent review treadmills

- **Sibling-path sweep:** require the fixer to enumerate all paths with the same
  shape and apply the invariant before pushing.
- **Class repetition:** on the third occurrence of a class, make the next
  dispatch a simplification round. A new file or comment wording does not make it
  a new class.
- **Design contract:** require each fix to state which invariant it preserves or
  which missing invariant it reveals and where that invariant should be
  enforced. Read an existing contract from its configured versioned or AO
  data-directory location; do not create worktree-local runtime state.
- **Out-of-scope deflection:** when a finding belongs to another subsystem and is
  not required for this PR's correctness, first verify operator authority to file
  issues. If authorized, file a linked issue with the evidence and record the
  disposition; verify separate authority before resolving the thread. Without
  either authority, record an owed human output instead of performing the
  provider mutation or pretending it was handled.
- **Non-convergence:** stop automated fix dispatches at the policy cap. The
  terminal alternatives are: (a) a simplification round repeats the same class,
  (b) the reviewer retry budget is exhausted, or (c) the sixth completed review
  round has all P1 findings dispositioned. In (c), capture, resolve, or explicitly
  disposition every remaining non-P1 finding before proceeding. In every
  alternative, require exact-head green required CI, mergeable/non-draft state,
  and no unresolved human hold or feedback. Deliver a human escalation with the
  ledger, attempted invariants/chokepoints, current HEAD, and exact decision
  needed.

## Recover after a crash

1. Load the last valid `STATE.json` (or its backup) and the versioned policy.
2. Run the health check and derive all provider/AO facts again.
3. Reconcile the external systems with the last dispatch receipt. Mark uncertain
   actions `ambiguous` until evidence resolves them.
4. Resume from the first incomplete action, including owed output. Never trust a
   scheduler prompt's claim that an action or delivery happened.
5. Reschedule only after state is durably written and policy still permits
   another cycle.
