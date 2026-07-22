// Package review is the daemon's HTTP-facing code-review service boundary. The
// core orchestration lives in internal/review; this layer is the thin contract
// the API controller depends on and delegates to the engine, so the same engine
// can also back a future in-process CLI trigger.
package review

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/designcontract"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/lifecycle"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	reviewcore "github.com/aoagents/agent-orchestrator/backend/internal/review"
)

// errRunSuperseded marks a run that became terminal before its result arrived.
// SubmitMany skips this expected per-run race while continuing to surface all
// other submission failures.
var errRunSuperseded = errors.New("review: run is no longer running")

// ErrInvalid and ErrNotFound re-export the engine sentinels so the HTTP
// controller maps service failures to 422/404 without importing the core.
var (
	ErrInvalid             = reviewcore.ErrInvalid
	ErrNotFound            = reviewcore.ErrNotFound
	ErrAgentBinaryNotFound = ports.ErrAgentBinaryNotFound
)

// Manager is the reviews surface the HTTP controller depends on.
type Manager interface {
	Trigger(ctx context.Context, workerID domain.SessionID) (reviewcore.TriggerResult, error)
	Cancel(ctx context.Context, workerID domain.SessionID) (reviewcore.CancelResult, error)
	SubmitOne(ctx context.Context, workerID domain.SessionID, review SubmittedReview) (domain.ReviewRun, error)
	SubmitMany(ctx context.Context, workerID domain.SessionID, reviews []SubmittedReview) ([]domain.ReviewRun, error)
	List(ctx context.Context, workerID domain.SessionID) (reviewcore.SessionReviews, error)
}

// Service is the API-facing review service. It delegates to the core engine.
type Service struct {
	engine    *reviewcore.Engine
	store     Store
	lifecycle Reducer
	deflector FindingDeflector
	clock     func() time.Time
	newID     func() string
}

var _ Manager = (*Service)(nil)

// Store is the review_run persistence surface owned by the service submit path.
type Store interface {
	GetReviewRun(ctx context.Context, id string) (domain.ReviewRun, bool, error)
	CompleteReviewRunWithFindings(ctx context.Context, id string, verdict domain.ReviewVerdict, body, githubReviewID, simplificationClass string, findings []domain.ReviewFinding) (bool, error)
	RefreshReviewRunSimplificationClass(ctx context.Context, id string) (bool, error)
	MarkReviewRunDelivered(ctx context.Context, id string, deliveredAt time.Time) (bool, error)
	MarkReviewRunDeflectedReviewCleared(ctx context.Context, id string, clearedAt time.Time) (bool, error)
	ListReviewRunsByBatch(ctx context.Context, sessionID domain.SessionID, batchID string) ([]domain.ReviewRun, error)
	ListPRsBySession(ctx context.Context, id domain.SessionID) ([]domain.PullRequest, error)
	ListReviewRunsBySession(ctx context.Context, id domain.SessionID) ([]domain.ReviewRun, error)
	ListReviewRunsByPR(ctx context.Context, prURL string) ([]domain.ReviewRun, error)
	ListReviewFindingsByRun(ctx context.Context, runID string) ([]domain.ReviewFinding, error)
	ListReviewFindingsBySession(ctx context.Context, id domain.SessionID) ([]domain.ReviewFinding, error)
	ClaimReviewFindingIssueAction(ctx context.Context, id, token string, leaseUntil, staleBefore time.Time) (bool, error)
	CompleteReviewFindingIssueAction(ctx context.Context, id, token, issueURL string) (bool, error)
	ReleaseReviewFindingIssueAction(ctx context.Context, id, token string) error
	ClaimReviewFindingThreadAction(ctx context.Context, id, token string, leaseUntil, staleBefore time.Time) (bool, error)
	CompleteReviewFindingThreadAction(ctx context.Context, id, token, replyID string) (bool, error)
	ReleaseReviewFindingThreadAction(ctx context.Context, id, token string) error
	GetSession(ctx context.Context, id domain.SessionID) (domain.SessionRecord, bool, error)
	GetProject(ctx context.Context, id string) (domain.ProjectRecord, bool, error)
}

