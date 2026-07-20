package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/designcontract"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

var (
	// ErrInvalidPRRef is returned when a claim request does not name a GitHub PR URL or positive PR number.
	ErrInvalidPRRef = errors.New("session: invalid pr ref")
	// ErrPRNotFound is returned when the SCM provider has no matching pull request.
	ErrPRNotFound = errors.New("session: pr not found")
	// ErrPRNotOpen is returned when a PR is draft, merged, or closed and therefore cannot be claimed.
	ErrPRNotOpen = errors.New("session: pr not open")
	// ErrSCMUnavailable is returned when live SCM facts cannot be fetched.
	ErrSCMUnavailable = errors.New("session: scm unavailable")
	// ErrProjectMismatch is returned when the PR repository does not match the session project repository.
	ErrProjectMismatch = errors.New("session: pr project mismatch")
	// ErrSessionNotClaimable is returned when an orchestrator session tries to claim a PR.
	ErrSessionNotClaimable = errors.New("session: not claimable")
	// ErrSessionNoWorkspace is returned when a session has no workspace path to associate with PR work.
	ErrSessionNoWorkspace = errors.New("session: no workspace")
	// ErrSessionWorkspaceNotGit reports that SCM branch operations are
	// inapplicable to scratch and shared-directory sessions.
	ErrSessionWorkspaceNotGit = errors.New("session: workspace is not git-backed")
	// ErrSessionDependencyPending reports that dependency-gated workspace
	// creation has not reached its durable promotion commit point.
	ErrSessionDependencyPending = errors.New("session: dependency promotion is pending")
	// ErrClaimTaskPromptTooLong rejects withheld claim work that cannot cross
	// the same bounded prompt boundary used for a normal spawn.
	ErrClaimTaskPromptTooLong = errors.New("session: claim task prompt is too long")
)

// MaxClaimTaskPromptBytes is the maximum UTF-8 payload accepted for work held
// behind a claim-ready contract barrier.
const MaxClaimTaskPromptBytes = 4096

// ClaimPROptions controls PR claim conflict behavior.
type ClaimPROptions struct {
	AllowTakeover bool
	// TaskPrompt is withheld from ao spawn --claim-pr workers until the atomic
	// ownership transaction's contract delivery barrier is lifted.
	TaskPrompt string
}

// ClaimPRResult is the session PR read model returned after a claim.
type ClaimPRResult struct {
	PRs                []domain.PRFacts
	BranchChanged      bool
	TakenOverFrom      []domain.SessionID
	DonorWasTerminated bool
	// ContractReady is true only after the claim-ready message containing the
	// canonical contract reached the claimant and its durable barrier cleared.
	ContractReady bool
}

type designContractDeliveryStore interface {
	GetPendingPRDesignContractDelivery(ctx context.Context, sessionID domain.SessionID, prURL string) (designcontract.PendingDelivery, bool, error)
	CompletePRDesignContractDelivery(ctx context.Context, sessionID domain.SessionID, prURL, deliveryToken string, contractRevision int64) (bool, error)
}

// ListPRs returns all PRs currently owned by a session, ordered for display.
func (s *Service) ListPRs(ctx context.Context, id domain.SessionID) ([]domain.PRFacts, error) {
	_, ok, err := s.store.GetSession(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", id, err)
	}
	if !ok {
		return nil, apierr.NotFound("SESSION_NOT_FOUND", "Unknown session")
	}
	return s.listPRFacts(ctx, id)
}

