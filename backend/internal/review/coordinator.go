package review

import (
	stdctx "context"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/reviewpolicy"
)

// MaxAutomaticReviewRounds is the durable budget for one PR's automatic
// review/fix loop. Manual review remains available after the coordinator stops.
const MaxAutomaticReviewRounds = 6

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

	runs, err := e.store.ListReviewRunsBySession(ctx, workerID)
	if err != nil {
		return CoordinateResult{}, err
	}
	prURL := firstNonEmpty(obs.PR.URL, obs.PR.HTMLURL)
	head := obs.PR.HeadSHA
	current, hasCurrent, round := coordinateRunState(runs, prURL, head)
	if reviewpolicy.HasCurrentHeadHumanApproval(obs.Review.Reviews, head) {
		return CoordinateResult{Outcome: CoordinateSatisfied, Round: round, Run: current}, nil
	}
	if hasCurrent {
		outcome := CoordinateWaiting
		if current.Status != domain.ReviewRunRunning && current.Verdict == domain.VerdictApproved && !reviewpolicy.HasUnresolvedCodexP0P1(obs.Review.Threads) {
			outcome = CoordinateSatisfied
		} else if current.Status != domain.ReviewRunRunning && current.Verdict == domain.VerdictChangesRequested && !reviewBodyHasBlockingFindings(current.Body) && !reviewpolicy.HasUnresolvedCodexP0P1(obs.Review.Threads) {
			outcome = CoordinateSatisfied
		}
		return CoordinateResult{Outcome: outcome, Round: round, Run: current}, nil
	}
	if round >= MaxAutomaticReviewRounds {
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
		if triggered.Run.PRURL == prURL && triggered.Run.TargetSHA == head {
			round++
		}
		return CoordinateResult{Outcome: CoordinateWaiting, Round: round, Run: triggered.Run}, nil
	}
	return CoordinateResult{Outcome: CoordinateStarted, Round: round + 1, Run: triggered.Run}, nil
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

// A changes-requested result from an older reviewer may predate the priority
// contract. Untagged findings therefore fail closed. Once the reviewer uses the
// required tags, P2/P3-only feedback is explicitly non-blocking.
func reviewBodyHasBlockingFindings(body string) bool {
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