// FindingDeflector is the provider mutation surface for opt-in out-of-scope
// review deflection.
type FindingDeflector interface {
	ports.SCMIssueFiler
	ports.SCMFindingThreadDeflector
	ports.SCMReviewDismisser
}

// Reducer is the lifecycle reaction boundary used after a review result has
// been persisted.
type Reducer interface {
	ApplyReviewResult(ctx context.Context, workerID domain.SessionID, result lifecycle.ReviewResult) (lifecycle.ReviewDeliveryOutcome, error)
	ApplyReviewBatch(ctx context.Context, workerID domain.SessionID, batchID string, results []lifecycle.ReviewResult) (lifecycle.ReviewDeliveryOutcome, error)
}

// Option customizes the review service.
type Option func(*Service)

// WithLifecycleReducer wires post-submit review delivery through lifecycle.
func WithLifecycleReducer(r Reducer) Option {
	return func(s *Service) { s.lifecycle = r }
}

// WithClock overrides the service clock for tests.
func WithClock(clock func() time.Time) Option {
	return func(s *Service) { s.clock = clock }
}

// WithIDSource overrides action lease token generation for tests.
func WithIDSource(newID func() string) Option {
	return func(s *Service) { s.newID = newID }
}

// WithFindingDeflector enables provider mutations when the project review
// policy opts in.
func WithFindingDeflector(deflector FindingDeflector) Option {
	return func(s *Service) { s.deflector = deflector }
}

