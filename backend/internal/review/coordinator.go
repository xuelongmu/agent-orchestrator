package review

import (
	stdctx "context"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/reviewpolicy"
)

// MaxAutomaticReviewRounds is the durable budget for one PR's automatic
// review/fix loop. Manual review remains available after the coordinator stops.
const MaxAutomaticReviewRounds = 6

// RoundCapHandoff owns the human notification and needs-input latch after the
// automatic review budget is exhausted. Returning an error keeps the current
// SCM snapshot unacknowledged so delivery is retried.
type RoundCapHandoff interface {
	ApplyReviewRoundCapHandoff(ctx stdctx.Context, workerID domain.SessionID, obs ports.SCMObservation, round int) error
}

// Automatic reviewer launch failures are retried from durable review_run
// timestamps. The exponential delay prevents a broken harness from being
// relaunched every SCM poll, while the cap guarantees eventual retry cadence.
const (
	AutomaticReviewRetryBaseDelay = 30 * time.Second
	AutomaticReviewRetryMaxDelay  = 15 * time.Minute
)

// CoordinateOutcome describes what the automatic review coordinator decided
// for one authoritative PR observation.
type CoordinateOutcome string

// Automatic review coordination outcomes.
const (
	CoordinateIneligible CoordinateOutcome = "ineligible"
	CoordinateStarted    CoordinateOutcome = "started"
	CoordinateWaiting    CoordinateOutcome = "waiting"
	CoordinateSatisfied  CoordinateOutcome = "satisfied"
	CoordinateExhausted  CoordinateOutcome = "exhausted"
)

// CoordinateResult is intentionally small: durable detail lives in review_run.
// Round is the number of distinct heads already assigned a review pass,
// including a newly started pass.
type CoordinateResult struct {
	Outcome CoordinateOutcome
	Round   int
	Run     domain.ReviewRun
}

// Coordinate advances the automatic review/fix loop for one PR observation.
// review_run is the durable coordinator ledger: its PR URL + target SHA prevents
// duplicate work across polling, concurrent observations, and daemon restarts.
func (e *Engine) Coordinate(ctx stdctx.Context, workerID domain.SessionID, obs ports.SCMObservation) (CoordinateResult, error) {
	if !coordinateEligible(obs) {
		return CoordinateResult{Outcome: CoordinateIneligible}, nil
	}

	prURL := firstNonEmpty(obs.PR.URL, obs.PR.HTMLURL)
	runs, err := e.store.ListReviewRunsByPR(ctx, prURL)
	if err != nil {
		return CoordinateResult{}, err
	}
	head := obs.PR.HeadSHA
	current, hasCurrent, round := coordinateRunState(runs, prURL, head)
	legacyRequired, err := e.legacyReviewFixDeclarationRequired(ctx, runs, prURL, head)
	if err != nil {
		return CoordinateResult{}, err
	}
	// Run the idempotent store gate even when a manual trigger already created
	// a current-head run. The atomic query excludes findings originating at this
	// head, so only older pending work can require and consume the trailer.
	if _, _, err := e.store.AcceptReviewFixCommit(ctx, workerID, prURL, head, obs.PR.HeadCommitMessage, legacyRequired, e.clock()); err != nil {
		return CoordinateResult{}, err
	}
	if reviewpolicy.HasCurrentHeadHumanApproval(obs.Review.Reviews, head) {
		return CoordinateResult{Outcome: CoordinateSatisfied, Round: round, Run: current}, nil
	}
	if hasCurrent {
		if current.Status == domain.ReviewRunFailed || current.Status == domain.ReviewRunCancelled {
			retryDelay := automaticReviewRetryDelay(coordinateRetryAttempts(runs, prURL, head))
			if e.clock().Before(current.CreatedAt.Add(retryDelay)) {
				return CoordinateResult{Outcome: CoordinateWaiting, Round: round, Run: current}, nil
			}
		} else {
			outcome := CoordinateWaiting
			findings, err := e.store.ListReviewFindingsByRun(ctx, current.ID)
			if err != nil {
				return CoordinateResult{}, err
			}
			allDeflected := currentRunFindingsDeflected(findings, current.ID)
			if current.Status != domain.ReviewRunRunning && current.Verdict == domain.VerdictApproved && !reviewpolicy.HasUnresolvedCodexP0P1(obs.Review.Threads) {
				outcome = CoordinateSatisfied
			} else if current.Status != domain.ReviewRunRunning && current.Verdict == domain.VerdictChangesRequested && allDeflected && !reviewpolicy.HasUnresolvedCodexP0P1(obs.Review.Threads) {
				outcome = CoordinateSatisfied
			} else if current.Status != domain.ReviewRunRunning && current.Verdict == domain.VerdictChangesRequested && !ReviewBodyHasBlockingFindings(current.Body) && !reviewpolicy.HasUnresolvedCodexP0P1(obs.Review.Threads) {
				outcome = CoordinateSatisfied
			}
			return CoordinateResult{Outcome: outcome, Round: round, Run: current}, nil
		}
	}
	if round >= MaxAutomaticReviewRounds {
		if e.roundCapHandoff != nil {
			if err := e.roundCapHandoff.ApplyReviewRoundCapHandoff(ctx, workerID, obs, round); err != nil {
				return CoordinateResult{}, err
			}
		}
		return CoordinateResult{Outcome: CoordinateExhausted, Round: round}, nil
	}
	triggered, err := e.trigger(ctx, workerID, prURL)
	if err != nil {
		return CoordinateResult{}, err
	}
	if !triggered.Created {
		// A concurrent coordinator may have won the per-worker trigger lock after
		// our initial ledger read. Reflect its durable current-head round rather
		// than returning the stale pre-lock count.
		if !hasCurrent && triggered.Run.PRURL == prURL && triggered.Run.TargetSHA == head {
			round++
		}
		return CoordinateResult{Outcome: CoordinateWaiting, Round: round, Run: triggered.Run}, nil
	}
	if !hasCurrent {
		round++
	}
	return CoordinateResult{Outcome: CoordinateStarted, Round: round, Run: triggered.Run}, nil
}