// ClaimPR attaches a live GitHub PR to a worker session and persists the current SCM facts atomically.
func (s *Service) ClaimPR(ctx context.Context, id domain.SessionID, ref string, opts ClaimPROptions) (ClaimPRResult, error) {
	if len(opts.TaskPrompt) > MaxClaimTaskPromptBytes {
		return ClaimPRResult{}, ErrClaimTaskPromptTooLong
	}
	rec, ok, err := s.store.GetSession(ctx, id)
	if err != nil {
		return ClaimPRResult{}, fmt.Errorf("get %s: %w", id, err)
	}
	if err := validatePRClaimSession(rec, ok); err != nil {
		return ClaimPRResult{}, err
	}
	project, ok, err := s.store.GetProject(ctx, string(rec.ProjectID))
	if err != nil {
		return ClaimPRResult{}, fmt.Errorf("project %s: %w", rec.ProjectID, err)
	}
	if !ok {
		return ClaimPRResult{}, apierr.Invalid("PROJECT_NOT_RESOLVABLE", "Project is not registered or has no repo — register it with `ao project add`", nil)
	}
	prURL, number, err := normalizePRRef(ref, project.RepoOriginURL)
	if err != nil {
		return ClaimPRResult{}, err
	}
	if err := requireSameGitHubRepo(prURL, project.RepoOriginURL); err != nil {
		return ClaimPRResult{}, err
	}
	if s.scm == nil || s.prClaimer == nil {
		return ClaimPRResult{}, ErrSCMUnavailable
	}
	claimLock := s.prClaimLock(prURL)
	claimLock.Lock()
	defer claimLock.Unlock()
	repo, err := scmRepoForClaim(s.scm, project.RepoOriginURL, prURL)
	if err != nil {
		return ClaimPRResult{}, err
	}
	refSpec := ports.SCMPRRef{Repo: repo, Number: number, URL: prURL}
	_, err = s.prClaimer.CheckPRClaim(ctx, prURL, id, opts.AllowTakeover)
	if err != nil {
		return ClaimPRResult{}, err
	}
	obs, err := s.fetchClaimObservation(ctx, refSpec)
	if err != nil {
		return ClaimPRResult{}, err
	}
	if obs.PR.Number == 0 {
		obs.PR.Number = number
	}
	if obs.PR.URL == "" {
		obs.PR.URL = prURL
	}
	if obs.PR.Draft || obs.PR.Merged || obs.PR.Closed {
		return ClaimPRResult{}, ErrPRNotOpen
	}
	reviewMode, err := s.enrichClaimReviews(ctx, refSpec, &obs)
	if err != nil {
		return ClaimPRResult{}, err
	}
	// Claims for different PRs may enrich concurrently, but one session cannot
	// change checkout again until the preceding claim-ready delivery attempt has
	// finished. This lock nests after the per-PR ownership lock and before the
	// session workspace mutation gate.
	unlockSessionClaim := s.lockSessionClaim(id)
	sessionClaimLocked := true
	defer func() {
		if sessionClaimLocked {
			unlockSessionClaim()
		}
	}()
	// A failed or incompletely acknowledged claim-ready delivery is durable and
	// retried by lifecycle. Do not let a different PR replace checkout before
	// that retry delivers the earlier PR's contract and withheld task. Inspect
	// the durable barrier outside the session workspace gate and without taking
	// the per-PR delivery lock, which lifecycle holds across pane I/O. A stale
	// pending read only causes a safe, retryable rejection.
	if err := s.rejectClaimWhileOtherDeliveryPending(ctx, id, prURL); err != nil {
		return ClaimPRResult{}, err
	}
	var (
		branchChanged bool
		outcome       ports.ClaimOutcome
	)
	err = func() error {
		unlockWorkspaceMutation := s.lockWorkspaceMutation(id)
		defer unlockWorkspaceMutation()

		// Provider enrichment is intentionally outside the workspace gate. Once
		// inside it, reload the authoritative record: dependency promotion may
		// have persisted a future path/branch while creating the workspace, and
		// Kill may have terminated the session after the initial request checks.
		latest, exists, readErr := s.store.GetSession(ctx, id)
		if readErr != nil {
			return fmt.Errorf("reload %s for PR claim: %w", id, readErr)
		}
		if err := validatePRClaimSession(latest, exists); err != nil {
			return err
		}
		rec = latest

		workspaceBranch := fmt.Sprintf("ao/claim/%s/pr-%d/root", id, number)
		branchChanged, err = s.scm.CheckoutPullRequest(ctx, refSpec, obs.PR, rec.Metadata.WorkspacePath, workspaceBranch)
		if err != nil {
			return fmt.Errorf("checkout PR #%d: %w", number, err)
		}
		now := s.clock().UTC()
		pr, checks, reviews, threads, comments := claimRowsFromSCM(id, obs, now)
		outcome, err = s.prClaimer.ClaimPR(ctx, pr, checks, reviews, threads, comments, reviewMode, opts.AllowTakeover, workspaceBranch, opts.TaskPrompt)
		if err != nil {
			return err
		}
		// SQLite is canonical, but keep its best-effort workspace projection in
		// the same mutation critical section as checkout and branch persistence.
		// Re-read the delivery payload under its per-PR lock so an invariant append
		// that committed after ClaimPR cannot be overwritten by outcome's older
		// contract snapshot.
		if outcome.ContractDeliveryPending {
			func() {
				unlockDelivery := designcontract.LockDelivery(prURL)
				defer unlockDelivery()
				contract := outcome.DesignContract
				if deliveryStore, ok := s.store.(designContractDeliveryStore); ok {
					delivery, pending, deliveryErr := deliveryStore.GetPendingPRDesignContractDelivery(ctx, id, prURL)
					if deliveryErr != nil {
						slog.Debug("claim PR: design contract projection refresh skipped", "prURL", prURL, "error", deliveryErr)
						return
					}
					if !pending {
						return
					}
					contract = delivery.Contract
				}
				if err := designcontract.MaterializePR(ctx, rec.Metadata.WorkspacePath, prURL, contract); err != nil {
					slog.Debug("claim PR: design contract projection skipped", "prURL", prURL, "error", err)
				}
			}()
		}
		return nil
	}()
	if err != nil {
		return ClaimPRResult{}, err
	}
	contractReady := !outcome.ContractDeliveryPending
	if outcome.ContractDeliveryPending && s.manager != nil {
		deliveryStore, ok := s.store.(designContractDeliveryStore)
		if !ok {
			slog.Warn("claim PR: contract delivery acknowledgement unavailable", "sessionId", id, "prURL", prURL)
		} else {
			unlockDelivery := designcontract.LockDelivery(prURL)
			delivery, pending, deliveryErr := deliveryStore.GetPendingPRDesignContractDelivery(ctx, id, prURL)
			if deliveryErr == nil && pending {
				// Pane delivery deliberately runs after releasing the workspace gate;
				// it can be slow and does not mutate checkout or branch metadata.
				message := domain.SanitizeControlChars(designcontract.ClaimReadyMessage(prURL, delivery.Contract, delivery.TaskPrompt))
				deliveryErr = s.manager.SendAutomated(ctx, id, message)
				if deliveryErr == nil {
					contractReady, deliveryErr = deliveryStore.CompletePRDesignContractDelivery(ctx, id, prURL, delivery.Token, delivery.Revision)
				}
			}
			unlockDelivery()
			if deliveryErr != nil || !contractReady {
				// Ownership is already durable. Keep the delivery obligation pending;
				// lifecycle retries it before any PR reaction after restart or polling.
				slog.Warn("claim PR: contract delivery remains pending", "sessionId", id, "prURL", prURL, "error", deliveryErr)
			}
		}
	}
	// The claim-ready pane attempt is outside the session workspace gate but
	// inside this session's claim lock, so another PR cannot replace checkout
	// before the agent receives (or definitively fails to receive) this barrier.
	unlockSessionClaim()
	sessionClaimLocked = false
	prs, err := s.listPRFacts(ctx, id)
	if err != nil {
		return ClaimPRResult{}, err
	}
	prs = claimedFirst(prs, prURL)
	res := ClaimPRResult{PRs: prs, BranchChanged: branchChanged, DonorWasTerminated: outcome.OwnerTerminated, ContractReady: contractReady}
	if outcome.PreviousOwner != "" && outcome.PreviousOwner != id {
		res.TakenOverFrom = []domain.SessionID{outcome.PreviousOwner}
	}
	return res, nil
}

