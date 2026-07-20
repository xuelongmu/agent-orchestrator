package lifecycle

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/designcontract"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/scmready"
	"github.com/aoagents/agent-orchestrator/backend/internal/sessionguard"
)

const reviewMaxNudge = 3
const mergeConflictMaxNudge = 1
const idleReviewMaxNudges = 3
const reactionReservationLease = 30 * time.Second

const reviewRoundCapNotificationFailed = "review-round-cap-notification-failed"
const suppressedNudgeNotificationFailed = "suppressed-nudge-notification-failed"
const reviewNudgeSendFailed = "review-nudge-send-failed"
const reviewFetchFailed = "review-fetch-failed"
const idleReviewUndeliverable = "idle-review-undeliverable"
const idleReviewNudgeFailed = "idle-review-nudge-failed"
const idleReviewNudgeExhausted = "idle-review-nudge-exhausted"
const prReactionDeliveryUncertain = "pr-reaction-delivery-uncertain"

var errMergedCleanupRateLimited = errors.New("merged cleanup parked by agent usage limit")
var errPRReactionHandoffPending = errors.New("uncertain PR reaction operator handoff is pending")

type humanHandoffOutcomeKind string

const (
	humanHandoffBlocked  humanHandoffOutcomeKind = "blocked"
	humanHandoffNotified humanHandoffOutcomeKind = "notified"
)

// humanHandoffOutcome is the durable terminal result of a human handoff.
// A failed delivery remains blocked and retryable; only a notification sink
// that returns success may advance it to notified.
type humanHandoffOutcome struct {
	Outcome humanHandoffOutcomeKind `json:"outcome"`
	Reason  string                  `json:"reason"`
}

// ReviewDeliveryOutcome reports what ApplyReviewResult did with a completed
// AO-internal review pass.
type ReviewDeliveryOutcome string

const (
	// ReviewDeliveryNoop means lifecycle did not send or confirm a review nudge
	// because the result was not relevant for delivery.
	ReviewDeliveryNoop ReviewDeliveryOutcome = "no_op"
	// ReviewDeliverySent means the worker nudge was sent or was already covered
	// by sendOnce dedup state and may be stamped delivered.
	ReviewDeliverySent ReviewDeliveryOutcome = "sent"
)

// ReviewResult is the already-persisted result of an AO-internal review pass.
// Lifecycle treats it as input to the reaction reducer; its only review_run
// mutation atomically records the simplification receipt and local event.
type ReviewResult struct {
	RunID               string
	BatchID             string
	WorkerID            domain.SessionID
	PRURL               string
	TargetSHA           string
	Verdict             domain.ReviewVerdict
	Body                string
	GithubReviewID      string
	DeliveredAt         *time.Time
	Findings            []domain.ReviewFinding
	Ledger              domain.FindingLedgerSummary
	SimplificationClass string
}

// ApplyReviewBatch reacts to one reviewer CLI submission after the review
// service has decided which current-head changes-requested results are
// deliverable.
func (m *Manager) ApplyReviewBatch(ctx context.Context, workerID domain.SessionID, batchID string, results []ReviewResult) (ReviewDeliveryOutcome, error) {
	if batchID == "" || len(results) == 0 {
		return ReviewDeliveryNoop, nil
	}
	rec, ok, err := m.store.GetSession(ctx, workerID)
	if err != nil || !ok {
		return ReviewDeliveryNoop, err
	}
	if rec.IsTerminated || rec.Activity.State.PausesAutomation() {
		return ReviewDeliveryNoop, nil
	}
	if m.guard == nil {
		return ReviewDeliveryNoop, nil
	}
	for _, result := range results {
		ready, readyErr := m.ensurePRDesignContractDelivered(ctx, workerID, result.PRURL)
		if readyErr != nil {
			return ReviewDeliveryNoop, readyErr
		}
		if !ready {
			return ReviewDeliveryNoop, nil
		}
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].PRURL != results[j].PRURL {
			return results[i].PRURL < results[j].PRURL
		}
		return results[i].RunID < results[j].RunID
	})
	fences, ok := reviewBatchReactionFences(workerID, results)
	if !ok {
		return ReviewDeliveryNoop, nil
	}
	var msg strings.Builder
	fmt.Fprintf(&msg, "[AO reviewer] AO's internal code reviewer submitted %d review(s) requesting changes.\n", len(results))
	var sigParts []string
	for i, r := range results {
		fmt.Fprintf(&msg, "\nReview %d\nPR: %s\nVerdict: %s", i+1, domain.SanitizeControlChars(r.PRURL), domain.SanitizeControlChars(string(r.Verdict)))
		if err := m.writeDesignContractDispatch(ctx, &msg, rec.Metadata.WorkspacePath, r.PRURL); err != nil {
			return ReviewDeliveryNoop, err
		}
		if r.TargetSHA != "" {
			fmt.Fprintf(&msg, "\nHead commit: %s", domain.SanitizeControlChars(r.TargetSHA))
		}
		writeReviewDispatchPolicy(&msg, r)
		if r.GithubReviewID != "" {
			safeReviewID := domain.SanitizeControlChars(r.GithubReviewID)
			fmt.Fprintf(&msg, "\nGitHub review: %s", safeReviewID)
			fmt.Fprintf(&msg, "\nOnce you have addressed it, reply on GitHub review %s with how you addressed it, then resolve the review comment threads you addressed.", safeReviewID)
		}
		if body := actionableReviewBody(r); body != "" {
			fmt.Fprintf(&msg, "\n\n%s:\n%s\n", reviewBodyLabel(r), domain.SanitizeControlChars(body))
		}
		sigParts = append(sigParts, strings.Join([]string{r.RunID, r.PRURL, r.TargetSHA, r.GithubReviewID, r.Body}, "\x00"))
	}
	anchorPR := results[0].PRURL
	key := "review-batch:" + anchorPR + ":" + batchID
	sig := strings.Join(sigParts, "\x01")
	outcome, err := m.sendOnce(ctx, workerID, anchorPR, key, sig, fences, msg.String(), reviewMaxNudge)
	if err != nil {
		return ReviewDeliveryNoop, err
	}
	if outcome == sendOnceSuppressed {
		// The worker went terminated/needs-input between the entry guard and the
		// paste: nothing reached it, so do NOT let the caller stamp the run
		// delivered — it must re-fire once the session is workable again.
		return ReviewDeliveryNoop, nil
	}
	for _, result := range results {
		if err := m.emitSimplificationRound(ctx, rec, result); err != nil {
			return ReviewDeliveryNoop, err
		}
	}
	return ReviewDeliverySent, nil
}

func reviewBatchReactionFences(workerID domain.SessionID, results []ReviewResult) ([]ports.PRReactionFence, bool) {
	fences := make([]ports.PRReactionFence, 0, len(results))
	byPR := make(map[string]string, len(results))
	for _, result := range results {
		prURL := strings.TrimSpace(result.PRURL)
		head := strings.TrimSpace(result.TargetSHA)
		if prURL == "" || head == "" || workerID == "" {
			return nil, false
		}
		if prior, ok := byPR[prURL]; ok {
			if prior != head {
				return nil, false
			}
			continue
		}
		byPR[prURL] = head
		fences = append(fences, ports.PRReactionFence{PRURL: prURL, SessionID: workerID, HeadSHA: head})
	}
	return fences, len(fences) > 0
}

func writeReviewDispatchPolicy(msg *strings.Builder, r ReviewResult) {
	if r.Ledger.TotalFindings > 0 {
		fmt.Fprintf(msg, "\nFinding-class ledger: %s\n", findingLedgerSummary(r.Ledger))
	}
	if r.SimplificationClass != "" {
		fmt.Fprintf(msg, "\nKIND: SIMPLIFICATION ROUND\nThe class %q has recurred at least 3 times. Identify the invariant this class violates and enforce it at ONE chokepoint; delete the per-site predicates. Treat the individual findings as symptoms and test cases, not as a patch list.\n", domain.SanitizeControlChars(r.SimplificationClass))
	}
	msg.WriteString("\nAfter addressing each finding, enumerate every sibling code path with the same shape and apply the same guarantee before pushing.\n")
}

func (m *Manager) writeDesignContractDispatch(ctx context.Context, msg *strings.Builder, workspacePath, prURL string) error {
	contract, ok, err := m.store.GetPRDesignContract(ctx, prURL)
	if err != nil {
		return fmt.Errorf("review dispatch: get design contract for %s: %w", prURL, err)
	}
	if !ok || strings.TrimSpace(contract) == "" {
		return fmt.Errorf("review dispatch: canonical design contract for %s is missing", prURL)
	}
	if err := designcontract.MaterializePR(ctx, workspacePath, prURL, contract); err != nil {
		slog.Debug("review dispatch: design contract projection skipped", "prURL", prURL, "error", err)
	}
	fmt.Fprintf(msg, "\nDesign contract (canonical AO state; .ao/CONTRACT.md is a read-only projection):\n%s\n", domain.SanitizeControlChars(designcontract.ForDispatch(contract)))
	fmt.Fprintf(msg, "\nEvery review-fix head commit must end with exactly one structured trailer bound by AO to this provider-observed commit and exact normalized PR. Preserve an existing canonical list item with `AO-Review-Fix-Invariant: {\"pr\":%q,\"mode\":\"preserve\",\"invariant\":\"<exact canonical line text without the list marker>\"}`. If a finding reveals a missing invariant, enforce it at one chokepoint and use mode `add` with a new plain one-line guarantee; AO validates and atomically appends it to this PR's canonical contract while binding the pending findings. Never edit the workspace projection directly. `ao contract add --pr %s --invariant \"<guarantee>\"` remains the explicit human/fixer contract path outside this commit boundary.\n", domain.SanitizeControlChars(prURL), domain.SanitizeControlChars(prURL))
	return nil
}

func findingLedgerSummary(ledger domain.FindingLedgerSummary) string {
	parts := make([]string, 0, len(ledger.Classes))
	for _, class := range ledger.Classes {
		parts = append(parts, fmt.Sprintf("%d are class %s", class.Count, domain.SanitizeControlChars(class.ClassTag)))
	}
	summary := fmt.Sprintf("%d findings over %d rounds", ledger.TotalFindings, ledger.Rounds)
	if len(parts) > 0 {
		summary += "; " + strings.Join(parts, "; ")
	}
	return summary
}

