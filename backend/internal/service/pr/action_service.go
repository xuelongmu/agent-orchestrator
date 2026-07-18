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
	prNumberPattern = regexp.MustCompile(`^[1-9]\d*$`)
	gitSHAPattern   = regexp.MustCompile(`^[0-9a-fA-F]{40}([0-9a-fA-F]{24})?$`)
)

// ActionManager is the controller-facing contract for /prs/{id} action routes.
type ActionManager interface {
	Merge(ctx context.Context, request MergeRequest) (MergeResult, error)
	ResolveComments(ctx context.Context, prID string, commentIDs []string) (ResolveResult, error)
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

// ResolveResult is the successful outcome of a resolve-comments operation.
type ResolveResult struct {
	Resolved int
}

type actionStore interface {
	GetPR(ctx context.Context, url string) (domain.PullRequest, bool, error)
}

type actionReader interface {
	FetchPullRequests(ctx context.Context, refs []ports.SCMPRRef) ([]ports.SCMObservation, error)
	FetchReviewThreads(ctx context.Context, ref ports.SCMPRRef) (ports.SCMReviewObservation, error)
}

// ActionDeps are the dependencies needed to execute PR actions.
type ActionDeps struct {
	Store  actionStore
	Merger ports.SCMMerger
	Reader actionReader
}

// ActionService implements provider-neutral pull-request actions.
type ActionService struct {
	store  actionStore
	merger ports.SCMMerger
	reader actionReader
}

var _ ActionManager = (*ActionService)(nil)

// NewActionService constructs the PR action executor.
func NewActionService(deps ActionDeps) *ActionService {
	return &ActionService{store: deps.Store, merger: deps.Merger, reader: deps.Reader}
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

// ResolveComments resolves review threads on the PR identified by prID.
// TODO: implement — resolve review threads via the SCM provider.
func (s *ActionService) ResolveComments(_ context.Context, _ string, _ []string) (ResolveResult, error) {
	return ResolveResult{Resolved: 0}, nil
}