// New wraps a core review engine as the API-facing service.
func New(engine *reviewcore.Engine, store Store, opts ...Option) *Service {
	s := &Service{
		engine: engine,
		store:  store,
		clock:  func() time.Time { return time.Now().UTC() },
		newID:  uuid.NewString,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Trigger starts (or reuses) a review pass for a worker's PR.
func (s *Service) Trigger(ctx context.Context, workerID domain.SessionID) (reviewcore.TriggerResult, error) {
	return s.engine.Trigger(ctx, workerID)
}

// Coordinate advances the automatic, head-bound review loop for one durable
// SCM observation. It is daemon-internal and intentionally not part of the
// HTTP Manager contract.
func (s *Service) Coordinate(ctx context.Context, workerID domain.SessionID, obs ports.SCMObservation) (reviewcore.CoordinateResult, error) {
	result, err := s.engine.Coordinate(ctx, workerID, obs)
	if err != nil || s.store == nil || s.lifecycle == nil {
		return result, err
	}
	run := result.Run
	if run.Status != domain.ReviewRunComplete || run.Verdict != domain.VerdictChangesRequested || run.DeliveredAt != nil {
		return result, nil
	}
	runs := []domain.ReviewRun{run}
	if run.BatchID != "" {
		runs, err = s.store.ListReviewRunsByBatch(ctx, workerID, run.BatchID)
		if err != nil {
			return result, err
		}
	}
	delivered, err := s.deliverSubmitted(ctx, workerID, runs)
	if err != nil {
		return result, err
	}
	for _, replayed := range delivered {
		if replayed.ID == result.Run.ID {
			result.Run = replayed
			break
		}
	}
	return result, nil
}

// CoordinateReview implements the SCM observer's restart-safe coordination
// hook while keeping the richer result internal to this service.
func (s *Service) CoordinateReview(ctx context.Context, workerID domain.SessionID, obs ports.SCMObservation) error {
	_, err := s.Coordinate(ctx, workerID, obs)
	return err
}

// Cancel stops the live reviewer pane and marks running review passes as failed.
func (s *Service) Cancel(ctx context.Context, workerID domain.SessionID) (reviewcore.CancelResult, error) {
	return s.engine.Cancel(ctx, workerID)
}

// SubmittedReview is one review result supplied by the reviewer CLI.
type SubmittedReview struct {
	RunID          string
	Verdict        domain.ReviewVerdict
	Body           string
	GithubReviewID string
	Findings       []SubmittedFinding
}

// SubmittedFinding is reviewer-authored structured taxonomy for one blocking
// finding. ClassTag is a stable kebab-case root-cause category.
type SubmittedFinding struct {
	File              string
	ClassTag          string
	RootCauseNote     string
	ProposedInvariant string
	ThreadID          string
	Body              string
	OutOfScope        bool
}

// Submit records a reviewer's result for a specific worker review pass.
func (s *Service) Submit(ctx context.Context, workerID domain.SessionID, runID string, verdict domain.ReviewVerdict, body, githubReviewID string) (domain.ReviewRun, error) {
	return s.SubmitOne(ctx, workerID, SubmittedReview{
		RunID:          runID,
		Verdict:        verdict,
		Body:           body,
		GithubReviewID: githubReviewID,
	})
}

// SubmitOne records one structured result while preserving the singular API's
// error contract when the run was already superseded.
func (s *Service) SubmitOne(ctx context.Context, workerID domain.SessionID, review SubmittedReview) (domain.ReviewRun, error) {
	runs, err := s.SubmitMany(ctx, workerID, []SubmittedReview{review})
	if err != nil {
		return domain.ReviewRun{}, err
	}
	if len(runs) == 0 {
		return domain.ReviewRun{}, fmt.Errorf("%w: no review result submitted", ErrInvalid)
	}
	return runs[0], nil
}

// SubmitMany records one reviewer CLI submission containing results for one or
// more PR-scoped runs. Delivery is scoped to the runs in this submission, so a
// missing/stuck result for another PR in the same trigger cannot block feedback.
func (s *Service) SubmitMany(ctx context.Context, workerID domain.SessionID, reviews []SubmittedReview) ([]domain.ReviewRun, error) {
	if workerID == "" {
		return nil, fmt.Errorf("%w: worker session id is required", ErrInvalid)
	}
	if len(reviews) == 0 {
		return nil, fmt.Errorf("%w: at least one review result is required", ErrInvalid)
	}
	if s.store == nil {
		return nil, fmt.Errorf("review service store is not configured")
	}
	runs := make([]domain.ReviewRun, 0, len(reviews))
	for _, review := range reviews {
		run, err := s.submitOne(ctx, workerID, review)
		if err != nil {
			if errors.Is(err, errRunSuperseded) {
				continue
			}
			return nil, err
		}
		runs = append(runs, run)
	}
	if s.lifecycle == nil {
		return runs, nil
	}
	delivered, err := s.deliverSubmitted(ctx, workerID, runs)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]domain.ReviewRun, len(delivered))
	for _, run := range delivered {
		byID[run.ID] = run
	}
	for i, run := range runs {
		if deliveredRun, ok := byID[run.ID]; ok {
			runs[i] = deliveredRun
		}
	}
	return runs, nil
}