func validatePRClaimSession(rec domain.SessionRecord, exists bool) error {
	if !exists {
		return apierr.NotFound("SESSION_NOT_FOUND", "Unknown session")
	}
	if rec.IsTerminated {
		return sessionmanagerAPIError("SESSION_TERMINATED", "Session is terminated")
	}
	if rec.Kind == domain.KindOrchestrator {
		return ErrSessionNotClaimable
	}
	if rec.Metadata.WorkspaceKind.WithDefault() != domain.WorkspaceKindWorktree {
		return ErrSessionWorkspaceNotGit
	}
	if rec.DependencyPending() || rec.DependencyPromotionToken != "" || (!rec.DependencyPreparedAt.IsZero() && rec.DependencyPromotedAt.IsZero()) {
		return ErrSessionDependencyPending
	}
	// A worktree path without a branch (or vice versa) is incomplete durable
	// inventory. Never let checkout guess against that half-created state.
	if strings.TrimSpace(rec.Metadata.WorkspacePath) == "" || strings.TrimSpace(rec.Metadata.Branch) == "" {
		return ErrSessionNoWorkspace
	}
	return nil
}

func (s *Service) rejectClaimWhileOtherDeliveryPending(ctx context.Context, id domain.SessionID, targetPRURL string) error {
	deliveryStore, ok := s.store.(designContractDeliveryStore)
	if !ok {
		return nil
	}
	prs, err := s.store.ListPRsBySession(ctx, id)
	if err != nil {
		return fmt.Errorf("list PRs owned by %s before claim: %w", id, err)
	}
	for _, pr := range prs {
		if strings.EqualFold(pr.URL, targetPRURL) {
			continue
		}
		_, pending, deliveryErr := deliveryStore.GetPendingPRDesignContractDelivery(ctx, id, pr.URL)
		if deliveryErr != nil {
			return fmt.Errorf("check pending PR delivery %s: %w", pr.URL, deliveryErr)
		}
		if pending {
			return apierr.Conflict(
				"PR_CLAIM_DELIVERY_PENDING",
				"Session must receive its pending PR claim before claiming a different PR",
				map[string]any{"pendingPrUrl": pr.URL},
			)
		}
	}
	return nil
}

