---
name: autonomous-drive-loop
description: Drive an autonomous pull-request review, fix, verification, and merge loop without prompt-carried state. Use when operating or recovering a recurring AO PR loop, interpreting reviewer-bot signals such as Codex, enforcing a merge bar, or preventing repeated review findings from becoming an unbounded patch treadmill.
---

# Autonomous Drive Loop

Run one evidence-based cycle at a time. Treat the recurring prompt only as a
pointer to this skill, the policy, and the state file.

Read [POLICY.md](POLICY.md) before acting. Its rules are binding until a human
deliberately changes the versioned file. Initialize a new loop with the procedure
below and keep its private `STATE.json` under the AO data directory, normally
`~/.ao/drive-loops/<loop-id>/STATE.json`. Never put runtime state in the repo.

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

Treat `nextAttemptSequence` as the next unused positive integer. Form each new
`attemptId` from the loop ID and that sequence value, and advance the value only
in the same durable state replacement that appends the prepared mutation.

- `decisions`: append `{id, recordedAt, kind, summary, evidence[]}`. Cite command
  output, provider URL/ID, or a tool receipt; do not cite memory.
- `dispatchLog`: append `{id, mutationIntentId, requestedAt, kind, headSha,
target, findingIds, outcome, externalId, evidence}`. Every provider-backed
  dispatch must point to its prepared `mutationLog` event through
  `mutationIntentId`; use that intent's attempt ID, external marker, and payload
  hash as the dispatch reconciliation keys. Use `outcome: "ambiguous"` when a
  crash makes delivery uncertain. Never create an ambiguous dispatch without
  its linked intent already durable.
- `mutationLog`: append write-ahead and receipt events for every non-idempotent
  provider mutation. A prepared event is `{id, intentId, recordedAt,
phase: "prepared", attemptId, kind, target, headSha, externalMarker,
payloadHash, providerBaseline}`; a terminal event reuses `intentId` and
  `attemptId` with `phase: "succeeded" | "failed" | "reconciled"` plus the
  provider receipt/evidence. `providerBaseline` is required when the provider
  cannot carry the external marker and records the pre-mutation state plus the
  exact observable transition that would prove this attempt occurred. Allocate
  `attemptId` from the durable
  `nextAttemptSequence`, then derive `intentId` and `externalMarker`
  deterministically from the loop, attempt ID, action kind, target, HEAD, and
  payload hash. Thus two policy-authorized writes with otherwise identical
  inputs remain distinct, while recovery of one ambiguous attempt reuses its
  original keys.
- `findingClassLedger`: keep one record per normalized root-cause class with
  `{classTag, invariant, rootCauseNote, occurrences[]}`. Each occurrence records
  `{findingId, round, url, file, headSha, disposition, fixCommit, issueUrl}`.
- `owedOutputs`: append `{id, createdAt, audience, summary, status, deliveredAt,
deliveryEvidence}`. Leave `status: "owed"` until delivery occurs.

Use stable event IDs and append history; do not rewrite prior decisions to make a
later outcome look inevitable. Never store credentials, tokens, or raw secrets.

## Initialize a new loop

Complete initialization before the first health check:

1. Resolve the repository root with `git rev-parse --show-toplevel` and treat
   `policy.path` as repository-relative. Run Git operations with
   `git -C <repo-root>` (or use a `:(top)` pathspec) so initialization is
   independent of the caller's current directory. Require the policy path to be
   tracked and unchanged in both the index and working tree.
2. Set `policy.gitCommit` to the full SHA returned by
   `git -C <repo-root> log -1 --format=%H -- ":(top)<policy-path>"`: the commit
   that last changed that path, not the repository HEAD when the loop happens to
   be created. Require the policy blob at repository `HEAD` and at that commit to
   have the same object ID.
3. Read the canonical policy bytes directly from that Git blob with a binary-safe
   `git cat-file blob <commit>:<policy-path>` equivalent. Compute
   `policy.contentSha256` over those blob bytes and read the policy version from
   the same bytes. Do not hash checkout bytes: line-ending conversion such as
   Windows `core.autocrlf` may make them bytewise different without changing the
   committed policy.
4. Copy [STATE.template.json](STATE.template.json) to the private state path.
   Populate every loop identity, target, reviewer, authority, and creation field;
   populate all policy fields with the values just computed; and leave
   `nextAttemptSequence` at `1`. Do not leave required strings or the pull-request
   number empty.
5. Persist the initialized document with the durable replacement procedure in
   step 4 below. Initialization is incomplete until the new state file and its
   parent-directory metadata have both been flushed successfully. Only then run
   the first health check.

## Run one cycle

### 1. Health check

- Confirm the AO daemon and relevant worker/reviewer sessions are reachable.
  Prefer `ao status` and `ao session get <id>` when those sessions are involved.
- Confirm the state parses, its repository and PR match the requested target,
  and `schemaVersion` is exactly the supported version `2`. A version 1 state
  lacks attempt sequencing and dispatch-to-intent links; stop before every
  provider write, including owed-output delivery, and require an explicit,
  operator-reviewed offline migration. Do not invent links or sequence values
  from prompt memory. Also stop on an unknown newer schema.
- Require `policy.gitCommit` and `policy.contentSha256` to be populated. From the
  repository root, require the policy path to be tracked and unchanged, resolve
  the commit that last changed its top-rooted path, read that commit's canonical
  Git blob bytes, and recompute the SHA-256 over those bytes. Confirm both values
  match the pins. The commit pin never means current repository HEAD or the HEAD
  at loop creation, and checkout line endings never define the content pin. Stop
  for operator confirmation before any action if either value is missing or
  mismatched.