func (s *Service) submitOne(ctx context.Context, workerID domain.SessionID, review SubmittedReview) (domain.ReviewRun, error) {
	runID := review.RunID
	verdict := review.Verdict
	body := review.Body
	githubReviewID := review.GithubReviewID
	if runID == "" {
		return domain.ReviewRun{}, fmt.Errorf("%w: review run id is required", ErrInvalid)
	}
	if !verdict.Valid() {
		return domain.ReviewRun{}, fmt.Errorf("%w: verdict must be %q or %q", ErrInvalid, domain.VerdictApproved, domain.VerdictChangesRequested)
	}
	if verdict == domain.VerdictChangesRequested && body == "" {
		return domain.ReviewRun{}, fmt.Errorf("%w: a changes_requested review requires a body", ErrInvalid)
	}
	if verdict != domain.VerdictChangesRequested && len(review.Findings) > 0 {
		return domain.ReviewRun{}, fmt.Errorf("%w: findings require a changes_requested verdict", ErrInvalid)
	}
	run, ok, err := s.store.GetReviewRun(ctx, runID)
	if err != nil {
		return domain.ReviewRun{}, err
	}
	if !ok {
		return domain.ReviewRun{}, fmt.Errorf("%w: review run %q", ErrNotFound, runID)
	}
	if run.SessionID != workerID {
		return domain.ReviewRun{}, fmt.Errorf("%w: review run %q does not belong to worker %q", ErrInvalid, runID, workerID)
	}

	switch run.Status {
	case domain.ReviewRunRunning:
		submitted := review.Findings
		if verdict == domain.VerdictChangesRequested && len(submitted) == 0 {
			submitted = inferFindings(body)
			if len(submitted) == 0 && reviewcore.BodyHasBlockingFindings(body) {
				submitted = []SubmittedFinding{{
					ClassTag: "unclassified-blocking", RootCauseNote: strings.TrimSpace(body), Body: strings.TrimSpace(body),
				}}
			}
		}
		findings, err := s.buildFindings(ctx, run, submitted)
		if err != nil {
			return domain.ReviewRun{}, err
		}
		history, err := s.store.ListReviewFindingsBySession(ctx, run.SessionID)
		if err != nil {
			return domain.ReviewRun{}, err
		}
		simplificationClass := reviewcore.SimplificationClassForRun(append(history, findings...), run.ID)
		updated, err := s.store.CompleteReviewRunWithFindings(ctx, run.ID, verdict, body, githubReviewID, simplificationClass, findings)
		if err != nil {
			return domain.ReviewRun{}, err
		}
		if !updated {
			return domain.ReviewRun{}, fmt.Errorf("%w: review run %q is not running", errRunSuperseded, runID)
		}
		run.Status = domain.ReviewRunComplete
		run.Verdict = verdict
		run.Body = body
		run.GithubReviewID = githubReviewID
		run.SimplificationClass = simplificationClass
	case domain.ReviewRunComplete:
		if run.Verdict != verdict {
			return domain.ReviewRun{}, fmt.Errorf("%w: review run %q already recorded verdict %q", ErrInvalid, runID, run.Verdict)
		}
		if body != "" && body != run.Body {
			return domain.ReviewRun{}, fmt.Errorf("%w: review run %q already recorded a different body", ErrInvalid, runID)
		}
		if githubReviewID != "" && githubReviewID != run.GithubReviewID {
			return domain.ReviewRun{}, fmt.Errorf("%w: review run %q already recorded GitHub review id %q", ErrInvalid, runID, run.GithubReviewID)
		}
	case domain.ReviewRunDelivered:
		return run, nil
	default:
		return domain.ReviewRun{}, fmt.Errorf("%w: review run %q is not running", errRunSuperseded, runID)
	}
	if err := s.deflectOutOfScope(ctx, run); err != nil {
		return domain.ReviewRun{}, err
	}
	return run, nil
}

func (s *Service) deliverSubmitted(ctx context.Context, workerID domain.SessionID, runs []domain.ReviewRun) ([]domain.ReviewRun, error) {
	// Retry partially-completed out-of-scope mutations before deciding which
	// findings belong in the worker fix dispatch. This path is also used by the
	// SCM coordinator after restart, so a transient provider failure cannot turn
	// a deferred finding back into fix work.
	for i, run := range runs {
		if run.Status == domain.ReviewRunComplete && run.Verdict == domain.VerdictChangesRequested {
			if err := s.deflectOutOfScope(ctx, run); err != nil {
				return nil, err
			}
			refreshed, err := s.refreshSimplificationClass(ctx, run)
			if err != nil {
				return nil, err
			}
			runs[i] = refreshed
		}
	}
	deliverable, err := s.deliverableRuns(ctx, workerID, runs)
	if err != nil {
		return nil, err
	}
	if len(deliverable) == 0 {
		return nil, nil
	}
	results, err := s.reviewResults(ctx, workerID, deliverable)
	if err != nil {
		return nil, err
	}
	var outcome lifecycle.ReviewDeliveryOutcome
	if len(results) == 1 && results[0].BatchID == "" {
		outcome, err = s.lifecycle.ApplyReviewResult(ctx, workerID, results[0])
	} else {
		outcome, err = s.lifecycle.ApplyReviewBatch(ctx, workerID, results[0].BatchID, results)
	}
	if err != nil {
		return nil, err
	}
	if outcome != lifecycle.ReviewDeliverySent {
		return nil, nil
	}
	deliveredAt := s.clock()
	delivered := make([]domain.ReviewRun, 0, len(deliverable))
	for _, run := range deliverable {
		updated, err := s.store.MarkReviewRunDelivered(ctx, run.ID, deliveredAt)
		if err != nil {
			return nil, err
		}
		if updated {
			run.Status = domain.ReviewRunDelivered
			run.DeliveredAt = &deliveredAt
			if run.SimplificationClass != "" {
				run.SimplificationDispatchedAt = &deliveredAt
			}
			delivered = append(delivered, run)
		}
	}
	return delivered, nil
}