func actionableReviewBody(r ReviewResult) string {
	if len(r.Findings) == 0 {
		return r.Body
	}
	var lines []string
	for _, finding := range r.Findings {
		if finding.FullyDeflected() {
			continue
		}
		line := fmt.Sprintf("- [%s]", finding.ClassTag)
		if finding.File != "" {
			line += " " + finding.File
		}
		if finding.Body != "" {
			line += ": " + finding.Body
		} else if finding.RootCauseNote != "" {
			line += ": " + finding.RootCauseNote
		}
		if finding.ThreadID != "" {
			line += " (thread " + finding.ThreadID + ")"
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func reviewBodyLabel(r ReviewResult) string {
	if len(r.Findings) == 0 {
		return "Review body"
	}
	return "Actionable findings"
}

func (m *Manager) emitSimplificationRound(ctx context.Context, rec domain.SessionRecord, r ReviewResult) error {
	if r.SimplificationClass == "" {
		return nil
	}
	if m.telemetry == nil {
		return nil
	}
	durable, ok := m.telemetry.(ports.DurableLocalEventSink)
	if !ok {
		return errors.New("lifecycle: simplification telemetry sink does not report local durability")
	}
	if !durable.DurableLocalTelemetry() {
		// Telemetry is disabled (the production NoopSink). Do not create a local
		// telemetry row that the user opted out of.
		return nil
	}
	events, ok := m.store.(simplificationEventStore)
	if !ok {
		return errors.New("lifecycle: review store does not support durable simplification events")
	}
	dispatchedAt := m.clock()
	projectID, sessionID := rec.ProjectID, rec.ID
	event := ports.TelemetryEvent{
		ID:   simplificationTelemetryID(r.RunID, r.TargetSHA),
		Name: "review_simplification_round", Source: "lifecycle", OccurredAt: dispatchedAt,
		Level: ports.TelemetryLevelInfo, ProjectID: &projectID, SessionID: &sessionID,
		Payload: map[string]any{"pr_url": r.PRURL, "class_tag": r.SimplificationClass, "findings": r.Ledger.TotalFindings, "rounds": r.Ledger.Rounds},
	}
	durableEvent, _, err := events.EnsureReviewRunSimplificationEvent(ctx, r.RunID, r.TargetSHA, event)
	if err != nil {
		return fmt.Errorf("persist simplification event for review run %q: %w", r.RunID, err)
	}
	// The undelivered review run is the replay trigger if the process stops
	// after the atomic SQLite write above. Every replay carries the same ID;
	// local SQLite ignores it and PostHog receives it as $insert_id.
	m.emitTelemetry(ctx, durableEvent)
	return nil
}

func simplificationTelemetryID(runID, targetSHA string) string {
	sum := sha256.Sum256([]byte(runID + "\x00" + targetSHA))
	return fmt.Sprintf("tev_review_simplification_%x", sum[:])
}

type reactionState struct {
	mu       sync.Mutex
	seen     map[string]string
	attempts map[string]int
	handoffs map[string]humanHandoffOutcome
	// loaded tracks PR URLs whose persisted dedup payload has been merged into
	// seen/attempts during this process. Lazy: we only pay the DB read on the
	// first reaction touching each PR after startup.
	loaded map[string]bool
}

func newReactionState() reactionState {
	return reactionState{seen: map[string]string{}, attempts: map[string]int{}, handoffs: map[string]humanHandoffOutcome{}, loaded: map[string]bool{}}
}

// reactionPayload is the JSON document persisted in pr.last_nudge_signature.
// Keeping the schema explicit (and stable) lets the daemon restart and resume
// the existing dedup state without re-nudging an agent.
type reactionPayload struct {
	Seen     map[string]string              `json:"seen,omitempty"`
	Attempts map[string]int                 `json:"attempts,omitempty"`
	Handoffs map[string]humanHandoffOutcome `json:"handoffs,omitempty"`
}

func reviewRoundCapHandoffKey(prURL string) string {
	return "review-handoff:" + prURL + ":round-cap"
}

// ApplyReviewRoundCapHandoff is the single outcome chokepoint for an exhausted
// automatic review loop. Notification delivery comes first; only confirmed
// delivery latches the handoff and parks the worker in waiting_input. A failed
// or missing sink records blocked and returns an error so the SCM observer does
// not acknowledge/throttle the snapshot and retries on its next poll.
func (m *Manager) ApplyReviewRoundCapHandoff(ctx context.Context, id domain.SessionID, obs ports.SCMObservation, round int) error {
	prURL := firstSCMNonEmpty(obs.PR.URL, obs.PR.HTMLURL)
	if prURL == "" {
		return nil
	}
	rec, ok, err := m.store.GetSession(ctx, id)
	if err != nil || !ok {
		return err
	}
	// Preserve the pending-input suppression invariant: another user decision
	// already owns this session, so do not stack a review-cap handoff on it.
	if rec.IsTerminated || rec.Activity.State.PausesAutomation() {
		return nil
	}

	key := reviewRoundCapHandoffKey(prURL)
	m.react.mu.Lock()
	defer m.react.mu.Unlock()
	if !m.react.loaded[prURL] {
		if err := m.loadPRSignaturesLocked(ctx, prURL); err != nil {
			return err
		}
		m.react.loaded[prURL] = true
	}
	intent := ports.NotificationIntent{
		Type:               domain.NotificationNeedsInput,
		SessionID:          rec.ID,
		ProjectID:          rec.ProjectID,
		PRURL:              prURL,
		CreatedAt:          m.clock(),
		SessionDisplayName: rec.DisplayName,
		PRNumber:           obs.PR.Number,
		PRTitle:            obs.PR.Title,
		PRSourceBranch:     obs.PR.SourceBranch,
		PRTargetBranch:     obs.PR.TargetBranch,
		Provider:           obs.Provider,
		Repo:               obs.Repo,
	}
	return m.deliverHumanHandoffLocked(ctx, prURL, key, reviewRoundCapNotificationFailed, intent, func() error {
		return m.parkReviewRoundCapSession(ctx, id)
	})
}

// deliverHumanHandoffLocked is the single durable outcome chokepoint for
// lifecycle fallbacks that must reach a human. The caller holds react.mu and
// has loaded prURL. Only confirmed notification delivery advances to notified;
// missing/failed delivery stays blocked and retryable.
func (m *Manager) deliverHumanHandoffLocked(ctx context.Context, prURL, key, reason string, intent ports.NotificationIntent, afterNotify func() error) error {
	if prior := m.react.handoffs[key]; prior.Outcome == humanHandoffNotified {
		return nil
	}
	record := func(outcome humanHandoffOutcome) error {
		m.react.handoffs[key] = outcome
		return m.persistPRSignaturesLocked(ctx, prURL)
	}
	blocked := humanHandoffOutcome{Outcome: humanHandoffBlocked, Reason: reason}
	if m.notifications == nil {
		return errors.Join(errors.New("lifecycle: human handoff notification sink is unavailable"), record(blocked))
	}
	if err := m.notifications.Notify(ctx, intent); err != nil {
		return errors.Join(fmt.Errorf("lifecycle: human handoff notification: %w", err), record(blocked))
	}
	if afterNotify != nil {
		if err := afterNotify(); err != nil {
			return err
		}
	}
	return record(humanHandoffOutcome{Outcome: humanHandoffNotified, Reason: reason})
}

func (m *Manager) parkReviewRoundCapSession(ctx context.Context, id domain.SessionID) error {
	return m.mutate(ctx, id, func(cur domain.SessionRecord, now time.Time) (domain.SessionRecord, bool) {
		if cur.IsTerminated || cur.Activity.State.PausesAutomation() {
			return cur, false
		}
		cur.Activity = domain.Activity{State: domain.ActivityWaitingInput, LastActivityAt: now}
		return cur, true
	})
}

// pendingNudge is one actionable PR nudge queued by ApplyPRObservation. Queuing
// each condition's nudge (instead of sending inline and returning) keeps the
// conditions independent — none can suppress another — and centralizes the
// send + dedup in a single loop.
type pendingNudge struct {
	key         string
	sig         string
	msg         string
	maxAttempts int
}

// ApplyPRObservation reacts to a fetched PR observation after the PR service has
// persisted it. It does not write PR rows; it owns PR-driven lifecycle effects
// and sends actionable agent nudges such as rebase, fix-CI, and
// address-review-feedback prompts.
func (m *Manager) ApplyPRObservation(ctx context.Context, id domain.SessionID, o ports.PRObservation) error {
	if !o.Fetched {
		return nil
	}
	// A PR reaching a terminal state (merged or closed) no longer ends the
	// session on its own: a session may own several PRs. Terminate only when no
	// open PR remains and at least one of them merged. The observer persists the
	// PR row before calling lifecycle, so the store already reflects this
	// transition when sessionComplete reads it.
	if o.Merged || o.Closed {
		done, err := m.sessionComplete(ctx, id)
		if err != nil {
			return err
		}
		if done {
			if err := m.cleanupMergedSession(ctx, id, o.URL); err != nil {
				return err
			}
			return m.reconcileDependencies()
		}
		return nil
	}
	ready, err := m.ensurePRDesignContractDelivered(ctx, id, o.URL)
	if err != nil || !ready {
		return err
	}
	// A provider-confirmed recovery ends the current conflict episode. Clear its
	// durable one-shot so the same head can be dispatched again if a later base
	// advance makes it conflicting anew.
	if o.Mergeability != domain.MergeConflicting && o.Mergeability != domain.MergeUnknown && o.Mergeability != "" {
		if err := m.resetMergeConflictNudges(ctx, o.URL, ""); err != nil {
			return err
		}
	}
	rec, ok, err := m.store.GetSession(ctx, id)
	if err != nil || !ok {
		return err
	}
	if rec.IsTerminated || rec.Activity.State.PausesAutomation() {
		return nil
	}
	// A single PR can trip several actionable conditions at once (failing CI,
	// unresolved review comments, a merge conflict). Queue every applicable nudge
	// and send them together, so each surfaces independently instead of one
	// returning early and hiding the rest — the bug this reducer had, where a CI
	// failure suppressed review feedback on the same PR. Each nudge self-dedups
	// via sendOnce; a send error short-circuits, and nudges already sent have
	// persisted their own dedup signature so the next poll retries only the rest.
	ident := prIdentity(o)
	var nudges []pendingNudge
	reactionHead := trustworthyPRReactionHead(o)
	trustedHead := reactionHead != ""

	if trustedHead && o.CI == domain.CIFailing {
		checks := failedPRChecks(o.Checks)
		if len(checks) > 0 {
			msg := formatCIFailureMessage(checks)
			if ident != "your PR" {
				msg = strings.Replace(msg, "your PR", ident, 1)
			}
			if o.URL != "" {
				msg += "\nPR: " + domain.SanitizeControlChars(o.URL)
			}
			nudges = append(nudges, pendingNudge{key: "ci:" + o.URL, sig: ciFailureSignature(checks), msg: msg, maxAttempts: 0})
		}
	}
	if trustedHead && (o.Review == domain.ReviewChangesRequest || hasUnresolvedComments(o.Comments)) {
		comments := unresolvedReviewComments(o.Comments)
		msg := formatReviewCommentsMessage(comments)
		var contract strings.Builder
		if err := m.writeDesignContractDispatch(ctx, &contract, rec.Metadata.WorkspacePath, o.URL); err != nil {
			return err
		}
		msg += contract.String()
		if ident != "your PR" {
			msg = strings.Replace(msg, "your PR", ident, 1)
		}
		if o.URL != "" {
			msg += "\nPR: " + domain.SanitizeControlChars(o.URL)
		}
		sig := reviewCommentsSignature(comments)
		if sig == "" {
			sig = string(o.Review)
		}
		nudges = append(nudges, pendingNudge{key: "review:" + o.URL, sig: sig, msg: msg, maxAttempts: reviewMaxNudge})
	}
	if o.Mergeability == domain.MergeConflicting && strings.TrimSpace(o.URL) != "" && strings.TrimSpace(o.HeadSHA) != "" {
		// Only the bottom of a stack is eligible for the rebase nudge. A PR
		// stacked on an open parent is expected to report conflicts against its
		// parent branch until the parent merges and it retargets, so nudging the
		// agent to rebase it now would be noise. Mergeability UNKNOWN (the brief
		// post-retarget recompute window) never reaches here.
		blocked, err := m.prBlockedByOpenParent(ctx, id, o.URL)
		if err != nil {
			return err
		}
		if !blocked {
			sig := mergeConflictSignature(o)
			key := mergeConflictNudgeKey(o.URL, sig)
			if err := m.resetMergeConflictNudges(ctx, o.URL, key); err != nil {
				return err
			}
			base := domain.SanitizeControlChars(o.TargetBranch)
			if strings.TrimSpace(base) == "" {
				base = "the PR base branch"
			}
			msg := "The provider reports merge conflicts on " + ident + "."
			msg += "\nExact head: " + domain.SanitizeControlChars(o.HeadSHA)
			msg += "\nBase: " + base
			msg += "\nConfirm the PR still points at this exact head, then update from the base using the repository's normal merge or rebase workflow, resolve the conflicts, run the relevant checks, and push the updated head. AO has not executed Git commands for you."
			msg += "\nPR: " + domain.SanitizeControlChars(o.URL)
			nudges = append(nudges, pendingNudge{key: key, sig: sig, msg: msg, maxAttempts: mergeConflictMaxNudge})
		}
	}

	var reactionErrs []error
	for _, n := range nudges {
		fences := []ports.PRReactionFence{{PRURL: o.URL, SessionID: id, HeadSHA: reactionHead}}
		if _, err := m.sendOnce(ctx, id, o.URL, n.key, n.sig, fences, n.msg, n.maxAttempts); err != nil {
			// One condition's retryable control-plane failure must not prevent
			// independent sibling reactions from reaching the agent. Return the
			// joined failure only after every condition has been reduced so the
			// observer withholds its semantic acknowledgement and retries.
			reactionErrs = append(reactionErrs, err)
		}
	}
	return errors.Join(reactionErrs...)
}

func trustworthyPRReactionHead(o ports.PRObservation) string {
	// Check results can lag the PR snapshot even when every check names the
	// same commit. Only the provider-observed PR head authorizes dispatch.
	return strings.TrimSpace(o.HeadSHA)
}

func mergeConflictSignature(o ports.PRObservation) string {
	return strings.Join([]string{o.HeadSHA, o.TargetBranch, string(o.Mergeability)}, "\x00")
}

func mergeConflictNudgeKey(prURL, sig string) string {
	sum := sha256.Sum256([]byte(sig))
	return fmt.Sprintf("merge-conflict:%s:%x", prURL, sum)
}

// resetMergeConflictNudges removes superseded per-head conflict episodes. The
// current key may be kept for an unchanged conflicting observation; an empty
// keepKey is a provider-confirmed recovery. Caller-visible persistence makes
// both paths survive daemon restarts without growing an unbounded key ledger.
func (m *Manager) resetMergeConflictNudges(ctx context.Context, prURL, keepKey string) error {
	if strings.TrimSpace(prURL) == "" {
		return nil
	}
	m.react.mu.Lock()
	defer m.react.mu.Unlock()
	if !m.react.loaded[prURL] {
		if err := m.loadPRSignaturesLocked(ctx, prURL); err != nil {
			return err
		}
		m.react.loaded[prURL] = true
	}
	prefix := "merge-conflict:" + prURL
	changed := false
	for key := range m.react.seen {
		if key != keepKey && (key == prefix || strings.HasPrefix(key, prefix+":")) {
			delete(m.react.seen, key)
			changed = true
		}
	}
	for key := range m.react.attempts {
		if key != keepKey && (key == prefix || strings.HasPrefix(key, prefix+":")) {
			delete(m.react.attempts, key)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return m.persistPRSignaturesLocked(ctx, prURL)
}

// ApplySCMReviewFetchFailure handles the one SCM failure that can otherwise
// leave an idle PR overlay silent forever. The handoff identity comes from the
// observed head/review pipeline state, not agent-delivery proof: a fetch failure
// itself needs a human fallback even when the prior review reaction escalated
// without reaching the agent. Handoffs and agent-delivery signatures remain
// separate facts so a daemon restart cannot reinterpret that escalation as work
// the agent received.
func (m *Manager) ApplySCMReviewFetchFailure(ctx context.Context, id domain.SessionID, o ports.SCMObservation) error {
	prURL := firstSCMNonEmpty(o.PR.URL, o.PR.HTMLURL)
	if prURL == "" || o.PR.Merged || o.PR.Closed {
		return nil
	}
	m.react.mu.Lock()
	defer m.react.mu.Unlock()
	if !m.react.loaded[prURL] {
		if err := m.loadPRSignaturesLocked(ctx, prURL); err != nil {
			return err
		}
		m.react.loaded[prURL] = true
	}
	failureSig := reviewFetchFailureSignature(o)
	if failureSig == "" {
		return nil
	}
	rec, eligible, err := m.idleReviewFailureSession(ctx, id)
	if err != nil || !eligible {
		return err
	}
	return m.deliverReviewFailureHandoffLocked(ctx, rec, o, reviewFetchFailed)
}

// ApplyIdleReviewSnapshot is the single deferral decision for an idle worker
// with a freshly fetched review backlog. The observer calls it only for a
// semantically unchanged snapshot: a changed snapshot first belongs to the
// normal review dispatcher, and the next forced idle refresh can safely decide
// whether that delivery made progress.
//
// A human handoff is deferred only when the worker is positively idle beyond
// the activity window, the review snapshot is complete, actionable comments
// are present, the normal review delivery is durably proven, the reminder
// reaches the worker, and its bounded budget remains. Every uncertainty is a
// durable one-time handoff for this idle episode.
func (m *Manager) ApplyIdleReviewSnapshot(ctx context.Context, id domain.SessionID, o ports.SCMObservation) error {
	prURL := firstSCMNonEmpty(o.PR.URL, o.PR.HTMLURL)
	if !o.Fetched || prURL == "" || o.PR.Merged || o.PR.Closed {
		return nil
	}
	ready, err := m.ensurePRDesignContractDelivered(ctx, id, prURL)
	if err != nil || !ready {
		return err
	}
	rec, ok, err := m.store.GetSession(ctx, id)
	if err != nil || !ok {
		return err
	}

	m.react.mu.Lock()
	defer m.react.mu.Unlock()
	if !m.react.loaded[prURL] {
		if err := m.loadPRSignaturesLocked(ctx, prURL); err != nil {
			return err
		}
		m.react.loaded[prURL] = true
	}

	idleBeyondWindow := !rec.IsTerminated && rec.Activity.State == domain.ActivityIdle &&
		m.clock().Sub(rec.Activity.LastActivityAt) >= m.window
	allUnresolved := unresolvedSCMReviewThreads(o.Review.Threads)
	if !idleBeyondWindow || (!o.Review.Partial && allUnresolved == 0) {
		return m.clearIdleReviewEpisodeLocked(ctx, prURL)
	}
	if rec.IsTerminated || rec.Activity.State.NeedsInput() {
		return nil
	}

	episodeKey := idleReviewEpisodeKey(prURL, rec.Activity.LastActivityAt)
	handoffKey, err := m.canonicalIdleReviewHandoffLocked(ctx, prURL, rec.Activity.LastActivityAt)
	if err != nil {
		return err
	}
	if prior := m.react.handoffs[handoffKey]; prior.Outcome == humanHandoffNotified {
		return nil
	}
	prObs := scmToPRObservation(o)
	actionable := unresolvedReviewComments(prObs.Comments)
	actionableSig := reviewCommentsSignature(actionable)
	deliveredSig := m.react.seen["review:"+prURL]

	handoff := func(reason string) error {
		current, err := m.idleReviewEpisodeCurrent(ctx, id, rec.Activity.LastActivityAt)
		if err != nil {
			return err
		}
		if !current {
			return m.clearIdleReviewEpisodeIdentityLocked(ctx, prURL, rec.Activity.LastActivityAt)
		}
		body := "The idle agent could not be safely reminded about its pull request review backlog. Open the pull request and review the outstanding feedback."
		if reason == idleReviewNudgeExhausted {
			body = fmt.Sprintf("The agent remains idle with unresolved pull request feedback after %d automated reminder(s). Human review is needed.", idleReviewMaxNudges)
		}
		intent := ports.NotificationIntent{
			Type:               domain.NotificationNeedsInput,
			SessionID:          rec.ID,
			ProjectID:          rec.ProjectID,
			PRURL:              prURL,
			CreatedAt:          m.clock(),
			TitleOverride:      "Idle PR review backlog needs attention",
			BodyOverride:       body,
			SessionDisplayName: rec.DisplayName,
			PRNumber:           o.PR.Number,
			PRTitle:            o.PR.Title,
			PRSourceBranch:     o.PR.SourceBranch,
			PRTargetBranch:     o.PR.TargetBranch,
			Provider:           o.Provider,
			Repo:               o.Repo,
		}
		return m.deliverHumanHandoffLocked(ctx, prURL, handoffKey, reason, intent, nil)
	}

	// A partial page cannot prove the backlog is complete. Likewise, a backlog
	// with no sendable human-authored comment, or one lacking proof of a prior
	// real delivery, cannot justify suppressing the human alert.
	if rec.FirstSignalAt.IsZero() || o.Review.Partial || allUnresolved == 0 || actionableSig == "" ||
		!reviewSignatureCovers(deliveredSig, actionableSig) || m.guard == nil {
		return handoff(idleReviewUndeliverable)
	}

	attempts := m.react.attempts[episodeKey]
	if attempts >= idleReviewMaxNudges {
		return handoff(idleReviewNudgeExhausted)
	}
	msg := fmt.Sprintf("You still have %d unresolved review comment(s) on %s and appear to be idle. Address them now, push fixes, and resolve each addressed thread.\n\n%s",
		len(actionable), prIdentity(prObs), formatReviewCommentsMessage(actionable))
	var contract strings.Builder
	if err := m.writeDesignContractDispatch(ctx, &contract, rec.Metadata.WorkspacePath, prURL); err != nil {
		return err
	}
	msg += contract.String()
	outcome, sendErr := m.guard.NudgeIdleEpisode(ctx, id, msg, rec.Activity.LastActivityAt)
	if outcome == sessionguard.SuppressedStaleEpisode || outcome == sessionguard.SuppressedRateLimited {
		return m.clearIdleReviewEpisodeIdentityLocked(ctx, prURL, rec.Activity.LastActivityAt)
	}
	if sendErr != nil || outcome != sessionguard.Sent {
		handoffErr := handoff(idleReviewNudgeFailed)
		if sendErr != nil {
			return errors.Join(sendErr, handoffErr)
		}
		return handoffErr
	}
	m.react.attempts[episodeKey] = attempts + 1
	return m.persistPRSignaturesLocked(ctx, prURL)
}

// idleReviewEpisodeCurrent closes the gap between the snapshot's initial
// eligibility read and its eventual side effect. Activity recovery persists
// before its durable idle-review cleanup acquires react.mu, so a reducer can be
// waiting behind this decision while the store already says the worker is
// active. Requiring the same idle timestamp immediately before each nudge or
// handoff prevents stale positive-idle evidence from driving either action.
func (m *Manager) idleReviewEpisodeCurrent(ctx context.Context, id domain.SessionID, idleSince time.Time) (bool, error) {
	rec, ok, err := m.store.GetSession(ctx, id)
	if err != nil || !ok {
		return false, err
	}
	return !rec.IsTerminated && rec.Activity.State == domain.ActivityIdle &&
		rec.Activity.LastActivityAt.Equal(idleSince) &&
		m.clock().Sub(rec.Activity.LastActivityAt) >= m.window, nil
}

func unresolvedSCMReviewThreads(threads []ports.SCMReviewThreadObservation) int {
	n := 0
	for _, th := range threads {
		if !th.Resolved {
			n++
		}
	}
	return n
}

func reviewSignatureCovers(delivered, current string) bool {
	if delivered == "" || current == "" {
		return false
	}
	deliveredVersion, delivered := splitReviewSignatureVersion(delivered)
	currentVersion, current := splitReviewSignatureVersion(current)
	if deliveredVersion != currentVersion {
		return false
	}
	covered := make(map[string]struct{})
	for _, part := range strings.Split(delivered, "\x01") {
		covered[part] = struct{}{}
	}
	for _, part := range strings.Split(current, "\x01") {
		if _, ok := covered[part]; !ok {
			return false
		}
	}
	return true
}

func splitReviewSignatureVersion(sig string) (string, string) {
	if rest, ok := strings.CutPrefix(sig, "v2:"); ok {
		return "v2", rest
	}
	return "v1", sig
}

func idleReviewEpisodeKey(prURL string, idleSince time.Time) string {
	sum := sha256.Sum256([]byte(idleSince.UTC().Format(time.RFC3339Nano)))
	return fmt.Sprintf("idle-review:%s:%x", prURL, sum)
}

func idleReviewHandoffKey(prURL string, idleSince time.Time) string {
	sum := sha256.Sum256([]byte(idleSince.UTC().Format(time.RFC3339Nano)))
	return fmt.Sprintf("idle-review-handoff:%s:%x", prURL, sum)
}

func (m *Manager) clearIdleReviewEpisodeLocked(ctx context.Context, prURL string) error {
	if !m.clearIdleReviewPRStateLocked(prURL) {
		return nil
	}
	return m.persistPRSignaturesLocked(ctx, prURL)
}

// clearIdleReviewEpisodeIdentityLocked removes only the stale decision's
// episode. A newer idle episode may already have persisted its own budget or
// handoff while this caller was waiting for react.mu; those keys must survive.
func (m *Manager) clearIdleReviewEpisodeIdentityLocked(ctx context.Context, prURL string, idleSince time.Time) error {
	changed := false
	attemptKey := idleReviewEpisodeKey(prURL, idleSince)
	if _, ok := m.react.attempts[attemptKey]; ok {
		delete(m.react.attempts, attemptKey)
		changed = true
	}
	handoffKey := idleReviewHandoffKey(prURL, idleSince)
	if _, ok := m.react.handoffs[handoffKey]; ok {
		delete(m.react.handoffs, handoffKey)
		changed = true
	}
	if !changed {
		return nil
	}
	return m.persistPRSignaturesLocked(ctx, prURL)
}

// clearIdleReviewStateForSession is called directly by the authoritative
// activity reducer on an idle -> non-idle transition. It prevents stale
// attempt/handoff episode keys from waiting indefinitely for another SCM
// refresh and bounds the durable ledger for long-lived sessions.
func (m *Manager) clearIdleReviewStateForSession(ctx context.Context, id domain.SessionID) error {
	prs, err := m.store.ListPRsBySession(ctx, id)
	if err != nil {
		return err
	}
	seen := map[string]struct{}{}
	var errs []error
	for _, pr := range prs {
		prURL := strings.TrimSpace(pr.URL)
		if prURL == "" {
			continue
		}
		if _, ok := seen[prURL]; ok {
			continue
		}
		seen[prURL] = struct{}{}
		m.react.mu.Lock()
		if !m.react.loaded[prURL] {
			if err := m.loadPRSignaturesLocked(ctx, prURL); err != nil {
				errs = append(errs, err)
				m.react.mu.Unlock()
				continue
			}
			m.react.loaded[prURL] = true
		}
		if m.clearIdleReviewPRStateLocked(prURL) {
			errs = append(errs, m.persistPRSignaturesLocked(ctx, prURL))
		}
		m.react.mu.Unlock()
	}
	return errors.Join(errs...)
}

// clearIdleReviewPRStateLocked removes only idle/stuck-episode state. Normal
// review/CI/conflict delivery signatures remain intact. Caller holds react.mu.
func (m *Manager) clearIdleReviewPRStateLocked(prURL string) bool {
	changed := false
	for key := range m.react.attempts {
		if strings.HasPrefix(key, "idle-review:"+prURL+":") {
			delete(m.react.attempts, key)
			changed = true
		}
	}
	for key := range m.react.handoffs {
		if strings.HasPrefix(key, "idle-review-handoff:"+prURL+":") ||
			strings.HasPrefix(key, "review-failure:"+prURL+":") {
			delete(m.react.handoffs, key)
			changed = true
		}
	}
	return changed
}

// reviewFetchFailureSignature is the durable identity of an idle PR overlay
// whose review threads could not be refreshed. It deliberately excludes Seen:
// notification dedup must not promote an escalation hash into agent-delivery
// proof. A changed head or pipeline decision opens a new failure episode.
func reviewFetchFailureSignature(o ports.SCMObservation) string {
	head := firstSCMNonEmpty(o.Review.HeadSHA, o.PR.HeadSHA)
	decision := domain.ReviewDecision(o.Review.Decision)
	overlay := decision == domain.ReviewChangesRequest || decision == domain.ReviewRequired || decision == domain.ReviewApproved ||
		domain.Mergeability(o.Mergeability.State) == domain.MergeMergeable || domain.CIState(o.CI.Summary) == domain.CIFailing
	if !overlay {
		return ""
	}
	return strings.Join([]string{head, string(decision), o.Mergeability.State, o.CI.Summary}, "\x00")
}

// cleanupMergedSession runs the resource-owning session manager before the
// terminal lifecycle write. Tests and minimal embedders that do not wire a
// cleaner retain the reducer-only behavior and simply mark the row terminal.
func (m *Manager) cleanupMergedSession(ctx context.Context, id domain.SessionID, prURL string) error {
	shouldClean := false
	if err := m.mutate(ctx, id, func(cur domain.SessionRecord, _ time.Time) (domain.SessionRecord, bool) {
		if cur.IsTerminated || cur.Metadata.MergedCleanupPending {
			return cur, false
		}
		cur.Metadata.MergedCleanupPending = true
		cur.Metadata.MergedCleanupPRURL = prURL
		if cur.Activity.State != domain.ActivityRateLimited {
			shouldClean = true
		}
		return cur, true
	}); err != nil {
		return err
	}
	if !shouldClean {
		return nil
	}
	_, err := m.runMergedCleanup(ctx, id)
	if errors.Is(err, errMergedCleanupRateLimited) {
		return nil
	}
	return err
}

func (m *Manager) runMergedCleanup(ctx context.Context, id domain.SessionID) (bool, error) {
	// Persist the terminal reservation before external I/O. A concurrent
	// rate-limit hook therefore has a deterministic winner: if it lands first,
	// reservation refuses cleanup; if reservation lands first, the hook sees a
	// terminal row and cannot rewrite it. The durable pending latch remains set
	// until resource cleanup succeeds, so a daemon restart can replay failures.
	lease, eligible, err := m.reserveMergedCleanup(ctx, id)
	if err != nil || !eligible {
		return false, err
	}
	m.mu.Lock()
	cleaner := m.mergedCleaner
	m.mu.Unlock()
	if cleaner != nil {
		cleaned, err := cleaner.CleanupMergedSession(ctx, id, lease)
		if err != nil || !cleaned {
			return false, err
		}
	}
	return m.markMergedCleanupComplete(ctx, id, lease)
}

// RetryMergedCleanup replays a durable cleanup latch discovered by the SCM
// poller. It is intentionally independent of a fresh PR observation: terminal
// PRs drop out of provider open-PR lists, while teardown failures must remain
// retryable across polls and daemon restarts.
func (m *Manager) RetryMergedCleanup(ctx context.Context, id domain.SessionID) error {
	rec, ok, err := m.store.GetSession(ctx, id)
	if err != nil || !ok || !rec.Metadata.MergedCleanupPending {
		return err
	}
	if rec.Activity.State == domain.ActivityRateLimited {
		return nil
	}
	intent, err := m.mergedCleanupNotificationIntent(ctx, rec)
	if err != nil {
		return err
	}
	completed, err := m.runMergedCleanup(ctx, id)
	if err != nil {
		if errors.Is(err, errMergedCleanupRateLimited) {
			return nil
		}
		return err
	}
	if !completed {
		return nil
	}
	m.emitNotification(ctx, intent)
	return nil
}

func (m *Manager) mergedCleanupNotificationIntent(ctx context.Context, rec domain.SessionRecord) (*ports.NotificationIntent, error) {
	prs, err := m.store.ListPRsBySession(ctx, rec.ID)
	if err != nil {
		return nil, err
	}
	var trigger *domain.PullRequest
	for i := range prs {
		if prs[i].URL == rec.Metadata.MergedCleanupPRURL {
			trigger = &prs[i]
			break
		}
	}
	if trigger == nil {
		return nil, nil
	}
	o := ports.SCMObservation{
		ObservedAt: trigger.ObservedAt,
		Provider:   trigger.Provider,
		Repo:       trigger.Repo,
		PR: ports.SCMPRObservation{
			URL:          trigger.URL,
			HTMLURL:      trigger.HTMLURL,
			Number:       trigger.Number,
			Title:        trigger.Title,
			SourceBranch: trigger.SourceBranch,
			TargetBranch: trigger.TargetBranch,
			Merged:       trigger.Merged,
			Closed:       trigger.Closed,
		},
	}
	return m.notificationIntentForSCM(rec, o), nil
}

// ApplyReviewResult reacts to a completed AO-internal review pass after the
// review service has persisted the run result. It mirrors ApplyPRObservation:
// no change_log reads and only lifecycle side effects plus the durable
// simplification activity receipt.
func (m *Manager) ApplyReviewResult(ctx context.Context, workerID domain.SessionID, r ReviewResult) (ReviewDeliveryOutcome, error) {
	if r.Verdict != domain.VerdictChangesRequested || r.DeliveredAt != nil {
		return ReviewDeliveryNoop, nil
	}
	rec, ok, err := m.store.GetSession(ctx, workerID)
	if err != nil || !ok {
		return ReviewDeliveryNoop, err
	}
	if rec.IsTerminated || rec.Activity.State.PausesAutomation() {
		return ReviewDeliveryNoop, nil
	}
	if m.guard == nil {
		return ReviewDeliveryNoop, nil
	}
	msg := fmt.Sprintf("[AO reviewer] AO's internal code reviewer submitted a review.\n\nPR: %s\nVerdict: %s", domain.SanitizeControlChars(r.PRURL), domain.SanitizeControlChars(string(r.Verdict)))
	var contract strings.Builder
	if err := m.writeDesignContractDispatch(ctx, &contract, rec.Metadata.WorkspacePath, r.PRURL); err != nil {
		return ReviewDeliveryNoop, err
	}
	msg += contract.String()
	var policy strings.Builder
	writeReviewDispatchPolicy(&policy, r)
	msg += policy.String()
	if r.GithubReviewID != "" {
		safeReviewID := domain.SanitizeControlChars(r.GithubReviewID)
		msg += fmt.Sprintf("\nGitHub review: %s", safeReviewID)
		msg += fmt.Sprintf("\n\nOnce you have addressed it, reply on GitHub review %s with how you addressed it, then resolve the review comment threads you addressed.", safeReviewID)
	}
	if body := actionableReviewBody(r); body != "" {
		msg += "\n\n" + reviewBodyLabel(r) + ":\n" + domain.SanitizeControlChars(body)
	}
	key := "review:" + r.PRURL + ":ao:" + r.RunID
	sig := strings.Join([]string{r.TargetSHA, r.RunID, r.GithubReviewID, r.Body}, "\x00")
	if strings.TrimSpace(r.TargetSHA) == "" {
		return ReviewDeliveryNoop, nil
	}
	fences := []ports.PRReactionFence{{PRURL: r.PRURL, SessionID: workerID, HeadSHA: r.TargetSHA}}
	outcome, err := m.sendOnce(ctx, workerID, r.PRURL, key, sig, fences, msg, reviewMaxNudge)
	if err != nil {
		return ReviewDeliveryNoop, err
	}
	if outcome == sendOnceSuppressed {
		// Suppressed by the just-in-time guard (worker went terminated/needs-
		// input): the review feedback did not reach the worker, so leave the run
		// undelivered to re-fire on the next observation.
		return ReviewDeliveryNoop, nil
	}
	if err := m.emitSimplificationRound(ctx, rec, r); err != nil {
		return ReviewDeliveryNoop, err
	}
	return ReviewDeliverySent, nil
}

// sessionComplete reports whether the session has reached the multi-PR
// completion bar: at least one PR merged and no PR still open. A session with no
// PRs, or with any open PR, is not complete.
func (m *Manager) sessionComplete(ctx context.Context, id domain.SessionID) (bool, error) {
	prs, err := m.store.ListPRsBySession(ctx, id)
	if err != nil {
		return false, err
	}
	merged := false
	for _, pr := range prs {
		if !pr.Merged && !pr.Closed {
			return false, nil
		}
		if pr.Merged {
			merged = true
		}
	}
	return merged, nil
}

// prBlockedByOpenParent reports whether the PR at prURL is stacked on top of
// another still-open PR in the same session — i.e. its target branch is the
// source branch of a sibling open PR. Such a PR is not the bottom of its stack
// and is exempt from merge-conflict nudges. Branch facts are read from the
// store, which the observer has already updated for this observation.
func (m *Manager) prBlockedByOpenParent(ctx context.Context, id domain.SessionID, prURL string) (bool, error) {
	prs, err := m.store.ListPRsBySession(ctx, id)
	if err != nil {
		return false, err
	}
	openSources := make(map[string]bool, len(prs))
	for _, pr := range prs {
		if !pr.Merged && !pr.Closed && pr.SourceBranch != "" {
			openSources[pr.SourceBranch] = true
		}
	}
	for _, pr := range prs {
		if pr.URL == prURL {
			return pr.TargetBranch != "" && openSources[pr.TargetBranch], nil
		}
	}
	return false, nil
}

// ApplySCMObservation is the provider-neutral lifecycle entrypoint used by the
// SCM observer. The existing reaction logic still operates on PRObservation, so
// lifecycle performs the compatibility projection internally instead of leaking
// the old PR DTO back into the observer/provider boundary.
func (m *Manager) ApplySCMObservation(ctx context.Context, id domain.SessionID, o ports.SCMObservation) error {
	if !o.Fetched {
		return nil
	}
	if err := m.ApplyPRObservation(ctx, id, scmToPRObservation(o)); err != nil {
		return err
	}
	intent, err := m.notificationIntentForCurrentSCM(ctx, id, o)
	if err != nil {
		return err
	}
	m.emitNotification(ctx, intent)
	return nil
}

func (m *Manager) notificationIntentForCurrentSCM(ctx context.Context, id domain.SessionID, o ports.SCMObservation) (*ports.NotificationIntent, error) {
	// Serialize the session snapshot with activity transitions so ready-to-merge
	// notifications do not race against a simultaneous waiting_input update.
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok, err := m.store.GetSession(ctx, id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	return m.notificationIntentForSCM(rec, o), nil
}

func (m *Manager) notificationIntentForSCM(rec domain.SessionRecord, o ports.SCMObservation) *ports.NotificationIntent {
	prURL := firstSCMNonEmpty(o.PR.URL, o.PR.HTMLURL)
	base := ports.NotificationIntent{
		SessionID:          rec.ID,
		ProjectID:          rec.ProjectID,
		PRURL:              prURL,
		CreatedAt:          timeOr(o.ObservedAt, m.clock()),
		SessionDisplayName: rec.DisplayName,
		PRNumber:           o.PR.Number,
		PRTitle:            o.PR.Title,
		PRSourceBranch:     o.PR.SourceBranch,
		PRTargetBranch:     o.PR.TargetBranch,
		Provider:           o.Provider,
		Repo:               o.Repo,
	}
	if o.PR.Merged {
		base.Type = domain.NotificationPRMerged
		return &base
	}
	if o.PR.Closed {
		base.Type = domain.NotificationPRClosedUnmerged
		return &base
	}
	// Merge readiness deliberately has a narrower bot-review policy than agent
	// feedback routing. Do not change that provider policy here, but also do not
	// tell the user a PR is ready in the same observation that routes actionable
	// anchored feedback to its worker.
	if rec.IsTerminated || rec.Activity.State.NeedsInput() || !scmObservationIsReadyToMerge(o) ||
		hasUnresolvedComments(scmToPRObservation(o).Comments) {
		return nil
	}
	base.Type = domain.NotificationReadyToMerge
	return &base
}

func scmObservationIsReadyToMerge(o ports.SCMObservation) bool {
	return scmready.IsReadyToMerge(o)
}

func scmToPRObservation(o ports.SCMObservation) ports.PRObservation {
	pr := ports.PRObservation{
		Fetched:      o.Fetched,
		URL:          firstSCMNonEmpty(o.PR.URL, o.PR.HTMLURL),
		Number:       o.PR.Number,
		Title:        o.PR.Title,
		SourceBranch: o.PR.SourceBranch,
		TargetBranch: o.PR.TargetBranch,
		HeadSHA:      o.PR.HeadSHA,
		Draft:        o.PR.Draft,
		Merged:       o.PR.Merged,
		Closed:       o.PR.Closed,
		CI:           domain.CIState(o.CI.Summary),
		Review:       domain.ReviewDecision(o.Review.Decision),
		Mergeability: domain.Mergeability(o.Mergeability.State),
	}
	if pr.CI == "" {
		pr.CI = domain.CIUnknown
	}
	if pr.Review == "" {
		pr.Review = domain.ReviewNone
	}
	if pr.Mergeability == "" {
		pr.Mergeability = domain.MergeUnknown
	}
	if o.CI.RerunRequested {
		// The observer has already durably bounded and requested a provider
		// rerun. Keep the session in a non-mergeable CI state without dispatching
		// an agent to chase the stale failed job.
		pr.CI = domain.CIPending
	}
	checkCommit := firstSCMNonEmpty(o.CI.HeadSHA, o.PR.HeadSHA)
	for _, ch := range o.CI.FailedChecks {
		if o.CI.RerunRequested {
			break
		}
		status := domain.PRCheckStatus(ch.Status)
		if status == "" {
			status = domain.PRCheckFailed
		}
		logTail := ch.LogTail
		if logTail == "" {
			logTail = o.CI.FailureLogTail
		}
		pr.Checks = append(pr.Checks, ports.PRCheckObservation{
			Name:       ch.Name,
			CommitHash: checkCommit,
			Status:     status,
			URL:        ch.URL,
			LogTail:    logTail,
		})
	}
	for _, th := range o.Review.Threads {
		if th.Resolved {
			continue
		}
		anchored := strings.TrimSpace(th.Path) != "" && th.Line > 0
		for _, c := range th.Comments {
			// Thread bot-ness is aggregate metadata and may describe a bot-started
			// thread that later receives human feedback. Suppress only bot-authored
			// chatter without an anchor; human comments are always actionable.
			if c.IsBot && !anchored {
				continue
			}
			pr.Comments = append(pr.Comments, ports.PRCommentObservation{
				ID:       c.ID,
				ThreadID: th.ID,
				Author:   c.Author,
				File:     th.Path,
				Line:     th.Line,
				Body:     c.Body,
				URL:      c.URL,
				Resolved: th.Resolved,
			})
		}
	}
	return pr
}

// ApplyTrackerFacts reacts to a fetched Tracker issue observation. It owns the
// issue-driven side of session lifecycle and the initial bot-mention nudge;
// it does NOT persist tracker rows (the future Tracker observer in #35 owns
// the read-side persistence path).
//
// Reactions today:
//   - Issue terminal (state == done or cancelled) → MarkTerminated. The
//     reducer is idempotent — repeat observations on an already-terminated
//     session are no-ops because MarkTerminated skips when IsTerminated.
//   - Assignee changed → log only. No session-state reaction yet; the policy
//     for "assignee changed away from AO" is reserved for the write-side work
//     tracked by #40.
//   - New bot comment → one-time nudge using the same sendOnce + dedup
//     signature pattern as the SCM lane. Dedup is in-memory only for now;
//     cross-restart persistence lands with the Tracker observer (issue #35)
//     when issue-row signature storage is on the table.
func (m *Manager) ApplyTrackerFacts(ctx context.Context, id domain.SessionID, o ports.TrackerObservation) error {
	if !o.Fetched {
		return nil
	}
	rec, ok, err := m.store.GetSession(ctx, id)
	if err != nil || !ok {
		return err
	}
	if isTerminalTrackerState(o.Issue.State) {
		if rec.Activity.State == domain.ActivityRateLimited {
			return nil
		}
		return m.markTerminatedUnlessRateLimited(ctx, id)
	}
	if rec.IsTerminated || rec.Activity.State.PausesAutomation() {
		return nil
	}
	if o.Changed.Assignee {
		slog.Default().Info("lifecycle: tracker issue assignee changed",
			"session", id, "issue", o.Issue.URL, "assignee", o.Issue.Assignee)
	}
	if o.Changed.Comments {
		bodies, ids := newBotCommentContent(o.Comments)
		if len(ids) > 0 {
			msg := "A bot left a new comment on your tracker issue. Address it and update the session."
			if joined := strings.Join(bodies, "\n\n"); strings.TrimSpace(joined) != "" {
				msg += "\n\n" + joined
			}
			// Empty prURL routes sendOnce through its in-memory-only branch:
			// the PR-row signature load/persist is skipped, so the dedup
			// survives only for the lifetime of this Manager. Cross-restart
			// persistence ships with #35.
			_, err := m.sendOnce(ctx, id, "", "tracker-bot:"+o.Issue.URL, strings.Join(ids, ","), nil, msg, 0)
			return err
		}
	}
	return nil
}

func isTerminalTrackerState(state domain.NormalizedIssueState) bool {
	return state == domain.IssueDone || state == domain.IssueCancelled
}

func newBotCommentContent(comments []ports.TrackerCommentObservation) ([]string, []string) {
	bodies := make([]string, 0, len(comments))
	ids := make([]string, 0, len(comments))
	for _, c := range comments {
		if !c.IsBot {
			continue
		}
		// Both an ID and a body are required: ID anchors the dedup
		// signature (an empty ID collapses to "" which collides with
		// the zero value of m.react.seen[key] and silently suppresses
		// the nudge), and a body is what we actually need to surface
		// to the agent.
		if c.ID == "" || strings.TrimSpace(c.Body) == "" {
			continue
		}
		bodies = append(bodies, c.Body)
		ids = append(ids, c.ID)
	}
	return bodies, ids
}

func firstSCMNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// prIdentity renders a short, sanitized PR identity ("PR #123 \"Title\"
// (feat/x → main)") for nudge messages so an agent in a multi-PR session can
// tell which PR — and where in a stack — a nudge refers to. Title and branch
// names are provider-controlled and reach the agent's live pane, so both are
// control-char sanitized. Falls back to "your PR" when the number is unknown.
func prIdentity(o ports.PRObservation) string {
	if o.Number <= 0 {
		return "your PR"
	}
	id := fmt.Sprintf("PR #%d", o.Number)
	if o.Title != "" {
		id += fmt.Sprintf(" %q", domain.SanitizeControlChars(o.Title))
	}
	if o.SourceBranch != "" && o.TargetBranch != "" {
		id += fmt.Sprintf(" (%s → %s)", domain.SanitizeControlChars(o.SourceBranch), domain.SanitizeControlChars(o.TargetBranch))
	}
	return id
}

func hasUnresolvedComments(comments []ports.PRCommentObservation) bool {
	for _, c := range comments {
		if !c.Resolved {
			return true
		}
	}
	return false
}

func failedPRChecks(checks []ports.PRCheckObservation) []ports.PRCheckObservation {
	failed := make([]ports.PRCheckObservation, 0, len(checks))
	for _, ch := range checks {
		if ch.Status == domain.PRCheckFailed {
			failed = append(failed, ch)
		}
	}
	return failed
}

func ciFailureSignature(checks []ports.PRCheckObservation) string {
	parts := make([]string, 0, len(checks))
	for _, ch := range checks {
		parts = append(parts, strings.Join([]string{ch.Name, ch.CommitHash, string(ch.Status), ch.URL, ch.LogTail}, "\x00"))
	}
	return strings.Join(parts, "\x01")
}

func formatCIFailureMessage(checks []ports.PRCheckObservation) string {
	var msg strings.Builder
	msg.WriteString("CI is failing on your PR.\n")
	for _, ch := range checks {
		name := domain.SanitizeControlChars(ch.Name)
		if strings.TrimSpace(name) == "" {
			name = "unnamed check"
		}
		status := domain.SanitizeControlChars(string(ch.Status))
		if strings.TrimSpace(status) == "" {
			status = "failed"
		}
		fmt.Fprintf(&msg, "\nFailed: %s (%s)", name, status)
		if ch.URL != "" {
			fmt.Fprintf(&msg, "\nFailure URL: %s", domain.SanitizeControlChars(ch.URL))
		}
		if ch.LogTail != "" {
			// LogTail is raw CI job output; sanitize before it reaches the
			// agent's live pane so embedded escape sequences can't drive the
			// terminal (the dedup signature stays on the raw bytes). The fence
			// grows to contain embedded backtick fences without mutating logs.
			tail := domain.SanitizeControlChars(ch.LogTail)
			fence := markdownCodeFence(tail)
			lineCount := len(strings.Split(tail, "\n"))
			lineLabel := "lines"
			if lineCount == 1 {
				lineLabel = "line"
			}
			fmt.Fprintf(&msg, "\n\nLog tail (last %d %s):\n%s\n%s\n%s", lineCount, lineLabel, fence, tail, fence)
		}
		msg.WriteString("\n")
	}
	msg.WriteString("\nUse the included log tail and failure URL first; fetch full CI logs only if you need additional context. Fix the issues and push again.")
	return msg.String()
}

func unresolvedReviewComments(comments []ports.PRCommentObservation) []ports.PRCommentObservation {
	unresolved := make([]ports.PRCommentObservation, 0, len(comments))
	for _, c := range comments {
		if c.Resolved {
			continue
		}
		unresolved = append(unresolved, c)
	}
	return unresolved
}

func reviewCommentsSignature(comments []ports.PRCommentObservation) string {
	parts := make([]string, 0, len(comments))
	for _, c := range comments {
		// Count and provider IDs alone are not enough: a reviewer may replace
		// one unresolved thread with another at the same count, or edit an
		// existing comment in place. Hash the provider-neutral actionable
		// content while retaining stable IDs when available. Links are omitted
		// because providers may refresh them without changing the feedback.
		semantic := strings.Join([]string{
			strings.TrimSpace(c.ThreadID),
			strings.TrimSpace(c.ID),
			strings.TrimSpace(c.Author),
			c.File,
			fmt.Sprint(c.Line),
			c.Body,
		}, "\x00")
		sum := sha256.Sum256([]byte(semantic))
		parts = append(parts, fmt.Sprintf("%x", sum))
	}
	if len(parts) == 0 {
		return ""
	}
	sort.Strings(parts)
	return "v2:" + strings.Join(parts, "\x01")
}

func formatReviewCommentsMessage(comments []ports.PRCommentObservation) string {
	if len(comments) == 0 {
		return "A reviewer left feedback on your PR. Address it and push. Fetch the review details only if you need additional context beyond what AO has provided here."
	}
	var msg strings.Builder
	fmt.Fprintf(&msg, "The following %d unresolved review comment(s) are on your PR as of just now. You should not need to re-fetch this data unless you need additional context.\n", len(comments))
	for i, c := range comments {
		location := "(general)"
		if c.File != "" {
			location = domain.SanitizeControlChars(c.File)
			if c.Line > 0 {
				location = fmt.Sprintf("%s:%d", location, c.Line)
			}
		}
		author := domain.SanitizeControlChars(c.Author)
		if strings.TrimSpace(author) == "" {
			author = "unknown reviewer"
		}
		// Comment bodies are attacker-influenced (anyone who can comment on the
		// PR) and get pasted into the agent's live pane; strip control/escape
		// chars before formatting them.
		body := domain.SanitizeControlChars(c.Body)
		fmt.Fprintf(&msg, "\n%d. %s (@%s):\n%s", i+1, location, author, body)
		if c.URL != "" {
			fmt.Fprintf(&msg, "\n   %s", domain.SanitizeControlChars(c.URL))
		}
		if c.ThreadID != "" {
			fmt.Fprintf(&msg, "\n   Thread ID: %s", domain.SanitizeControlChars(c.ThreadID))
		}
		msg.WriteString("\n")
	}
	msg.WriteString("\nAddress each comment and push fixes. Use the thread ID to resolve each thread directly after pushing when available. You should not need to re-fetch review data unless you need additional context beyond what is provided here.")
	return msg.String()
}

func markdownCodeFence(s string) string {
	maxRun := 0
	run := 0
	for _, r := range s {
		if r == '`' {
			run++
			if run > maxRun {
				maxRun = run
			}
			continue
		}
		run = 0
	}
	if maxRun < 3 {
		return "```"
	}
	return strings.Repeat("`", maxRun+1)
}

// sendOnceOutcome tells a caller whether a nudge is accounted for (actually
// sent, or already covered by dedup state) versus suppressed by the just-in-time
// session guard. It matters for review delivery: a suppressed nudge must NOT be
// stamped delivered, or the feedback is lost when the session later unblocks.
type sendOnceOutcome int

const (
	// sendOnceAccounted: the message was sent, or a prior identical send is
	// already recorded (dedup hit) or the attempt budget is spent. In every
	// case the caller may treat the nudge as delivered — nothing more to do.
	sendOnceAccounted sendOnceOutcome = iota
	// sendOnceSuppressed: the just-in-time guard skipped the paste because the
	// session is terminated or awaiting the user (blocked/waiting_input). The
	// message did NOT reach the worker; the caller must not mark it delivered so
	// it re-fires on the next observation once the session is workable again.
	sendOnceSuppressed
)

func (m *Manager) sendOnce(ctx context.Context, id domain.SessionID, prURL, key, sig string, fences []ports.PRReactionFence, msg string, maxAttempts int) (sendOnceOutcome, error) {
	ready, err := m.ensurePRDesignContractDelivered(ctx, id, prURL)
	if err != nil {
		return sendOnceSuppressed, err
	}
	if !ready {
		return sendOnceSuppressed, nil
	}
	if m.guard == nil {
		return sendOnceAccounted, nil
	}
	m.react.mu.Lock()
	defer m.react.mu.Unlock()

	reservationStore, durableReservation := m.store.(reactionReservationStore)
	reservationToken := ""
	attempts := 0
	var priorSeen string
	var priorSeenPresent bool
	var priorAttempts int
	var priorAttemptsPresent bool
	if prURL != "" && durableReservation {
		// The durable CAS is deliberately first. In particular, do not trust a
		// rehydrated seen entry before Reserve has had a chance to surface a
		// crash-surviving started reservation as unknown delivery.
		reservationToken = uuid.NewString()
		now := m.clock()
		reservation, err := reservationStore.ReservePRReaction(
			ctx, prURL, key, sig, maxAttempts, reservationToken,
			fences, now, now.Add(reactionReservationLease),
		)
		if err != nil {
			return sendOnceAccounted, err
		}
		switch reservation.Status {
		case ports.PRReactionAccounted, ports.PRReactionExhausted:
			if reservation.Signature != "" {
				m.react.seen[key] = reservation.Signature
			}
			m.react.attempts[key] = reservation.Attempts
			return sendOnceAccounted, nil
		case ports.PRReactionBusy, ports.PRReactionStale:
			// No pane write is proven. Review callers must not stamp their run
			// delivered while another owner is only preparing a send or while
			// the observation fence is stale.
			return sendOnceSuppressed, nil
		case ports.PRReactionUncertain:
			// The prior owner may have crossed the pane boundary. Account it
			// fail-closed, surface one durable operator handoff, and keep reducing
			// sibling reactions; there is intentionally no automatic recovery.
			return sendOnceAccounted, m.handoffUncertainPRReactionLocked(ctx, id, prURL, key, reservation.Signature)
		case ports.PRReactionReserved:
			// Continue to the final exact-head/session fence immediately before
			// the guard is allowed to attempt a pane write.
		default:
			return sendOnceAccounted, fmt.Errorf("reserve PR reaction %s/%q: unexpected status %q", prURL, key, reservation.Status)
		}
		if !m.react.loaded[prURL] {
			if err := m.loadPRSignaturesLocked(ctx, prURL); err != nil {
				_, releaseErr := reservationStore.ReleasePRReaction(ctx, prURL, key, reservationToken)
				return sendOnceSuppressed, errors.Join(err, releaseErr)
			}
			m.react.loaded[prURL] = true
		}
		priorSeen, priorSeenPresent = m.react.seen[key]
		priorAttempts, priorAttemptsPresent = m.react.attempts[key]
		startedAt := m.clock()
		started, err := reservationStore.StartPRReaction(ctx, prURL, key, reservationToken, startedAt, startedAt.Add(reactionReservationLease))
		if err != nil {
			// Start is transactional: an error proves the pane boundary was not
			// crossed. Free the still-reserved token so a later poll need not wait
			// for its lease to expire.
			_, releaseErr := reservationStore.ReleasePRReaction(ctx, prURL, key, reservationToken)
			return sendOnceSuppressed, errors.Join(err, releaseErr)
		}
		if started.Status != ports.PRReactionReserved {
			if started.Status == ports.PRReactionUncertain {
				return sendOnceAccounted, m.handoffUncertainPRReactionLocked(ctx, id, prURL, key, sig)
			}
			// A stale/expired start has not crossed the pane boundary. Free only
			// this token; a newer owner generation is protected by the guard.
			_, releaseErr := reservationStore.ReleasePRReaction(ctx, prURL, key, reservationToken)
			return sendOnceSuppressed, releaseErr
		}
		m.react.seen[key] = started.Signature
		m.react.attempts[key] = started.Attempts
	} else {
		// Non-SQLite embeddings retain the old in-memory fast path and
		// send-then-persist behavior as a graceful degradation floor.
		if prURL != "" && !m.react.loaded[prURL] {
			if err := m.loadPRSignaturesLocked(ctx, prURL); err != nil {
				return sendOnceAccounted, err
			}
			m.react.loaded[prURL] = true
		}
		if m.react.seen[key] == sig {
			return sendOnceAccounted, nil
		}
		attempts = m.react.attempts[key]
		// A bounded attempt budget belongs to one reaction fingerprint, not to
		// the PR for the rest of its lifetime. In particular, an idle worker may
		// have exhausted an earlier review round; new content must still wake it.
		if prior, ok := m.react.seen[key]; ok && prior != sig {
			attempts = 0
		}
		if maxAttempts > 0 && attempts >= maxAttempts {
			return sendOnceAccounted, nil
		}
	}

	rollbackReservation := func() error {
		if reservationToken == "" {
			return nil
		}
		released, err := reservationStore.ReleasePRReaction(ctx, prURL, key, reservationToken)
		if err != nil {
			return err
		}
		if !released {
			return fmt.Errorf("release PR reaction %s/%q: reservation ownership changed", prURL, key)
		}
		if priorSeenPresent {
			m.react.seen[key] = priorSeen
		} else {
			delete(m.react.seen, key)
		}
		if priorAttemptsPresent {
			m.react.attempts[key] = priorAttempts
		} else {
			delete(m.react.attempts, key)
		}
		reservationToken = ""
		return nil
	}
	// The guard re-reads the session immediately before pasting: the caller's
	// NeedsInput() entry check ran before this function's dedup/persist I/O, so
	// a permission hook could have stored blocked (or the session could have
	// terminated) in the meantime. A suppressed write returns SUPPRESSED (not
	// accounted), so a review caller won't stamp it delivered and it re-fires
	// once the session is workable again. A store failure inside the guard also
	// suppresses (fail closed, nothing was written); a messenger failure means
	// the write was attempted and stays accounted, matching the pre-guard
	// behavior.
	outcome, err := m.guard.Nudge(ctx, id, msg)
	if err != nil {
		if outcome != sessionguard.Sent {
			rollbackErr := rollbackReservation()
			return sendOnceSuppressed, errors.Join(err, rollbackErr)
		}
		// Sent+error is explicitly an unknown delivery: the messenger crossed
		// its write boundary and may have landed the bytes. Finalize the durable
		// attempt before surfacing the error; releasing here would permit a
		// restart/concurrent poll to oversend.
		finalizeErr := error(nil)
		if reservationToken != "" {
			committed, commitErr := reservationStore.CommitPRReaction(ctx, prURL, key, reservationToken)
			if commitErr != nil {
				finalizeErr = commitErr
			} else if !committed {
				finalizeErr = fmt.Errorf("commit PR reaction %s/%q: reservation ownership changed", prURL, key)
			}
			reservationToken = ""
		}
		// A review nudge for an idle PR overlay has no separate stuck-state
		// transition to alert the user. Surface a durable, one-time fallback while
		// leaving the unknown agent delivery accounted so it cannot oversend.
		if key == "review:"+prURL {
			rec, eligible, readErr := m.idleReviewFailureSession(ctx, id)
			if readErr != nil {
				return sendOnceAccounted, errors.Join(err, finalizeErr, readErr)
			}
			if eligible {
				obs := ports.SCMObservation{Fetched: true, PR: ports.SCMPRObservation{URL: prURL}}
				handoffErr := m.deliverReviewFailureHandoffLocked(ctx, rec, obs, reviewNudgeSendFailed)
				return sendOnceAccounted, errors.Join(err, finalizeErr, handoffErr)
			}
		}
		return sendOnceAccounted, errors.Join(err, finalizeErr)
	}
	if outcome != sessionguard.Sent {
		rollbackErr := rollbackReservation()
		if outcome == sessionguard.SuppressedAwaitingUser {
			if err := m.handoffSuppressedPendingNudgeLocked(ctx, id, prURL, key, sig); err != nil {
				return sendOnceSuppressed, errors.Join(rollbackErr, err)
			}
		}
		return sendOnceSuppressed, rollbackErr
	}
	if reservationToken != "" {
		committed, err := reservationStore.CommitPRReaction(ctx, prURL, key, reservationToken)
		if err != nil {
			slog.Default().Warn("lifecycle: PR reaction commit is uncertain", "session", id, "pr", prURL, "reaction", key, "err", err)
			return sendOnceAccounted, m.handoffUncertainPRReactionLocked(ctx, id, prURL, key, sig)
		}
		if !committed {
			slog.Default().Warn("lifecycle: PR reaction commit ownership changed", "session", id, "pr", prURL, "reaction", key)
			return sendOnceAccounted, m.handoffUncertainPRReactionLocked(ctx, id, prURL, key, sig)
		}
		return sendOnceAccounted, nil
	}
	// Order: Send → in-memory mutation → durable persist. Sending first means a
	// transient persist failure does NOT swallow a real send (the agent saw the
	// message; subsequent polls in this process suppress re-sends via the
	// in-memory dedup). A persist failure that survives until a daemon restart
	// degrades to one extra nudge — preferred over the inverse (persist before
	// send, then crash mid-call) which would silently lose a real nudge.
	m.react.seen[key] = sig
	m.react.attempts[key] = attempts + 1
	if prURL != "" {
		if err := m.persistPRSignaturesLocked(ctx, prURL); err != nil {
			return sendOnceAccounted, err
		}
	}
	return sendOnceAccounted, nil
}

// ensurePRDesignContractDelivered retries the durable claim barrier before any
// actionable PR reaction. It is deliberately independent of in-memory dedup,
// so a fresh lifecycle manager after daemon restart recovers the obligation.
func (m *Manager) ensurePRDesignContractDelivered(ctx context.Context, id domain.SessionID, prURL string) (bool, error) {
	if strings.TrimSpace(prURL) == "" {
		return true, nil
	}
	store, ok := m.store.(designContractDeliveryStore)
	if !ok {
		return true, nil
	}
	unlockDelivery := designcontract.LockDelivery(prURL)
	defer unlockDelivery()
	delivery, pending, err := store.GetPendingPRDesignContractDelivery(ctx, id, prURL)
	if err != nil || !pending {
		return !pending, err
	}
	rec, exists, err := m.store.GetSession(ctx, id)
	if err != nil || !exists || rec.IsTerminated {
		return false, err
	}
	if err := designcontract.MaterializePR(ctx, rec.Metadata.WorkspacePath, prURL, delivery.Contract); err != nil {
		slog.Debug("claim barrier: design contract projection skipped", "sessionId", id, "prURL", prURL, "error", err)
	}
	m.mu.Lock()
	sender := m.automatedSender
	m.mu.Unlock()
	if sender == nil {
		return false, nil
	}
	message := domain.SanitizeControlChars(designcontract.ClaimReadyMessage(prURL, delivery.Contract, delivery.TaskPrompt))
	if err := sender.SendAutomated(ctx, id, message); err != nil {
		latest, exists, readErr := m.store.GetSession(ctx, id)
		if readErr != nil {
			return false, readErr
		}
		// Unsafe pane states are expected suppression/retry conditions, not
		// observer failures. The confirmed sender may also have just persisted
		// PendingSubmitFingerprint after detecting an unsubmitted large draft.
		// In every case leave the durable claim barrier pending for a safe poll.
		if !exists || latest.IsTerminated || latest.Activity.State.PausesAutomation() || latest.Metadata.PendingSubmitFingerprint != "" {
			return false, nil
		}
		return false, err
	}
	completed, err := store.CompletePRDesignContractDelivery(ctx, id, prURL, delivery.Token, delivery.Revision)
	if err != nil {
		return false, err
	}
	return completed, nil
}

// idleReviewFailureSession is the shared eligibility gate for broken review
// nudge/fetch paths. PR pipeline status overlays activity in the UI, so only an
// idle agent beyond the normal recent-activity window needs this additional
// fallback. Active, terminated, and already-user-owned sessions keep their
// existing behavior.
func (m *Manager) idleReviewFailureSession(ctx context.Context, id domain.SessionID) (domain.SessionRecord, bool, error) {
	rec, ok, err := m.store.GetSession(ctx, id)
	if err != nil || !ok {
		return rec, false, err
	}
	if rec.IsTerminated || rec.Activity.State != domain.ActivityIdle {
		return rec, false, nil
	}
	return rec, m.clock().Sub(rec.Activity.LastActivityAt) >= m.window, nil
}

func (m *Manager) deliverReviewFailureHandoffLocked(ctx context.Context, rec domain.SessionRecord, o ports.SCMObservation, reason string) error {
	prURL := firstSCMNonEmpty(o.PR.URL, o.PR.HTMLURL)
	current, err := m.idleReviewEpisodeCurrent(ctx, rec.ID, rec.Activity.LastActivityAt)
	if err != nil {
		return err
	}
	if !current {
		return m.clearIdleReviewEpisodeIdentityLocked(ctx, prURL, rec.Activity.LastActivityAt)
	}
	handoffKey, err := m.canonicalIdleReviewHandoffLocked(ctx, prURL, rec.Activity.LastActivityAt)
	if err != nil {
		return err
	}
	body := "Pull request review feedback could not be delivered to the idle agent. Open the pull request and review the outstanding feedback."
	if reason == reviewFetchFailed {
		body = "Pull request review feedback could not be refreshed while the agent is idle. Open the pull request and verify the outstanding review threads."
	}
	intent := ports.NotificationIntent{
		Type:               domain.NotificationNeedsInput,
		SessionID:          rec.ID,
		ProjectID:          rec.ProjectID,
		PRURL:              prURL,
		CreatedAt:          m.clock(),
		TitleOverride:      "PR review feedback needs attention",
		BodyOverride:       body,
		SessionDisplayName: rec.DisplayName,
		PRNumber:           o.PR.Number,
		PRTitle:            o.PR.Title,
		PRSourceBranch:     o.PR.SourceBranch,
		PRTargetBranch:     o.PR.TargetBranch,
		Provider:           o.Provider,
		Repo:               o.Repo,
	}
	return m.deliverHumanHandoffLocked(ctx, prURL, handoffKey, reason, intent, nil)
}

// canonicalIdleReviewHandoffLocked returns the one per-PR + idle-episode latch
// shared by successful-snapshot decisions and every fetch/send failure path.
// It also collapses #111's condition-specific durable keys before either path
// checks dedup, so upgrade order cannot produce a duplicate notification.
// Caller holds react.mu and has loaded prURL.
func (m *Manager) canonicalIdleReviewHandoffLocked(ctx context.Context, prURL string, idleSince time.Time) (string, error) {
	// Every fetch/send/snapshot failure in one positive idle episode shares this
	// one latch. Reason/head/signature changes are diagnostic metadata, never a
	// new permission to notify the human again.
	handoffKey := idleReviewHandoffKey(prURL, idleSince)
	// #111 persisted condition-specific latches, and an earlier #52 iteration
	// added idle time while still keeping the condition in the identity. Migrate
	// every old per-condition shape for this PR onto the canonical episode key
	// without duplicating delivery. The old schema had no recoverable episode
	// identity, so preserving its notified latch is the only fail-safe migration.
	canonical, canonicalExists := m.react.handoffs[handoffKey]
	migrated := false
	for oldKey, legacy := range m.react.handoffs {
		if !strings.HasPrefix(oldKey, "review-failure:"+prURL+":") {
			continue
		}
		delete(m.react.handoffs, oldKey)
		if !canonicalExists || (canonical.Outcome != humanHandoffNotified && legacy.Outcome == humanHandoffNotified) {
			canonical = legacy
			canonicalExists = true
		}
		migrated = true
	}
	if migrated {
		m.react.handoffs[handoffKey] = canonical
		if err := m.persistPRSignaturesLocked(ctx, prURL); err != nil {
			return "", err
		}
	}
	return handoffKey, nil
}

// handoffSuppressedPendingNudgeLocked surfaces a PR condition that could not
// reach the worker because a previously delivered prompt is still pending in
// its editor. The condition remains unaccounted (no seen signature or attempt)
// so it can reach the worker after the latch clears; the human fallback is
// durably latched by condition signature so refresh/replay cannot duplicate it.
func (m *Manager) handoffSuppressedPendingNudgeLocked(ctx context.Context, id domain.SessionID, prURL, key, sig string) error {
	if prURL == "" {
		return nil
	}
	rec, ok, err := m.store.GetSession(ctx, id)
	if err != nil || !ok {
		return err
	}
	if rec.Metadata.PendingSubmitFingerprint == "" {
		return nil
	}
	sum := sha256.Sum256([]byte(key + "\x00" + sig))
	handoffKey := fmt.Sprintf("nudge-handoff:%s:%x", prURL, sum)
	intent := ports.NotificationIntent{
		Type:               domain.NotificationNeedsInput,
		SessionID:          rec.ID,
		ProjectID:          rec.ProjectID,
		PRURL:              prURL,
		CreatedAt:          m.clock(),
		TitleOverride:      "PR feedback is waiting",
		BodyOverride:       "New pull request feedback could not be sent to the agent while editor input is pending. Open the pull request and submit or clear the pending input.",
		SessionDisplayName: rec.DisplayName,
	}
	return m.deliverHumanHandoffLocked(ctx, prURL, handoffKey, suppressedNudgeNotificationFailed, intent, nil)
}

// handoffUncertainPRReactionLocked records the fail-closed liveness tradeoff
// for a started reservation whose pane delivery cannot be proven. The
// reservation remains permanently uncertain so AO never auto-resends it; a
// durable, successful notification is latched once and subsequent polls treat
// the reaction as accounted. Failed notification/persistence returns a
// handoff-pending error after sibling reactions have run, keeping the
// observer's semantic acknowledgement stale so the next poll retries only the
// idempotent handoff path (never the pane write).
// Caller holds react.mu.
func (m *Manager) handoffUncertainPRReactionLocked(ctx context.Context, id domain.SessionID, prURL, key, signature string) error {
	if prURL == "" {
		return nil
	}
	if !m.react.loaded[prURL] {
		if err := m.loadPRSignaturesLocked(ctx, prURL); err != nil {
			return errors.Join(errPRReactionHandoffPending, fmt.Errorf("load reaction state: %w", err))
		}
		m.react.loaded[prURL] = true
	}
	rec, ok, err := m.store.GetSession(ctx, id)
	if err != nil {
		return errors.Join(errPRReactionHandoffPending, fmt.Errorf("resolve session %s: %w", id, err))
	}
	if !ok {
		return fmt.Errorf("%w: resolve session %s: not found", errPRReactionHandoffPending, id)
	}
	sum := sha256.Sum256([]byte(key + "\x00" + signature))
	handoffKey := fmt.Sprintf("reaction-uncertain:%s:%x", prURL, sum)
	intent := ports.NotificationIntent{
		Type:               domain.NotificationNeedsInput,
		SessionID:          rec.ID,
		ProjectID:          rec.ProjectID,
		PRURL:              prURL,
		CreatedAt:          m.clock(),
		TitleOverride:      "PR delivery outcome needs verification",
		BodyOverride:       "AO cannot determine whether a lifecycle message reached the agent. It will not resend automatically. Inspect the agent pane and pull request, then resolve the outstanding work manually.",
		SessionDisplayName: rec.DisplayName,
	}
	if err := m.deliverHumanHandoffLocked(ctx, prURL, handoffKey, prReactionDeliveryUncertain, intent, nil); err != nil {
		return errors.Join(errPRReactionHandoffPending, err)
	}
	return nil
}

// loadPRSignaturesLocked merges any previously persisted reaction-dedup state
// for prURL into the in-memory maps. Caller must hold m.react.mu.
func (m *Manager) loadPRSignaturesLocked(ctx context.Context, prURL string) error {
	raw, err := m.store.GetPRLastNudgeSignature(ctx, prURL)
	if err != nil {
		return err
	}
	if raw == "" {
		return nil
	}
	// A corrupt persisted payload must not crash the lifecycle write path;
	// the worst case from a swallow is re-firing a nudge once.
	var p reactionPayload
	_ = json.Unmarshal([]byte(raw), &p)
	for k, v := range p.Seen {
		if _, ok := m.react.seen[k]; !ok {
			m.react.seen[k] = v
		}
	}
	for k, v := range p.Attempts {
		if cur, ok := m.react.attempts[k]; !ok || v > cur {
			m.react.attempts[k] = v
		}
	}
	for k, v := range p.Handoffs {
		if _, ok := m.react.handoffs[k]; !ok {
			m.react.handoffs[k] = v
		}
	}
	return nil
}

// persistPRSignaturesLocked serialises every reaction-dedup entry whose key
// references prURL and writes the JSON payload back via the store. Caller must
// hold m.react.mu. A failed persist surfaces upward so the in-memory mutation
// (which the messenger already acted on) is not silently divergent from disk.
func (m *Manager) persistPRSignaturesLocked(ctx context.Context, prURL string) error {
	payload := reactionPayload{Seen: map[string]string{}, Attempts: map[string]int{}, Handoffs: map[string]humanHandoffOutcome{}}
	for k, v := range m.react.seen {
		if reactionKeyTargetsPR(k, prURL) {
			payload.Seen[k] = v
		}
	}
	for k, v := range m.react.attempts {
		if reactionKeyTargetsPR(k, prURL) {
			payload.Attempts[k] = v
		}
	}
	for k, v := range m.react.handoffs {
		if reactionKeyTargetsPR(k, prURL) {
			payload.Handoffs[k] = v
		}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return m.store.UpdatePRLastNudgeSignature(ctx, prURL, string(raw))
}

// reactionKeyTargetsPR matches the "<type>:<url>[:<extra>]" reaction keys used
// by ApplyPRObservation. Anchoring on the second colon-delimited segment keeps
// PR-specific keys grouped with the row that survives a restart.
func reactionKeyTargetsPR(key, prURL string) bool {
	if prURL == "" {
		return false
	}
	parts := strings.SplitN(key, ":", 2)
	if len(parts) != 2 {
		return false
	}
	rest := parts[1]
	return rest == prURL || strings.HasPrefix(rest, prURL+":")
}
