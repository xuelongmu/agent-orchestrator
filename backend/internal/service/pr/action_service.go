package pr

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/scmready"
)

var (
	prNumberPattern       = regexp.MustCompile(`^[1-9]\d*$`)
	gitSHAPattern         = regexp.MustCompile(`^[0-9a-fA-F]{40}([0-9a-fA-F]{24})?$`)
	reviewThreadIDPattern = regexp.MustCompile(`^[A-Za-z0-9_./+:=-]{1,256}$`)
)

// ActionManager is the controller-facing contract for /prs/{id} action routes.
type ActionManager interface {
	Merge(ctx context.Context, request MergeRequest) (MergeResult, error)
	ResolveComments(ctx context.Context, request ResolveRequest) (ResolveResult, error)
}

// MergeRequest is the service-level input for an exact-head PR merge.
type MergeRequest struct {
	PRID            string
	PRURL           string
	ExpectedHeadSHA string
}

// MergeResult is the successful outcome of a PR merge.
type MergeResult struct {
	PRNumber       int
	Method         string // always "squash"
	MergeCommitSHA string
}

// ResolveRequest identifies one tracked PR and optionally selects review
// threads. An empty ThreadIDs slice means all locally-known unresolved threads.
type ResolveRequest struct {
	PRID      string
	PRURL     string
	ThreadIDs []string
}

// ResolveResult is the successful outcome of a resolve-comments operation.
type ResolveResult struct {
	Requested       int
	Resolved        int
	AlreadyResolved int
	Failed          int
}

// ResolveFailure records one provider mutation that failed. Keeping the node
// ID with the typed cause makes partial completion auditable by callers.
type ResolveFailure struct {
	ThreadID string
	Err      error
}

// ResolveError reports a partial or total batch failure. Unwrap exposes every
// typed cause so callers can map not-found/permission failures with errors.Is.
type ResolveError struct {
	Result   ResolveResult
	Failures []ResolveFailure
}

func (e *ResolveError) Error() string {
	return fmt.Sprintf("pr: resolve review threads: %d of %d failed", e.Result.Failed, e.Result.Requested)
}

func (e *ResolveError) Unwrap() []error {
	out := make([]error, 0, len(e.Failures))
	for _, failure := range e.Failures {
		out = append(out, failure.Err)
	}
	return out
}

type actionStore interface {
	GetPR(ctx context.Context, url string) (domain.PullRequest, bool, error)
	ListPRReviewThreads(ctx context.Context, prURL string) ([]domain.PullRequestReviewThread, error)
}

type actionReader interface {
	FetchPullRequests(ctx context.Context, refs []ports.SCMPRRef) ([]ports.SCMObservation, error)
	FetchReviewThreads(ctx context.Context, ref ports.SCMPRRef) (ports.SCMReviewObservation, error)
}

// ActionDeps are the dependencies needed to execute PR actions.
type ActionDeps struct {
	Store    actionStore
	Merger   ports.SCMMerger
	Reader   actionReader
	Resolver ports.SCMReviewThreadResolver
}

// ActionService implements provider-neutral pull-request actions.
type ActionService struct {
	store    actionStore
	merger   ports.SCMMerger
	reader   actionReader
	resolver ports.SCMReviewThreadResolver
}

var _ ActionManager = (*ActionService)(nil)

// NewActionService constructs the PR action executor.
func NewActionService(deps ActionDeps) *ActionService {
	return &ActionService{store: deps.Store, merger: deps.Merger, reader: deps.Reader, resolver: deps.Resolver}
}