func (s *Service) deliverableRuns(ctx context.Context, workerID domain.SessionID, runs []domain.ReviewRun) ([]domain.ReviewRun, error) {
	currentHeads, err := s.currentHeadsByPR(ctx, workerID)
	if err != nil {
		return nil, err
	}
	p2OnlyRoundLimit, err := s.p2OnlyRoundLimit(ctx, workerID)
	if err != nil {
		return nil, err
	}
	deliverable := make([]domain.ReviewRun, 0, len(runs))
	for _, run := range runs {
		if run.Status != domain.ReviewRunComplete || run.Verdict != domain.VerdictChangesRequested || run.DeliveredAt != nil {
			continue
		}
		if run.BatchID != "" && currentHeads[run.PRURL] != run.TargetSHA {
			continue
		}
		findings, err := s.store.ListReviewFindingsByRun(ctx, run.ID)
		if err != nil {
			return nil, err
		}
		if len(findings) > 0 && !hasActionableFinding(findings) {
			continue
		}
		if p2OnlyRoundLimit > 0 {
			reached, err := s.p2OnlyReviewStreakReached(ctx, run, p2OnlyRoundLimit)
			if err != nil {
				return nil, err
			}
			if reached {
				continue
			}
		}
		deliverable = append(deliverable, run)
	}
	return deliverable, nil
}

func (s *Service) p2OnlyRoundLimit(ctx context.Context, workerID domain.SessionID) (int, error) {
	session, ok, err := s.store.GetSession(ctx, workerID)
	if err != nil || !ok {
		return 0, err
	}
	project, ok, err := s.store.GetProject(ctx, string(session.ProjectID))
	if err != nil || !ok {
		return 0, err
	}
	return project.Config.ReviewPolicy.P2OnlyRoundLimit, nil
}

func (s *Service) p2OnlyReviewStreakReached(ctx context.Context, current domain.ReviewRun, limit int) (bool, error) {
	if limit <= 0 || !p2OnlyChangesRequested(current) {
		return false, nil
	}
	runs, err := s.store.ListReviewRunsByPR(ctx, current.PRURL)
	if err != nil {
		return false, err
	}
	sort.SliceStable(runs, func(i, j int) bool {
		return runs[i].CreatedAt.After(runs[j].CreatedAt)
	})

	started := false
	lastHead := ""
	streak := 0
	for _, run := range runs {
		if !started {
			if run.ID != current.ID {
				continue
			}
			started = true
		}
		if run.TargetSHA != "" && run.TargetSHA == lastHead {
			continue
		}
		lastHead = run.TargetSHA
		if !p2OnlyChangesRequested(run) {
			break
		}
		streak++
		if streak >= limit {
			return true, nil
		}
	}
	return false, nil
}

func p2OnlyChangesRequested(run domain.ReviewRun) bool {
	if run.Status != domain.ReviewRunComplete && run.Status != domain.ReviewRunDelivered {
		return false
	}
	if run.Verdict != domain.VerdictChangesRequested || reviewcore.BodyHasBlockingFindings(run.Body) {
		return false
	}
	body := strings.ToLower(run.Body)
	return strings.Contains(body, "[p2]") || strings.Contains(body, "[p3]")
}

