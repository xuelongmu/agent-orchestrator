# Autonomous drive-loop policy

**Policy version: 1**

This file contains stable operator policy. Change it only in a deliberate,
reviewed commit. Never let a recurring loop rewrite it. Procedures belong in
`SKILL.md`; observations and receipts belong in the loop's private `STATE.json`.

## Safety and authority

- Bind each loop to one repository and pull request. Stop on an identity mismatch.
- Re-query mutable provider and AO facts before acting. Do not act from a prior
  cycle's prompt, summary, cached HEAD, or cached status.
- Before each non-idempotent provider mutation, durably record a deterministic
  intent, a unique durable attempt ID, and either an external marker derived from
  that attempt ID or the permitted markerless baseline below. Distinct
  policy-authorized attempts use distinct attempt IDs even when their target,
  HEAD, and payload match. Require an external receipt before recording the
  mutation as successful; reconcile prepared or ambiguous mutations by their
  original attempt-aware marker, target, and payload fingerprint before retrying
  them.
- Require a provider-visible marker or idempotency key for a repeated attempt
  with the same action kind, target, HEAD, and payload. For a first markerless
  attempt, durably record a provider baseline and proceed only when an observable
  transition can be uniquely attributed to that attempt. If recovery remains
  ambiguous, stop for operator disposition; never match an earlier receipt or
  issue an automatic retry.
- Flush each new state file and its parent-directory metadata to stable storage
  before treating a prepared or terminal mutation event as durable. Atomic
  rename without those flushes is insufficient.
- Pin both the policy file's content hash and the commit that last changed
  `POLICY.md` when a loop is created. The commit pin is never arbitrary current
  HEAD or loop-creation HEAD. Hash the canonical Git blob bytes from a
  repository-rooted path, not checkout bytes subject to line-ending conversion.
  On recovery, stop before acting if either pin is missing or does not match the
  policy blob and its last-changing commit.
- Accept only a state schema whose migration rules this skill explicitly
  understands. Stop every provider write for an older or newer schema until an
  operator-reviewed offline migration produces a supported state; never infer
  missing mutation identity or reconciliation links.
- Do not override branch protection, dismiss findings, resolve another author's
  thread, file issues, switch reviewers, or merge unless the operator has granted
  that authority.
- Never expose secrets in state, prompts, dispatches, logs, or escalation output.

## Merge bar

Merge only when all of these conditions hold in one fresh snapshot:

1. The expected PR is open, not draft, and targets the expected base branch.
2. The candidate verdict and every merge check apply to the current HEAD SHA.
3. All required checks have completed successfully; none are pending, skipped
   when required, cancelled, failing, or unknown.
4. Every review and issue-comment verdict channel has been inspected, and one
   current-HEAD reviewer terminal condition holds: (a) an explicit clean final
   verdict/thumbs-up, (b) a completed review with no unresolved P1 finding, or
   (c) the sixth completed review round with every P1 finding dispositioned.
5. No unresolved human feedback or undispositioned reviewer finding remains.
   Capture, resolve, or explicitly disposition non-P1 findings before merging;
   finding contents and dispositions, not a summary count, determine this.
6. No human hold, requested change, unresolved ambiguity, out-of-scope correctness
   dependency, or owed merge-blocking output remains.
7. The provider reports the PR mergeable and repository-required human approvals
   are present.

Silence, reviewer engagement, zero unresolved items, a stale approval, quota
exhaustion, and green CI by themselves are never approval. Re-read HEAD
immediately before merge; restart evaluation if it changed.

## Reviewer signals and retries

- Accept final verdicts from both pull-request reviews and issue comments after
  verifying reviewer identity and target SHA.
- Treat acknowledgements and reactions as engagement only.
- Treat rate-limit, usage-limit, quota, authentication, and reviewer-process
  failures as blocked review attempts. Notify the operator; never reinterpret
  them as clean verdicts.
- After engagement without a verdict, wait 30 minutes before re-triggering. Send
  at most two re-triggers for the same HEAD, and only after confirming no newer
  request or verdict exists. Give each authorized re-trigger its own durable
  attempt ID and marker; never reconcile it against an earlier attempt's receipt.
- Do not silently switch to a different reviewer. A policy-authorized substitute
  must produce its own current-HEAD verdict.

## Review convergence

- Normalize findings by violated invariant or root cause and record every
  occurrence in the finding-class ledger.
- Require a sibling-path sweep in every fix dispatch.
- On the third occurrence of the same class, make the next dispatch a
  simplification round: enforce the invariant at one chokepoint and remove
  redundant site-specific logic.
- A finding may be deflected out of scope only when it is not required for this
  PR's correctness. File and link a follow-up issue before resolving the thread;
  otherwise escalate it as owed output.
- Permit at most six automated fix rounds per PR. Stop and escalate earlier when
  a simplification round produces the same class again or the reviewer retry
  limit is exhausted; those stops are not merge approval unless a reviewer
  terminal condition from the merge bar independently holds. At the sixth
  completed review round, capture, resolve, or explicitly disposition every
  remaining non-P1 finding and require every P1 finding to be dispositioned.
  Exact-head green required CI, mergeable/non-draft state, and no unresolved
  human hold or feedback remain mandatory. Include the current HEAD, finding
  classes and occurrences, attempted invariant or chokepoint, verification
  evidence, and precise decision required in any escalation.

## Delivery and scheduling

- Consider user/operator output delivered only after it is sent and a receipt or
  equivalent evidence is recorded.
- Keep owed output durable across crashes and process it every cycle, but first
  reconcile every unresolved prepared mutation and ambiguous dispatch. Owed
  delivery is itself a provider mutation and must not bypass that ordering.
- Recurring prompts may contain only the loop ID and pointers to `SKILL.md`, this
  policy, and `STATE.json`. They must not carry mutable facts or completion claims.
- Reschedule only when another action remains, state is durable including the
  required file and parent-directory flushes, and no policy stop condition
  applies.