func (s *Service) prClaimLock(prURL string) *sync.Mutex {
	s.prClaimLocksMu.Lock()
	defer s.prClaimLocksMu.Unlock()
	if s.prClaimLocks == nil {
		s.prClaimLocks = make(map[string]*sync.Mutex)
	}
	if s.prClaimLocks[prURL] == nil {
		s.prClaimLocks[prURL] = &sync.Mutex{}
	}
	return s.prClaimLocks[prURL]
}

func (s *Service) fetchClaimObservation(ctx context.Context, ref ports.SCMPRRef) (ports.SCMObservation, error) {
	batch, err := s.scm.FetchPullRequests(ctx, []ports.SCMPRRef{ref})
	if err != nil {
		if errors.Is(err, ports.ErrSCMNotFound) {
			return ports.SCMObservation{}, ErrPRNotFound
		}
		return ports.SCMObservation{}, fmt.Errorf("%w: %w", ErrSCMUnavailable, err)
	}
	if len(batch) == 0 {
		return ports.SCMObservation{}, ErrPRNotFound
	}
	obs := batch[0]
	if !obs.Fetched {
		return ports.SCMObservation{}, ErrSCMUnavailable
	}
	return obs, nil
}

func (s *Service) enrichClaimReviews(ctx context.Context, ref ports.SCMPRRef, obs *ports.SCMObservation) (ports.ReviewWriteMode, error) {
	review, err := s.scm.FetchReviewThreads(ctx, ref)
	if err != nil {
		if errors.Is(err, ports.ErrSCMNotFound) {
			return ports.ReviewWritePreserve, ErrPRNotFound
		}
		return ports.ReviewWritePreserve, fmt.Errorf("%w: %w", ErrSCMUnavailable, err)
	}
	if review.Decision != "" {
		obs.Review.Decision = review.Decision
	}
	obs.Review.Threads = review.Threads
	obs.Review.Reviews = review.Reviews
	obs.Review.Partial = review.Partial
	if review.Partial {
		return ports.ReviewWriteMerge, nil
	}
	return ports.ReviewWriteReplace, nil
}

func scmRepoForClaim(provider scmProvider, projectOrigin, prURL string) (ports.SCMRepo, error) {
	if repo, ok := provider.ParseRepository(projectOrigin); ok {
		return repo, nil
	}
	owner, name, _, err := parseGitHubPRURL(prURL)
	if err != nil {
		return ports.SCMRepo{}, ErrInvalidPRRef
	}
	return ports.SCMRepo{Provider: "github", Host: "github.com", Owner: owner, Name: name, Repo: owner + "/" + name}, nil
}