func hasActionableFinding(findings []domain.ReviewFinding) bool {
	for _, finding := range findings {
		if !finding.FullyDeflected() {
			return true
		}
	}
	return false
}

// refreshSimplificationClass derives escalation from the durable ledger after
// deflection. Persisting the derived value before dispatch makes crash replay
// observe the same actionable finding set and clears a pre-deflection class
// that was successfully deferred.
func (s *Service) refreshSimplificationClass(ctx context.Context, run domain.ReviewRun) (domain.ReviewRun, error) {
	updated, err := s.store.RefreshReviewRunSimplificationClass(ctx, run.ID)
	if err != nil {
		return domain.ReviewRun{}, err
	}
	if !updated {
		current, ok, getErr := s.store.GetReviewRun(ctx, run.ID)
		if getErr != nil {
			return domain.ReviewRun{}, getErr
		}
		if !ok {
			return domain.ReviewRun{}, fmt.Errorf("%w: review run %q", ErrNotFound, run.ID)
		}
		return current, nil
	}
	current, ok, err := s.store.GetReviewRun(ctx, run.ID)
	if err != nil {
		return domain.ReviewRun{}, err
	}
	if !ok {
		return domain.ReviewRun{}, fmt.Errorf("%w: review run %q", ErrNotFound, run.ID)
	}
	return current, nil
}

func (s *Service) buildFindings(ctx context.Context, run domain.ReviewRun, submitted []SubmittedFinding) ([]domain.ReviewFinding, error) {
	if len(submitted) == 0 {
		return nil, nil
	}
	runs, err := s.store.ListReviewRunsByPR(ctx, run.PRURL)
	if err != nil {
		return nil, err
	}
	heads := map[string]struct{}{}
	for _, candidate := range runs {
		if candidate.TargetSHA != "" {
			heads[candidate.TargetSHA] = struct{}{}
		}
	}
	round := len(heads)
	if round == 0 {
		round = 1
	}
	findings := make([]domain.ReviewFinding, 0, len(submitted))
	for i, item := range submitted {
		classTag := normalizeClassTag(item.ClassTag)
		if classTag == "" {
			return nil, fmt.Errorf("%w: findings[%d].classTag is required", ErrInvalid, i)
		}
		finding := domain.ReviewFinding{
			ID: fmt.Sprintf("%s:%d", run.ID, i+1), RunID: run.ID,
			SessionID: run.SessionID, PRURL: run.PRURL, Round: round,
			File: strings.TrimSpace(item.File), ClassTag: classTag,
			RootCauseNote: strings.TrimSpace(item.RootCauseNote), ProposedInvariant: strings.TrimSpace(item.ProposedInvariant),
			ThreadID: strings.TrimSpace(item.ThreadID), Body: strings.TrimSpace(item.Body),
			OutOfScope: item.OutOfScope, CreatedAt: s.clock(),
		}
		if finding.RootCauseNote == "" {
			finding.RootCauseNote = finding.Body
		}
		if finding.ProposedInvariant != "" {
			normalized, err := designcontract.NormalizeInvariant(finding.ProposedInvariant)
			if err != nil || finding.OutOfScope {
				reason := "out-of-scope findings cannot propose contract invariants"
				if err != nil {
					reason = err.Error()
				}
				finding.RootCauseNote = strings.TrimSpace(finding.RootCauseNote + " [Invariant proposal rejected: " + reason + ".]")
				finding.ProposedInvariant = ""
			} else {
				finding.ProposedInvariant = normalized
			}
		}
		findings = append(findings, finding)
	}
	return findings, nil
}