// Merge squash-merges one tracked PR only when fresh SCM facts prove the shared
// definition of done for the exact head supplied by the caller. The provider
// merge request repeats that head as a final compare-and-swap guard.
func (s *ActionService) Merge(ctx context.Context, request MergeRequest) (MergeResult, error) {
	prNumber, err := parsePRNumber(request.PRID)
	if err != nil || strings.TrimSpace(request.PRURL) == "" {
		return MergeResult{}, fmt.Errorf("%w: invalid pull request identity", ErrInvalidPR)
	}
	if s.store == nil || s.merger == nil || s.reader == nil {
		return MergeResult{}, errors.New("pr: merge action is not configured")
	}
	expectedHead := strings.TrimSpace(request.ExpectedHeadSHA)
	if expectedHead == "" {
		return MergeResult{}, fmt.Errorf("%w: expected head is required", ErrPRPreconditions)
	}
	if !gitSHAPattern.MatchString(expectedHead) {
		return MergeResult{}, fmt.Errorf("%w: invalid expected head", ErrInvalidPR)
	}
	expectedHead = strings.ToLower(expectedHead)

	tracked, ok, err := s.store.GetPR(ctx, request.PRURL)
	if err != nil {
		return MergeResult{}, fmt.Errorf("load pull request: %w", err)
	}
	if !ok || tracked.Number != prNumber {
		return MergeResult{}, ErrPRNotFound
	}
	if tracked.Draft || tracked.Merged || tracked.Closed {
		return MergeResult{}, ErrPRNotMergeable
	}

	storedHead := strings.TrimSpace(tracked.HeadSHA)
	if storedHead == "" || !gitSHAPattern.MatchString(storedHead) {
		return MergeResult{}, fmt.Errorf("%w: pull request head is unknown", ErrPRPreconditions)
	}
	if !strings.EqualFold(expectedHead, storedHead) {
		return MergeResult{}, ErrPRHeadChanged
	}

	repo, ok := scmRepoForPR(tracked)
	if !ok {
		return MergeResult{}, fmt.Errorf("%w: pull request repository is unknown", ErrPRPreconditions)
	}
	ref := ports.SCMPRRef{Repo: repo, Number: tracked.Number, URL: tracked.URL}
	fresh, err := s.fetchMergeReadiness(ctx, ref)
	if err != nil {
		return MergeResult{}, err
	}
	if !strings.EqualFold(fresh.PR.HeadSHA, expectedHead) {
		return MergeResult{}, ErrPRHeadChanged
	}
	if !scmready.IsReadyToMerge(fresh) {
		return MergeResult{}, ErrPRPreconditions
	}

	out, err := s.merger.MergePullRequest(ctx, ports.SCMMergeRequest{
		PR:              ref,
		ExpectedHeadSHA: expectedHead,
		Method:          ports.SCMMergeSquash,
	})
	if err != nil {
		switch {
		case errors.Is(err, ports.ErrSCMNotFound):
			return MergeResult{}, fmt.Errorf("%w: %w", ErrPRNotFound, err)
		case errors.Is(err, ports.ErrSCMHeadChanged):
			return MergeResult{}, fmt.Errorf("%w: %w", ErrPRHeadChanged, err)
		case errors.Is(err, ports.ErrSCMNotMergeable):
			return MergeResult{}, fmt.Errorf("%w: %w", ErrPRNotMergeable, err)
		case errors.Is(err, ports.ErrSCMPermissionDenied):
			return MergeResult{}, fmt.Errorf("%w: %w", ErrPRPermissionDenied, err)
		default:
			return MergeResult{}, fmt.Errorf("merge pull request: %w", err)
		}
	}
	return MergeResult{PRNumber: tracked.Number, Method: string(ports.SCMMergeSquash), MergeCommitSHA: out.MergeCommitSHA}, nil
}

func (s *ActionService) fetchMergeReadiness(ctx context.Context, ref ports.SCMPRRef) (ports.SCMObservation, error) {
	observations, err := s.reader.FetchPullRequests(ctx, []ports.SCMPRRef{ref})
	if err != nil {
		if errors.Is(err, ports.ErrSCMNotFound) {
			return ports.SCMObservation{}, fmt.Errorf("%w: %w", ErrPRNotFound, err)
		}
		return ports.SCMObservation{}, fmt.Errorf("refresh pull request before merge: %w", err)
	}
	if len(observations) != 1 || !observations[0].Fetched || observations[0].PR.Number != ref.Number {
		return ports.SCMObservation{}, ErrPRNotFound
	}
	observation := observations[0]
	review, err := s.reader.FetchReviewThreads(ctx, ref)
	if err != nil {
		if errors.Is(err, ports.ErrSCMNotFound) {
			return ports.SCMObservation{}, fmt.Errorf("%w: %w", ErrPRNotFound, err)
		}
		return ports.SCMObservation{}, fmt.Errorf("refresh pull request reviews before merge: %w", err)
	}
	observation.Review = review
	return observation, nil
}

func parsePRNumber(value string) (int, error) {
	if !prNumberPattern.MatchString(value) {
		return 0, ErrInvalidPR
	}
	n, err := strconv.ParseInt(value, 10, 32)
	if err != nil || n <= 0 {
		return 0, ErrInvalidPR
	}
	return int(n), nil
}