- Stop rather than guess if identity, credentials, repository, or state integrity
  is uncertain.
- Before any provider write, including owed-output delivery, check for every
  prepared mutation without a terminal event and every `ambiguous` dispatch left
  by a crash. Resolve a dispatch through its `mutationIntentId`; search the
  provider by that intent's attempt-aware external marker, exact target, and
  payload hash, or apply its recorded baseline rule for a permitted markerless
  attempt. Append a reconciliation event and do not issue any provider mutation
  until every prior outcome is known. Stop if an ambiguous dispatch has no valid
  intent link or the search is inconclusive.
- Only after reconciliation leaves no unresolved provider write, inspect
  `owedOutputs`. Deliver outstanding items using the same write-ahead mutation
  protocol even when the cycle's only other safe action is to wait or stop.

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

Before every non-idempotent provider mutation, hash the exact payload, allocate
the next `attemptId`, and derive the deterministic intent ID and external marker
from the loop ID, attempt ID, action kind, exact target, HEAD, and payload hash.
In one state update, append the prepared event to `mutationLog` and advance
`nextAttemptSequence`; durably persist and validate the state; then perform
exactly that mutation. A policy-authorized new attempt consumes a new attempt ID
even when its target, HEAD, and payload match an earlier attempt. A recovery or
transport retry for an unresolved attempt must reuse the existing attempt ID,
intent ID, and marker and must not consume a new sequence value.

Put the marker in the provider-visible payload or idempotency-key field when
supported. This applies to review requests and re-triggers, fix dispatches, issue
filing, thread resolution, owed-output delivery, and other provider writes. A
provider-backed `dispatchLog` entry must reference the prepared intent.

If a provider cannot carry a marker, record a `providerBaseline` in the prepared
event: the exact pre-mutation provider state or watermark and the observable
transition that would uniquely prove the attempt occurred. Permit the mutation
only when that transition can be attributed to this attempt. After any prior
attempt with the same kind, target, HEAD, and payload hash, do not issue another
markerless attempt; a policy-authorized repeated attempt requires a
provider-visible marker or idempotency key. If recovery cannot distinguish a
markerless attempt from pre-existing or concurrent state, leave it unresolved,
stop provider writes, and request an operator disposition rather than matching
an earlier receipt or retrying. Never perform any mutation if its prepared intent
and required baseline cannot be made durable.

After the provider responds, append a terminal mutation event and capture the
command/tool result, provider ID or URL, target HEAD, and timestamp. If the
response is lost or the receipt cannot be durably persisted, leave the prepared
intent unresolved so recovery reconciles it before any retry. Treat the mutation
as completed only after the terminal event has passed the file and directory
flushes in step 4. Never report success from intent alone.

### 4. Update state from evidence

For ordinary observations and decisions, update `STATE.json` only after the
result is known. Provider mutations are the exception: persist their prepared
intent before acting and their terminal receipt afterward. For every state
write, use this durable replacement sequence:

1. Write a complete new JSON document to a temporary file in the same directory,
   flush its file contents to stable storage (`fsync`, `FlushFileBuffers`, or the
   platform equivalent), and close it.
2. Parse and validate the temporary file, then preserve the previous valid state
   as a backup without patching the live JSON in place.
3. Atomically replace `STATE.json` with the temporary file. Flush the resulting
   state file and then its parent-directory metadata to stable storage (or use a
   documented platform primitive that provides those same durability
   guarantees).
4. Treat the update as durable only after every flush succeeds. If a prepared
   update fails, stop before the provider write. If a terminal update fails after
   the provider write, treat its prepared event as unresolved so the next cycle
   reconciles it.

Prepared and terminal mutation events, attempt-sequence advancement, initialized
policy pins, decisions, and delivery receipts all use this sequence. Atomic
rename alone is not durable. Never patch the live JSON in place.

If the action may have succeeded but its terminal receipt was not persisted, the
next cycle must use the prepared intent's marker and payload fingerprint to
search the provider and append a reconciliation event. For a permitted
markerless attempt, compare the provider against its durable `providerBaseline`
and require the recorded transition to be uniquely attributable; otherwise stop
for operator disposition. Do not blindly repeat the action. If no valid state or
backup exists, stop and ask a human to reconstruct the non-derivable facts;
never recover them from a recurring prompt.

### 5. Deliver owed output

After the health check has reconciled all prior provider writes, read
`owedOutputs` from the reconciled state and again after every state update. Send
each owed human-facing result to its intended audience through the write-ahead
mutation protocol, then record the delivery receipt and set it to `delivered`. A
prepared message, a state entry, or a prompt saying `DONE` is not delivery. Retry
or surface failures; do not silently clear them.

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
  data-directory location; do not create worktree-local runtime state. End the
  review-fix head commit with exactly one
  `AO-Review-Fix-Invariant: {"pr":"<exact normalized PR URL>","mode":"preserve|add","invariant":"<one-line guarantee>"}`
  trailer. `preserve` must name exact canonical list-item text; `add` is
  grammar-checked and appended atomically with the pending-finding binding.
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
2. Run the health check and derive all provider/AO facts again. Reject any state
   whose schema is not the supported version before delivering owed output or
   issuing another provider write; version 1 requires an explicit,
   operator-reviewed offline migration.
3. Before delivering owed output or issuing any other provider write, reconcile
   every unresolved prepared mutation and ambiguous dispatch through its linked
   attempt-aware intent. Mark uncertain actions `ambiguous` until evidence
   resolves them.
4. After reconciliation is complete, resume from the first incomplete action,
   including owed output. Never trust a scheduler prompt's claim that an action
   or delivery happened.
5. Reschedule only after state is durably written and policy still permits
   another cycle.