func normalizeClassTag(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	dash := false
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			dash = false
		} else if b.Len() > 0 && !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func inferFindings(body string) []SubmittedFinding {
	var findings []SubmittedFinding
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		lower := strings.ToLower(line)
		if !strings.Contains(lower, "[p0]") && !strings.Contains(lower, "[p1]") {
			continue
		}
		classTag := "correctness-invariant"
		switch {
		case strings.Contains(lower, "notify") || strings.Contains(lower, "notification"):
			classTag = "missing-notification-on-broken-path"
		case strings.Contains(lower, "error") || strings.Contains(lower, "fail"):
			classTag = "missing-error-handling"
		case strings.Contains(lower, "race") || strings.Contains(lower, "concurr"):
			classTag = "concurrency-safety"
		case strings.Contains(lower, "nil") || strings.Contains(lower, "null"):
			classTag = "nil-safety"
		case strings.Contains(lower, "auth") || strings.Contains(lower, "permission"):
			classTag = "authorization"
		}
		findings = append(findings, SubmittedFinding{ClassTag: classTag, RootCauseNote: strings.TrimSpace(line), Body: strings.TrimSpace(line)})
	}
	return findings
}

func (s *Service) deflectOutOfScope(ctx context.Context, run domain.ReviewRun) error {
	if s.deflector == nil {
		return nil
	}
	session, ok, err := s.store.GetSession(ctx, run.SessionID)
	if err != nil || !ok {
		return err
	}
	project, ok, err := s.store.GetProject(ctx, string(session.ProjectID))
	if err != nil || !ok || !project.Config.ReviewPolicy.OutOfScopeDeflection {
		return err
	}
	findings, err := s.store.ListReviewFindingsByRun(ctx, run.ID)
	if err != nil {
		return err
	}
	for _, finding := range findings {
		if !finding.OutOfScope {
			continue
		}
		if finding.FullyDeflected() {
			continue
		}
		binding := ports.SCMReviewThreadBinding{
			PRURL: run.PRURL, ReviewID: run.GithubReviewID, ThreadID: finding.ThreadID,
			File: finding.File, Body: finding.Body, ActionKey: finding.ID,
		}
		// Without a provider-confirmed binding there is no safe thread to mutate.
		// Leave the finding actionable instead of silently suppressing it.
		if binding.ThreadID == "" || binding.ReviewID == "" {
			continue
		}
		bound, err := s.deflector.ReviewThreadBound(ctx, binding)
		if err != nil {
			return err
		}
		if !bound {
			continue
		}
		issueURL := finding.DeferredIssueURL
		if issueURL == "" {
			token := s.newID()
			now := s.clock()
			claimed, err := s.store.ClaimReviewFindingIssueAction(ctx, finding.ID, token, now.Add(2*time.Minute), now)
			if err != nil {
				return err
			}
			if !claimed {
				return fmt.Errorf("review finding %s issue action is already in progress", finding.ID)
			}
			title := fmt.Sprintf("review follow-up: %s", strings.ReplaceAll(finding.ClassTag, "-", " "))
			body := fmt.Sprintf("Deferred from %s review round %d.\n\nFile: `%s`\n\nRoot cause: %s\n\n%s", run.PRURL, finding.Round, finding.File, finding.RootCauseNote, finding.Body)
			filed, err := s.deflector.FileDeferredIssue(ctx, ports.SCMDeferredIssueRequest{
				PRURL: run.PRURL, Title: title, Body: body, ActionKey: finding.ID,
			})
			if err != nil {
				_ = s.store.ReleaseReviewFindingIssueAction(ctx, finding.ID, token)
				return err
			}
			issueURL = filed.URL
			stored, err := s.store.CompleteReviewFindingIssueAction(ctx, finding.ID, token, issueURL)
			if err != nil {
				_ = s.store.ReleaseReviewFindingIssueAction(ctx, finding.ID, token)
				return err
			}
			if !stored {
				return fmt.Errorf("review finding %s lost its issue action lease", finding.ID)
			}
		}
		if !finding.ThreadResolved {
			token := s.newID()
			now := s.clock()
			claimed, err := s.store.ClaimReviewFindingThreadAction(ctx, finding.ID, token, now.Add(2*time.Minute), now)
			if err != nil {
				return err
			}
			if !claimed {
				return fmt.Errorf("review finding %s thread action is already in progress", finding.ID)
			}
			binding.IssueURL = issueURL
			resolved, err := s.deflector.DeflectReviewThread(ctx, binding)
			if err != nil {
				_ = s.store.ReleaseReviewFindingThreadAction(ctx, finding.ID, token)
				return err
			}
			if !resolved.Resolved || resolved.ThreadID != finding.ThreadID || resolved.ReplyID == "" {
				_ = s.store.ReleaseReviewFindingThreadAction(ctx, finding.ID, token)
				return fmt.Errorf("review finding %s thread resolution was not confirmed", finding.ID)
			}
			stored, err := s.store.CompleteReviewFindingThreadAction(ctx, finding.ID, token, resolved.ReplyID)
			if err != nil {
				_ = s.store.ReleaseReviewFindingThreadAction(ctx, finding.ID, token)
				return err
			}
			if !stored {
				return fmt.Errorf("review finding %s lost its thread action lease", finding.ID)
			}
		}
	}
	findings, err = s.store.ListReviewFindingsByRun(ctx, run.ID)
	if err != nil {
		return err
	}
	if len(findings) == 0 || hasActionableFinding(findings) {
		return nil
	}
	currentRun, ok, err := s.store.GetReviewRun(ctx, run.ID)
	if err != nil || !ok {
		return err
	}
	if currentRun.DeflectedReviewClearedAt != nil {
		return nil
	}
	cleared, err := s.deflector.DismissReview(ctx, ports.SCMReviewDismissalRequest{
		PRURL: run.PRURL, ReviewID: run.GithubReviewID,
		Message: "All blocking findings were deferred to linked follow-up issues by AO.",
	})
	if err != nil {
		return err
	}
	if !cleared.Cleared {
		return fmt.Errorf("review run %s changes-requested review was not cleared", run.ID)
	}
	if _, err := s.store.MarkReviewRunDeflectedReviewCleared(ctx, run.ID, s.clock()); err != nil {
		return err
	}
	return nil
}