func scmRepoForPR(pr domain.PullRequest) (ports.SCMRepo, bool) {
	parts := strings.Split(pr.Repo, "/")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return ports.SCMRepo{}, false
	}
	provider := strings.ToLower(strings.TrimSpace(pr.Provider))
	if provider == "" {
		provider = "github"
	}
	host := strings.ToLower(strings.TrimSpace(pr.Host))
	if host == "" && provider == "github" {
		host = "github.com"
	}
	return ports.SCMRepo{Provider: provider, Host: host, Owner: parts[0], Name: parts[1], Repo: pr.Repo}, true
}

// ResolveComments ensures selected review threads are resolved, or all locally
// known unresolved threads when no explicit IDs are supplied. The operation is
// idempotent and returns an error whenever any provider mutation is unconfirmed.
func (s *ActionService) ResolveComments(ctx context.Context, request ResolveRequest) (ResolveResult, error) {
	if s.store == nil || s.resolver == nil {
		return ResolveResult{}, ErrActionNotConfigured
	}
	prNumber, err := parsePRNumber(request.PRID)
	if err != nil || strings.TrimSpace(request.PRURL) == "" {
		return ResolveResult{}, fmt.Errorf("%w: invalid pull request identity", ErrInvalidPR)
	}
	tracked, ok, err := s.store.GetPR(ctx, request.PRURL)
	if err != nil {
		return ResolveResult{}, fmt.Errorf("load pull request: %w", err)
	}
	if !ok || tracked.Number != prNumber {
		return ResolveResult{}, ErrPRNotFound
	}
	threads, err := s.store.ListPRReviewThreads(ctx, tracked.URL)
	if err != nil {
		return ResolveResult{}, fmt.Errorf("list pull request review threads: %w", err)
	}
	byID := make(map[string]domain.PullRequestReviewThread, len(threads))
	for _, thread := range threads {
		byID[thread.ThreadID] = thread
	}

	selected, explicit, err := selectReviewThreads(request.ThreadIDs, threads, byID)
	if err != nil {
		return ResolveResult{}, err
	}
	if len(selected) == 0 {
		return ResolveResult{}, ErrNothingToResolve
	}
	result := ResolveResult{Requested: len(selected)}
	failures := make([]ResolveFailure, 0)
	for _, threadID := range selected {
		wasResolved := explicit && byID[threadID].Resolved
		resolved, resolveErr := s.resolver.ResolveReviewThread(ctx, threadID)
		if resolveErr == nil && (resolved.ThreadID != threadID || !resolved.Resolved) {
			resolveErr = errors.New("provider did not confirm resolved state")
		}
		if resolveErr == nil {
			if wasResolved {
				result.AlreadyResolved++
			} else {
				result.Resolved++
			}
			continue
		}
		result.Failed++
		failures = append(failures, ResolveFailure{ThreadID: threadID, Err: mapResolveError(resolveErr)})
	}
	if len(failures) > 0 {
		return result, &ResolveError{Result: result, Failures: failures}
	}
	return result, nil
}

func selectReviewThreads(requested []string, threads []domain.PullRequestReviewThread, byID map[string]domain.PullRequestReviewThread) ([]string, bool, error) {
	if len(requested) == 0 {
		selected := make([]string, 0, len(threads))
		for _, thread := range threads {
			if !thread.Resolved {
				selected = append(selected, thread.ThreadID)
			}
		}
		return selected, false, nil
	}
	seen := make(map[string]struct{}, len(requested))
	selected := make([]string, 0, len(requested))
	for _, threadID := range requested {
		if strings.TrimSpace(threadID) != threadID || !reviewThreadIDPattern.MatchString(threadID) {
			return nil, true, fmt.Errorf("%w: invalid review thread id", ErrInvalidPR)
		}
		if _, ok := byID[threadID]; !ok {
			return nil, true, fmt.Errorf("%w: %s", ErrReviewThreadNotFound, threadID)
		}
		if _, duplicate := seen[threadID]; duplicate {
			continue
		}
		seen[threadID] = struct{}{}
		selected = append(selected, threadID)
	}
	return selected, true, nil
}

func mapResolveError(err error) error {
	switch {
	case errors.Is(err, ports.ErrSCMNotFound):
		return fmt.Errorf("%w: %w", ErrReviewThreadNotFound, err)
	case errors.Is(err, ports.ErrSCMPermissionDenied):
		return fmt.Errorf("%w: %w", ErrPRPermissionDenied, err)
	default:
		return err
	}
}
