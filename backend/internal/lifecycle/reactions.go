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

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/scmready"
	"github.com/aoagents/agent-orchestrator/backend/internal/sessionguard"
)

const reviewMaxNudge = 3
const mergeConflictMaxNudge = 1

const reviewRoundCapNotificationFailed = "review-round-cap-notification-failed"
const suppressedNudgeNotificationFailed = "suppressed-nudge-notification-failed"
const reviewNudgeSendFailed = "review-nudge-send-failed"
const reviewFetchFailed = "review-fetch-failed"

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
// Lifecycle treats it as input to the reaction reducer; it does not write the
// review_run row.
type ReviewResult struct {
	RunID          string
	BatchID        string
	WorkerID       domain.SessionID
	PRURL          string
	TargetSHA      string
	Verdict        domain.ReviewVerdict
	Body           string
	GithubReviewID string
	DeliveredAt    *time.Time
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
	if rec.IsTerminated || rec.Activity.State.NeedsInput() {
		return ReviewDeliveryNoop, nil
	}
	if m.guard == nil {
		return ReviewDeliveryNoop, nil
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].PRURL != results[j].PRURL {
			return results[i].PRURL < results[j].PRURL
		}
		return results[i].RunID < results[j].RunID
	})
	var msg strings.Builder
	fmt.Fprintf(&msg, "[AO reviewer] AO's internal code reviewer submitted %d review(s) requesting changes.\n", len(results))
	var sigParts []string
	for i, r := range results {
		fmt.Fprintf(&msg, "\nReview %d\nPR: %s\nVerdict: %s", i+1, domain.SanitizeControlChars(r.PRURL), domain.SanitizeControlChars(string(r.Verdict)))
		if r.TargetSHA != "" {
			fmt.Fprintf(&msg, "\nHead commit: %s", domain.SanitizeControlChars(r.TargetSHA))
		}
		if r.GithubReviewID != "" {
			safeReviewID := domain.SanitizeControlChars(r.GithubReviewID)
			fmt.Fprintf(&msg, "\nGitHub review: %s", safeReviewID)
			fmt.Fprintf(&msg, "\nOnce you have addressed it, reply on GitHub review %s with how you addressed it, then resolve the review comment threads you addressed.", safeReviewID)
		}
		if r.Body != "" {
			fmt.Fprintf(&msg, "\n\nReview body:\n%s\n", domain.SanitizeControlChars(r.Body))
		}
		sigParts = append(sigParts, strings.Join([]string{r.RunID, r.PRURL, r.TargetSHA, r.GithubReviewID, r.Body}, "\x00"))
	}
	anchorPR := results[0].PRURL
	key := "review-batch:" + anchorPR + ":" + batchID
	sig := strings.Join(sigParts, "\x01")
	outcome, err := m.sendOnce(ctx, workerID, anchorPR, key, sig, msg.String(), reviewMaxNudge)
	if err != nil {
		return ReviewDeliveryNoop, err
	}
	if outcome == sendOnceSuppressed {
		// The worker went terminated/needs-input between the entry guard and the
		// paste: nothing reached it, so do NOT let the caller stamp the run
		// delivered — it must re-fire once the session is workable again.
		return ReviewDeliveryNoop, nil
	}
	return ReviewDeliverySent, nil
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
	if rec.IsTerminated || rec.Activity.State.NeedsInput() {
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
		if cur.IsTerminated || cur.Activity.State.NeedsInput() {
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
			return m.cleanupMergedSession(ctx, id, o.URL)
		}
		return nil
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
	if rec.IsTerminated || rec.Activity.State.NeedsInput() {
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

	if o.CI == domain.CIFailing {
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
	if o.Review == domain.ReviewChangesRequest || hasUnresolvedComments(o.Comments) {
		comments := unresolvedReviewComments(o.Comments)
		msg := formatReviewCommentsMessage(comments)
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

	for _, n := range nudges {
		if _, err := m.sendOnce(ctx, id, o.URL, n.key, n.sig, n.msg, n.maxAttempts); err != nil {
			return err
		}
	}
	return nil
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
	return m.deliverReviewFailureHandoffLocked(ctx, rec, o, "review:"+prURL, failureSig, reviewFetchFailed)
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
		shouldClean = true
		cur.Metadata.MergedCleanupPending = true
		cur.Metadata.MergedCleanupPRURL = prURL
		return cur, true
	}); err != nil {
		return err
	}
	if !shouldClean {
		return nil
	}
	return m.runMergedCleanup(ctx, id)
}

func (m *Manager) runMergedCleanup(ctx context.Context, id domain.SessionID) error {
	m.mu.Lock()
	cleaner := m.mergedCleaner
	m.mu.Unlock()
	if cleaner != nil {
		if err := cleaner.CleanupMergedSession(ctx, id); err != nil {
			return err
		}
	}
	return m.markMergedCleanupComplete(ctx, id)
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
	intent, err := m.mergedCleanupNotificationIntent(ctx, rec)
	if err != nil {
		return err
	}
	if err := m.runMergedCleanup(ctx, id); err != nil {
		return err
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
// no change_log reads, no review_run writes, only lifecycle side effects.
func (m *Manager) ApplyReviewResult(ctx context.Context, workerID domain.SessionID, r ReviewResult) (ReviewDeliveryOutcome, error) {
	if r.Verdict != domain.VerdictChangesRequested || r.DeliveredAt != nil {
		return ReviewDeliveryNoop, nil
	}
	rec, ok, err := m.store.GetSession(ctx, workerID)
	if err != nil || !ok {
		return ReviewDeliveryNoop, err
	}
	if rec.IsTerminated || rec.Activity.State.NeedsInput() {
		return ReviewDeliveryNoop, nil
	}
	if m.guard == nil {
		return ReviewDeliveryNoop, nil
	}
	msg := fmt.Sprintf("[AO reviewer] AO's internal code reviewer submitted a review.\n\nPR: %s\nVerdict: %s", domain.SanitizeControlChars(r.PRURL), domain.SanitizeControlChars(string(r.Verdict)))
	if r.GithubReviewID != "" {
		safeReviewID := domain.SanitizeControlChars(r.GithubReviewID)
		msg += fmt.Sprintf("\nGitHub review: %s", safeReviewID)
		msg += fmt.Sprintf("\n\nOnce you have addressed it, reply on GitHub review %s with how you addressed it, then resolve the review comment threads you addressed.", safeReviewID)
	}
	if r.Body != "" {
		msg += "\n\nReview body:\n" + domain.SanitizeControlChars(r.Body)
	}
	key := "review:" + r.PRURL + ":ao:" + r.RunID
	sig := strings.Join([]string{r.TargetSHA, r.RunID, r.GithubReviewID, r.Body}, "\x00")
	outcome, err := m.sendOnce(ctx, workerID, r.PRURL, key, sig, msg, reviewMaxNudge)
	if err != nil {
		return ReviewDeliveryNoop, err
	}
	if outcome == sendOnceSuppressed {
		// Suppressed by the just-in-time guard (worker went terminated/needs-
		// input): the review feedback did not reach the worker, so leave the run
		// undelivered to re-fire on the next observation.
		return ReviewDeliveryNoop, nil
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
	if rec.IsTerminated || rec.Activity.State.NeedsInput() || !scmObservationIsReadyToMerge(o) {
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
		if th.Resolved || th.IsBot {
			continue
		}
		for _, c := range th.Comments {
			if c.IsBot {
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
	if isTerminalTrackerState(o.Issue.State) {
		return m.MarkTerminated(ctx, id)
	}
	rec, ok, err := m.store.GetSession(ctx, id)
	if err != nil || !ok {
		return err
	}
	if rec.IsTerminated || rec.Activity.State.NeedsInput() {
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
			_, err := m.sendOnce(ctx, id, "", "tracker-bot:"+o.Issue.URL, strings.Join(ids, ","), msg, 0)
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
		id := strings.TrimSpace(c.ID)
		threadID := strings.TrimSpace(c.ThreadID)
		if id == "" && threadID == "" {
			continue
		}
		parts = append(parts, threadID+"\x00"+id)
	}
	sort.Strings(parts)
	return strings.Join(parts, "\x01")
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

func (m *Manager) sendOnce(ctx context.Context, id domain.SessionID, prURL, key, sig, msg string, maxAttempts int) (sendOnceOutcome, error) {
	if m.guard == nil {
		return sendOnceAccounted, nil
	}
	m.react.mu.Lock()
	defer m.react.mu.Unlock()

	if prURL != "" && !m.react.loaded[prURL] {
		if err := m.loadPRSignaturesLocked(ctx, prURL); err != nil {
			return sendOnceAccounted, err
		}
		m.react.loaded[prURL] = true
	}

	if m.react.seen[key] == sig {
		return sendOnceAccounted, nil
	}
	attempts := m.react.attempts[key]
	if maxAttempts > 0 && attempts >= maxAttempts {
		return sendOnceAccounted, nil
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
			return sendOnceSuppressed, err
		}
		// A review nudge for an idle PR overlay has no separate stuck-state
		// transition to alert the user. Surface a durable, one-time fallback while
		// leaving the failed send unaccounted so the agent delivery may retry.
		if key == "review:"+prURL {
			rec, eligible, readErr := m.idleReviewFailureSession(ctx, id)
			if readErr != nil {
				return sendOnceAccounted, errors.Join(err, readErr)
			}
			if eligible {
				obs := ports.SCMObservation{Fetched: true, PR: ports.SCMPRObservation{URL: prURL}}
				handoffErr := m.deliverReviewFailureHandoffLocked(ctx, rec, obs, key, sig, reviewNudgeSendFailed)
				return sendOnceAccounted, errors.Join(err, handoffErr)
			}
		}
		return sendOnceAccounted, err
	}
	if outcome != sessionguard.Sent {
		if outcome == sessionguard.SuppressedAwaitingUser {
			if err := m.handoffSuppressedPendingNudgeLocked(ctx, id, prURL, key, sig); err != nil {
				return sendOnceSuppressed, err
			}
		}
		return sendOnceSuppressed, nil
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

func (m *Manager) deliverReviewFailureHandoffLocked(ctx context.Context, rec domain.SessionRecord, o ports.SCMObservation, key, sig, reason string) error {
	prURL := firstSCMNonEmpty(o.PR.URL, o.PR.HTMLURL)
	sum := sha256.Sum256([]byte(reason + "\x00" + key + "\x00" + sig))
	handoffKey := fmt.Sprintf("review-failure:%s:%x", prURL, sum)
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