func (s *Service) reviewResults(ctx context.Context, workerID domain.SessionID, runs []domain.ReviewRun) ([]lifecycle.ReviewResult, error) {
	ledgerFindings, err := s.store.ListReviewFindingsBySession(ctx, workerID)
	if err != nil {
		return nil, err
	}
	results := make([]lifecycle.ReviewResult, 0, len(runs))
	for _, run := range runs {
		prFindings := findingsForPR(ledgerFindings, run.PRURL)
		ledger := reviewcore.FindingLedger(prFindings)
		findings, err := s.store.ListReviewFindingsByRun(ctx, run.ID)
		if err != nil {
			return nil, err
		}
		results = append(results, lifecycle.ReviewResult{
			RunID:               run.ID,
			BatchID:             run.BatchID,
			WorkerID:            workerID,
			PRURL:               run.PRURL,
			TargetSHA:           run.TargetSHA,
			Verdict:             run.Verdict,
			Body:                run.Body,
			GithubReviewID:      run.GithubReviewID,
			DeliveredAt:         run.DeliveredAt,
			Findings:            findings,
			Ledger:              ledger,
			SimplificationClass: run.SimplificationClass,
		})
	}
	return results, nil
}

func findingsForPR(findings []domain.ReviewFinding, prURL string) []domain.ReviewFinding {
	out := make([]domain.ReviewFinding, 0, len(findings))
	for _, finding := range findings {
		if finding.PRURL == prURL {
			out = append(out, finding)
		}
	}
	return out
}

func (s *Service) currentHeadsByPR(ctx context.Context, workerID domain.SessionID) (map[string]string, error) {
	prs, err := s.store.ListPRsBySession(ctx, workerID)
	if err != nil {
		return nil, err
	}
	current := make(map[string]string, len(prs))
	for _, pr := range prs {
		current[pr.URL] = pr.HeadSHA
	}
	return current, nil
}

// List returns a worker's review state.
func (s *Service) List(ctx context.Context, workerID domain.SessionID) (reviewcore.SessionReviews, error) {
	return s.engine.List(ctx, workerID)
}