func claimRowsFromSCM(sessionID domain.SessionID, obs ports.SCMObservation, now time.Time) (domain.PullRequest, []domain.PullRequestCheck, []domain.PullRequestReview, []domain.PullRequestReviewThread, []domain.PullRequestComment) {
	observedAt := obs.ObservedAt
	if observedAt.IsZero() {
		observedAt = now
	}
	pr := domain.PullRequest{
		URL:                      firstNonEmpty(obs.PR.URL, obs.PR.HTMLURL),
		SessionID:                sessionID,
		Number:                   obs.PR.Number,
		Draft:                    obs.PR.Draft,
		Merged:                   obs.PR.Merged,
		Closed:                   obs.PR.Closed,
		CI:                       domain.CIState(firstNonEmpty(obs.CI.Summary, string(domain.CIUnknown))),
		Review:                   domain.ReviewDecision(firstNonEmpty(obs.Review.Decision, string(domain.ReviewNone))),
		Mergeability:             domain.Mergeability(firstNonEmpty(obs.Mergeability.State, string(domain.MergeUnknown))),
		UpdatedAt:                now,
		Provider:                 obs.Provider,
		Host:                     obs.Host,
		Repo:                     obs.Repo,
		SourceBranch:             obs.PR.SourceBranch,
		TargetBranch:             obs.PR.TargetBranch,
		HeadSHA:                  obs.PR.HeadSHA,
		Title:                    obs.PR.Title,
		Additions:                obs.PR.Additions,
		Deletions:                obs.PR.Deletions,
		ChangedFiles:             obs.PR.ChangedFiles,
		Author:                   obs.PR.Author,
		BaseSHA:                  obs.PR.BaseSHA,
		MergeCommitSHA:           obs.PR.MergeCommitSHA,
		ProviderState:            obs.PR.ProviderState,
		ProviderMergeable:        obs.PR.ProviderMergeable,
		ProviderMergeStateStatus: obs.PR.ProviderMergeStateStatus,
		HTMLURL:                  obs.PR.HTMLURL,
		CreatedAtProvider:        obs.PR.CreatedAtProvider,
		UpdatedAtProvider:        obs.PR.UpdatedAtProvider,
		MergedAtProvider:         obs.PR.MergedAtProvider,
		ClosedAtProvider:         obs.PR.ClosedAtProvider,
		ObservedAt:               observedAt,
		CIObservedAt:             observedAt,
		ReviewObservedAt:         observedAt,
	}
	checks := make([]domain.PullRequestCheck, 0, len(obs.CI.Checks))
	for _, ch := range obs.CI.Checks {
		checks = append(checks, domain.PullRequestCheck{Name: ch.Name, CommitHash: obs.CI.HeadSHA, Status: domain.PRCheckStatus(ch.Status), Conclusion: ch.Conclusion, URL: ch.URL, Details: ch.ProviderID, LogTail: ch.LogTail, CreatedAt: now})
	}
	reviews := make([]domain.PullRequestReview, 0, len(obs.Review.Reviews))
	for _, review := range obs.Review.Reviews {
		submittedAt := review.SubmittedAt
		if submittedAt.IsZero() {
			submittedAt = now
		}
		reviews = append(reviews, domain.PullRequestReview{
			ID:          review.ID,
			Author:      review.Author,
			State:       domain.ReviewDecision(firstNonEmpty(review.State, string(domain.ReviewNone))),
			URL:         review.URL,
			IsBot:       review.IsBot,
			SubmittedAt: submittedAt,
		})
	}
	threads := make([]domain.PullRequestReviewThread, 0, len(obs.Review.Threads))
	commentCount := 0
	for _, th := range obs.Review.Threads {
		commentCount += len(th.Comments)
	}
	comments := make([]domain.PullRequestComment, 0, commentCount)
	for _, th := range obs.Review.Threads {
		threads = append(threads, domain.PullRequestReviewThread{ThreadID: th.ID, Path: th.Path, Line: th.Line, Resolved: th.Resolved, IsBot: th.IsBot, UpdatedAt: now})
		for _, c := range th.Comments {
			comments = append(comments, domain.PullRequestComment{ThreadID: th.ID, ID: c.ID, Author: c.Author, File: th.Path, Line: th.Line, Body: c.Body, URL: c.URL, Resolved: th.Resolved, IsBot: c.IsBot || th.IsBot, CreatedAt: now})
		}
	}
	return pr, checks, reviews, threads, comments
}

func sessionmanagerAPIError(code, message string) error {
	return apierr.Conflict(code, message, nil)
}