func (e *Engine) legacyReviewFixDeclarationRequired(ctx stdctx.Context, runs []domain.ReviewRun, prURL, head string) (bool, error) {
	var latest domain.ReviewRun
	found := false
	for _, run := range runs {
		if run.PRURL != prURL || run.TargetSHA == "" || run.TargetSHA == head || run.Verdict == domain.VerdictNone {
			continue
		}
		if !found || run.CreatedAt.After(latest.CreatedAt) {
			latest, found = run, true
		}
	}
	if !found || latest.Verdict != domain.VerdictChangesRequested || !ReviewBodyHasBlockingFindings(latest.Body) {
		return false, nil
	}
	findings, err := e.store.ListReviewFindingsByRun(ctx, latest.ID)
	if err != nil {
		return false, err
	}
	return len(findings) == 0, nil
}

func currentRunFindingsDeflected(findings []domain.ReviewFinding, runID string) bool {
	found := false
	for _, finding := range findings {
		if finding.RunID != runID {
			continue
		}
		found = true
		if !finding.FullyDeflected() {
			return false
		}
	}
	return found
}

func coordinateEligible(obs ports.SCMObservation) bool {
	prURL := firstNonEmpty(obs.PR.URL, obs.PR.HTMLURL)
	return obs.Fetched &&
		prURL != "" &&
		obs.PR.HeadSHA != "" &&
		!obs.PR.Draft &&
		!obs.PR.Merged &&
		!obs.PR.Closed &&
		domain.CIState(obs.CI.Summary) == domain.CIPassing &&
		obs.CI.HeadSHA == obs.PR.HeadSHA &&
		obs.Review.HeadSHA == obs.PR.HeadSHA &&
		!obs.Review.Partial
}

func coordinateRunState(runs []domain.ReviewRun, prURL, currentHead string) (domain.ReviewRun, bool, int) {
	distinctHeads := make(map[string]struct{})
	var current domain.ReviewRun
	hasCurrent := false
	for _, run := range runs {
		if run.PRURL != prURL || run.TargetSHA == "" {
			continue
		}
		distinctHeads[run.TargetSHA] = struct{}{}
		if run.TargetSHA == currentHead && (!hasCurrent || run.CreatedAt.After(current.CreatedAt)) {
			current = run
			hasCurrent = true
		}
	}
	return current, hasCurrent, len(distinctHeads)
}

func coordinateRetryAttempts(runs []domain.ReviewRun, prURL, head string) int {
	attempts := 0
	for _, run := range runs {
		if run.PRURL == prURL && run.TargetSHA == head &&
			(run.Status == domain.ReviewRunFailed || run.Status == domain.ReviewRunCancelled) {
			attempts++
		}
	}
	return attempts
}

func automaticReviewRetryDelay(attempts int) time.Duration {
	delay := AutomaticReviewRetryBaseDelay
	for attempt := 1; attempt < attempts && delay < AutomaticReviewRetryMaxDelay; attempt++ {
		delay *= 2
		if delay > AutomaticReviewRetryMaxDelay {
			return AutomaticReviewRetryMaxDelay
		}
	}
	return delay
}

// A changes-requested result from an older reviewer may predate the priority
// contract. Untagged findings therefore fail closed. Once the reviewer uses the
// required tags, P2/P3-only feedback is explicitly non-blocking.
// ReviewBodyHasBlockingFindings applies the durable priority policy used by
// both coordination and fallback finding persistence. Untagged
// changes-requested feedback fails closed as blocking.
func ReviewBodyHasBlockingFindings(body string) bool {
	body = strings.ToLower(body)
	if reviewpolicy.HasP0OrP1(body) {
		return true
	}
	return !strings.Contains(body, "[p2]") && !strings.Contains(body, "[p3]")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