func (s *Service) listPRFacts(ctx context.Context, id domain.SessionID) ([]domain.PRFacts, error) {
	prs, err := s.store.ListPRsBySession(ctx, id)
	if err != nil {
		return nil, err
	}
	facts := make([]domain.PRFacts, 0, len(prs))
	for _, pr := range prs {
		comments, err := s.store.ListPRComments(ctx, pr.URL)
		if err != nil {
			return nil, err
		}
		facts = append(facts, pullRequestFacts(pr, comments))
	}
	sortPRFacts(facts)
	return facts, nil
}

func pullRequestFacts(pr domain.PullRequest, comments []domain.PullRequestComment) domain.PRFacts {
	unresolved := false
	for _, c := range comments {
		if !c.Resolved {
			unresolved = true
			break
		}
	}
	return domain.PRFacts{URL: pr.URL, Number: pr.Number, Draft: pr.Draft, Merged: pr.Merged, Closed: pr.Closed, CI: pr.CI, Review: pr.Review, Mergeability: pr.Mergeability, ReviewComments: unresolved, UpdatedAt: pr.UpdatedAt}
}

func sortPRFacts(prs []domain.PRFacts) {
	sort.SliceStable(prs, func(i, j int) bool {
		ia, ja := prActive(prs[i]), prActive(prs[j])
		if ia != ja {
			return ia
		}
		return prs[i].UpdatedAt.After(prs[j].UpdatedAt)
	})
}

func prActive(pr domain.PRFacts) bool { return !pr.Merged && !pr.Closed }

func claimedFirst(prs []domain.PRFacts, prURL string) []domain.PRFacts {
	idx := -1
	for i, pr := range prs {
		if pr.URL == prURL {
			idx = i
			break
		}
	}
	if idx <= 0 {
		return prs
	}
	claimed := prs[idx]
	copy(prs[1:idx+1], prs[0:idx])
	prs[0] = claimed
	return prs
}

func normalizePRRef(ref, repoOrigin string) (string, int, error) {
	ref = strings.TrimPrefix(strings.TrimSpace(ref), "#")
	if ref == "" {
		return "", 0, ErrInvalidPRRef
	}
	if n, err := strconv.Atoi(ref); err == nil && n > 0 {
		owner, repo, err := githubRepoFromURL(repoOrigin)
		if err != nil {
			return "", 0, ErrInvalidPRRef
		}
		return fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, repo, n), n, nil
	}
	owner, repo, n, err := parseGitHubPRURL(ref)
	if err != nil || owner == "" || repo == "" || n <= 0 {
		return "", 0, ErrInvalidPRRef
	}
	return fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, repo, n), n, nil
}

func requireSameGitHubRepo(prURL, repoOrigin string) error {
	if strings.TrimSpace(repoOrigin) == "" {
		return nil
	}
	po, pr, _, err := parseGitHubPRURL(prURL)
	if err != nil {
		return ErrInvalidPRRef
	}
	ro, rr, err := githubRepoFromURL(repoOrigin)
	if err != nil {
		return ErrInvalidPRRef
	}
	if !strings.EqualFold(po, ro) || !strings.EqualFold(pr, rr) {
		return ErrProjectMismatch
	}
	return nil
}

func parseGitHubPRURL(raw string) (string, string, int, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", 0, err
	}
	if !strings.EqualFold(u.Scheme, "https") || !strings.EqualFold(u.Hostname(), "github.com") {
		return "", "", 0, ErrInvalidPRRef
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 4 || parts[2] != "pull" {
		return "", "", 0, ErrInvalidPRRef
	}
	n, err := strconv.Atoi(parts[3])
	if err != nil || n <= 0 {
		return "", "", 0, ErrInvalidPRRef
	}
	return parts[0], strings.TrimSuffix(parts[1], ".git"), n, nil
}

func githubRepoFromURL(raw string) (string, string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", ErrInvalidPRRef
	}
	if strings.HasPrefix(raw, "git@github.com:") {
		path := strings.TrimPrefix(raw, "git@github.com:")
		parts := strings.Split(strings.TrimSuffix(path, ".git"), "/")
		if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
			return parts[0], parts[1], nil
		}
		return "", "", ErrInvalidPRRef
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", err
	}
	if !strings.EqualFold(u.Hostname(), "github.com") {
		return "", "", ErrInvalidPRRef
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", ErrInvalidPRRef
	}
	return parts[0], strings.TrimSuffix(parts[1], ".git"), nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
