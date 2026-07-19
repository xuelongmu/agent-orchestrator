// Package trackerintake implements the opt-in issue-intake observer. It polls a
// project's configured tracker for eligible issues and starts one worker session
// per issue, leaving PR/lifecycle handling to the existing observers.
package trackerintake

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/observe"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

const (
	// DefaultTickInterval is intentionally slower than runtime liveness checks:
	// intake is a backlog sweep, not an interactive status surface.
	DefaultTickInterval = time.Minute
	// DefaultFailureBackoff suppresses repeated polls for a project after an
	// intake failure. The observer retries automatically after this window.
	DefaultFailureBackoff = 5 * time.Minute
	// DefaultClaimLease bounds a claim that crashes before session creation.
	// Once Spawn creates its durable seed row, claim acquisition reconciles that
	// row and never retries the issue even if the daemon dies before completion.
	DefaultClaimLease = 5 * time.Minute
	// maxIntakePromptLen mirrors the session HTTP prompt limit. Intake uses the
	// session service directly, so it must enforce the same boundary itself.
	maxIntakePromptLen = 4096

	intakePromptTruncationNotice = "\n\n[Issue content truncated to fit the session prompt limit. Open the linked issue for the full details.]\n"
	intakePromptFooter           = "\nImplement the requested change in this repository, run the relevant checks, and open or update a pull request when ready."
)

// Store is the durable discovery and atomic-claim surface the observer needs.
type Store interface {
	ListProjects(ctx context.Context) ([]domain.ProjectRecord, error)
	TrackerIntakeHasCapacity(ctx context.Context, projectID domain.ProjectID, maxConcurrent int, now time.Time) (bool, error)
	ClaimTrackerIntakeIssue(ctx context.Context, claim ports.TrackerIntakeClaim, maxConcurrent int) (ports.TrackerIntakeClaimResult, error)
	RenewTrackerIntakeIssue(ctx context.Context, claim ports.TrackerIntakeClaim, now, leaseExpiresAt time.Time) (bool, error)
	CompleteTrackerIntakeIssue(ctx context.Context, claim ports.TrackerIntakeClaim, sessionID domain.SessionID, completedAt time.Time) (bool, error)
	ReleaseTrackerIntakeIssue(ctx context.Context, claim ports.TrackerIntakeClaim, releasedAt time.Time) (bool, error)
}

// Spawner is the session creation surface used by intake.
type Spawner interface {
	Spawn(ctx context.Context, cfg ports.SpawnConfig) (domain.Session, error)
}

// TrackerResolver picks the tracker adapter for a project's configured
// provider.
type TrackerResolver interface {
	Resolve(provider domain.TrackerProvider) (ports.Tracker, error)
}

// SingleTrackerResolver returns the same tracker for one specific provider and
// refuses every other provider. It exists so single-provider deployments don't
// need to construct a map.
type SingleTrackerResolver struct {
	Provider domain.TrackerProvider
	Adapter  ports.Tracker
}

// Resolve returns the wrapped adapter when the requested provider matches, or
// when the resolver was constructed without a provider pin.
func (s SingleTrackerResolver) Resolve(provider domain.TrackerProvider) (ports.Tracker, error) {
	if s.Adapter == nil {
		return nil, fmt.Errorf("tracker intake: no adapter for provider %q", provider)
	}
	if s.Provider == "" || provider == "" || provider == s.Provider {
		return s.Adapter, nil
	}
	return nil, fmt.Errorf("tracker intake: no adapter for provider %q", provider)
}

// Config holds optional observer knobs. Zero values use production defaults.
type Config struct {
	Tick           time.Duration
	FailureBackoff time.Duration
	ClaimLease     time.Duration
	Clock          func() time.Time
	Token          func() string
	Logger         *slog.Logger
}

// Observer polls configured projects and starts sessions for eligible issues.
type Observer struct {
	resolver       TrackerResolver
	store          Store
	spawner        Spawner
	tick           time.Duration
	failureBackoff time.Duration
	claimLease     time.Duration
	clock          func() time.Time
	token          func() string
	logger         *slog.Logger
	backoffUntil   map[string]time.Time
}

// New constructs an Observer with safe defaults.
func New(resolver TrackerResolver, store Store, spawner Spawner, cfg Config) *Observer {
	o := &Observer{resolver: resolver, store: store, spawner: spawner, tick: cfg.Tick, failureBackoff: cfg.FailureBackoff, claimLease: cfg.ClaimLease, clock: cfg.Clock, token: cfg.Token, logger: cfg.Logger, backoffUntil: map[string]time.Time{}}
	if o.tick <= 0 {
		o.tick = DefaultTickInterval
	}
	if o.failureBackoff <= 0 {
		o.failureBackoff = DefaultFailureBackoff
	}
	if o.claimLease <= 0 {
		o.claimLease = DefaultClaimLease
	}
	if o.clock == nil {
		o.clock = time.Now
	}
	if o.token == nil {
		o.token = uuid.NewString
	}
	if o.logger == nil {
		o.logger = slog.Default()
	}
	return o
}

// Start launches the observer loop. The first poll runs immediately inside the
// goroutine, keeping daemon startup non-blocking.
func (o *Observer) Start(ctx context.Context) <-chan struct{} {
	return observe.StartPollLoop(ctx, o.tick, o.Poll, o.logger, "tracker intake")
}

// Poll runs one synchronous intake pass. Store discovery failures are returned
// because they prevent the pass from knowing the current world; provider and
// spawn failures are logged and skipped so one bad issue/project does not block
// the rest of the daemon.
func (o *Observer) Poll(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if o.resolver == nil || o.store == nil || o.spawner == nil {
		return nil
	}
	now := o.clock().UTC()
	projects, err := o.store.ListProjects(ctx)
	if err != nil {
		return err
	}
	enabledProjects := make([]domain.ProjectRecord, 0, len(projects))
	for _, project := range projects {
		if project.Config.TrackerIntake.Enabled {
			enabledProjects = append(enabledProjects, project)
		}
	}
	if len(enabledProjects) == 0 {
		return nil
	}
	for _, project := range enabledProjects {
		if err := ctx.Err(); err != nil {
			return err
		}
		if until, ok := o.backoffUntil[project.ID]; ok && now.Before(until) {
			o.logger.Debug("tracker intake: project in failure backoff", "project", project.ID, "until", until)
			continue
		}
		failed, err := o.pollProject(ctx, project)
		if err != nil {
			return err
		}
		if failed {
			o.backoffUntil[project.ID] = now.Add(o.failureBackoff)
		} else {
			delete(o.backoffUntil, project.ID)
		}
	}
	return nil
}

// pollProject returns failed=true for conditions that should be retried after a
// backoff window rather than logged on every poll.
func (o *Observer) pollProject(ctx context.Context, project domain.ProjectRecord) (failed bool, retErr error) {
	cfg := project.Config.TrackerIntake.WithDefaults()
	if !cfg.Enabled {
		return false, nil
	}
	if err := cfg.Validate(); err != nil {
		o.logger.Warn("tracker intake: skipping project with invalid config", "project", project.ID, "err", err)
		return true, nil
	}
	capacityNow := o.clock().UTC()
	hasCapacity, err := o.store.TrackerIntakeHasCapacity(ctx, domain.ProjectID(project.ID), cfg.MaxConcurrent, capacityNow)
	if err != nil {
		return false, err
	}
	if !hasCapacity {
		o.logger.Debug("tracker intake: project at concurrency limit", "project", project.ID, "limit", cfg.MaxConcurrent)
		return false, nil
	}
	repo, ok := trackerRepo(project, cfg)
	if !ok {
		o.logger.Warn("tracker intake: skipping project without tracker scope", "project", project.ID, "provider", cfg.Provider, "origin", project.RepoOriginURL)
		return true, nil
	}
	tracker, err := o.resolver.Resolve(cfg.Provider)
	if err != nil {
		o.logger.Warn("tracker intake: no adapter for provider", "project", project.ID, "provider", cfg.Provider, "err", err)
		return true, nil
	}
	issues, err := tracker.List(ctx, repo, domain.ListFilter{
		State:    domain.ListOpen,
		Assignee: cfg.Assignee,
	})
	if err != nil {
		o.logger.Error("tracker intake: list issues failed", "project", project.ID, "repo", repo.Native, "err", err)
		return true, nil
	}
	var spawnFailed bool
	for _, issue := range issues {
		if ctx.Err() != nil {
			return true, nil
		}
		if issue.State != domain.IssueOpen {
			continue
		}
		if !issueMatchesConfig(issue, cfg) {
			continue
		}
		issueID := CanonicalIssueID(issue.ID)
		if issueID == "" {
			continue
		}
		provider := issue.ID.Provider
		if provider == "" {
			provider = repo.Provider
		}
		nativeIssueID := normalizedTrackerNativeID(provider, issue.ID.Native)
		claimNow := o.clock().UTC()
		claim := ports.TrackerIntakeClaim{
			ProjectID: domain.ProjectID(project.ID), Provider: provider, Repo: repo.Native,
			IssueID: nativeIssueID, OwnerToken: o.token(),
			ClaimedAt: claimNow, LeaseExpiresAt: claimNow.Add(o.claimLease),
		}
		claimResult, err := o.store.ClaimTrackerIntakeIssue(ctx, claim, cfg.MaxConcurrent)
		if err != nil {
			return false, err
		}
		switch claimResult {
		case ports.TrackerIntakeClaimAlreadyProcessed, ports.TrackerIntakeClaimBusy:
			continue
		case ports.TrackerIntakeClaimCapacityReached:
			return spawnFailed, nil
		case ports.TrackerIntakeClaimAcquired:
		default:
			return false, fmt.Errorf("tracker intake: unknown claim result %d", claimResult)
		}
		spawnCtx, stopRenewal := o.maintainClaim(ctx, claim)
		session, err := o.spawner.Spawn(spawnCtx, ports.SpawnConfig{
			ProjectID:   domain.ProjectID(project.ID),
			IssueID:     issueID,
			Kind:        domain.KindWorker,
			Prompt:      BuildIssuePrompt(issue),
			IntakeClaim: &claim,
		})
		renewalErr := stopRenewal()
		if renewalErr != nil && err == nil {
			err = renewalErr
		}
		if err == nil {
			finalNow := o.clock().UTC()
			retained, finalErr := o.store.RenewTrackerIntakeIssue(ctx, claim, finalNow, finalNow.Add(o.claimLease))
			if finalErr != nil {
				err = finalErr
			} else if !retained {
				err = errors.New("tracker intake claim ownership lost before completion")
			}
		}
		if err != nil {
			o.logger.Error("tracker intake: spawn issue session failed", "project", project.ID, "issue", issueID, "err", err)
			releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			_, releaseErr := o.store.ReleaseTrackerIntakeIssue(releaseCtx, claim, o.clock().UTC())
			cancel()
			if releaseErr != nil {
				o.logger.Error("tracker intake: release failed spawn claim", "project", project.ID, "issue", issueID, "err", releaseErr)
			}
			spawnFailed = true
			continue
		}
		completed, err := o.store.CompleteTrackerIntakeIssue(ctx, claim, session.ID, o.clock().UTC())
		if err != nil || !completed {
			o.logger.Error("tracker intake: complete issue claim failed", "project", project.ID, "issue", issueID, "completed", completed, "err", err)
			spawnFailed = true
		}
	}
	return spawnFailed, nil
}

// maintainClaim renews the pending lease while Spawn is live. Spawn creates a
// durable session row before external workspace/runtime side effects; after
// that row exists a concurrent poll may reconcile the claim to completed, which
// RenewTrackerIntakeIssue deliberately also treats as retained ownership.
func (o *Observer) maintainClaim(ctx context.Context, claim ports.TrackerIntakeClaim) (context.Context, func() error) {
	spawnCtx, cancelSpawn := context.WithCancel(ctx)
	stop := make(chan struct{})
	done := make(chan error, 1)
	interval := o.claimLease / 3
	if interval <= 0 {
		interval = time.Second
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				done <- nil
				return
			case <-spawnCtx.Done():
				done <- spawnCtx.Err()
				return
			case <-ticker.C:
				now := o.clock().UTC()
				retained, err := o.store.RenewTrackerIntakeIssue(spawnCtx, claim, now, now.Add(o.claimLease))
				if err != nil || !retained {
					if err == nil {
						err = errors.New("tracker intake claim ownership lost during spawn")
					}
					cancelSpawn()
					done <- err
					return
				}
			}
		}
	}()
	return spawnCtx, func() error {
		close(stop)
		err := <-done
		cancelSpawn()
		return err
	}
}

func issueMatchesConfig(issue domain.Issue, cfg domain.TrackerIntakeConfig) bool {
	assignee := strings.TrimSpace(cfg.Assignee)
	switch {
	case assignee == "":
		return true
	case assignee == "*":
		return len(issue.Assignees) > 0
	case strings.EqualFold(assignee, "none"):
		return len(issue.Assignees) == 0
	default:
		return containsFold(issue.Assignees, assignee)
	}
}

func containsFold(values []string, needle string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), needle) {
			return true
		}
	}
	return false
}

// CanonicalIssueID stores tracker issue ids in sessions.issue_id with the
// provider included, so future providers cannot collide on native ids.
func CanonicalIssueID(id domain.TrackerID) domain.IssueID {
	provider := id.Provider
	if provider == "" {
		provider = domain.TrackerProviderGitHub
	}
	native := normalizedTrackerNativeID(provider, id.Native)
	if native == "" {
		return ""
	}
	return domain.IssueID(string(provider) + ":" + native)
}

// normalizedTrackerNativeID applies provider identity rules before either the
// durable claim key or sessions.issue_id is written. GitHub owner/repository
// names are case-insensitive, and its native issue id is owner/repo#number, so
// lower-casing prevents configuration/API response casing drift from creating
// a second intake lane for the same issue.
func normalizedTrackerNativeID(provider domain.TrackerProvider, native string) string {
	native = strings.TrimSpace(native)
	if provider == domain.TrackerProviderGitHub {
		return strings.ToLower(native)
	}
	return native
}

// BuildIssuePrompt turns normalized issue facts into the worker's initial task.
func BuildIssuePrompt(issue domain.Issue) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Work on tracker issue %s.\n\n", CanonicalIssueID(issue.ID))
	if issue.Title != "" {
		fmt.Fprintf(&b, "Title: %s\n", issue.Title)
	}
	if issue.URL != "" {
		fmt.Fprintf(&b, "URL: %s\n", issue.URL)
	}
	if len(issue.Labels) > 0 {
		fmt.Fprintf(&b, "Labels: %s\n", strings.Join(issue.Labels, ", "))
	}
	if len(issue.Assignees) > 0 {
		fmt.Fprintf(&b, "Assignees: %s\n", strings.Join(issue.Assignees, ", "))
	}
	body := strings.TrimSpace(issue.Body)
	if body != "" {
		fmt.Fprintf(&b, "\nBody:\n%s\n", body)
	}
	b.WriteString(intakePromptFooter)
	return capIntakePrompt(b.String())
}

func capIntakePrompt(prompt string) string {
	if len(prompt) <= maxIntakePromptLen {
		return prompt
	}
	prefix := strings.TrimSuffix(prompt, intakePromptFooter)
	prefixBudget := maxIntakePromptLen - len(intakePromptTruncationNotice) - len(intakePromptFooter)
	if prefixBudget <= 0 {
		return truncateUTF8(prompt, maxIntakePromptLen)
	}
	return truncateUTF8(prefix, prefixBudget) + intakePromptTruncationNotice + intakePromptFooter
}

func truncateUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := 0
	for i := range s {
		if i > maxBytes {
			break
		}
		cut = i
	}
	return s[:cut]
}

func trackerRepo(project domain.ProjectRecord, cfg domain.TrackerIntakeConfig) (domain.TrackerRepo, bool) {
	provider := cfg.Provider
	if provider == "" {
		provider = domain.TrackerProviderGitHub
	}
	if provider != domain.TrackerProviderGitHub {
		return domain.TrackerRepo{}, false
	}
	native := strings.TrimSpace(cfg.Repo)
	if native == "" {
		native = parseGitHubRepoNative(project.RepoOriginURL)
	}
	if native == "" {
		return domain.TrackerRepo{}, false
	}
	if provider == domain.TrackerProviderGitHub {
		native = strings.ToLower(native)
	}
	return domain.TrackerRepo{Provider: provider, Native: native}, true
}

func parseGitHubRepoNative(remote string) string {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return ""
	}
	if strings.HasPrefix(remote, "git@") {
		if _, rest, ok := strings.Cut(remote, ":"); ok {
			return cleanRepoPath(rest)
		}
	}
	if u, err := url.Parse(remote); err == nil && u.Host != "" {
		host := strings.TrimPrefix(strings.ToLower(u.Host), "www.")
		if host == "github.com" || strings.HasSuffix(host, ".github.com") || strings.HasSuffix(host, ".ghe.io") {
			return cleanRepoPath(u.Path)
		}
		return ""
	}
	return cleanRepoPath(remote)
}

func cleanRepoPath(path string) string {
	path = strings.Trim(strings.TrimSpace(path), "/")
	path = strings.TrimSuffix(path, ".git")
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		return ""
	}
	owner := strings.TrimSpace(parts[len(parts)-2])
	repo := strings.TrimSpace(parts[len(parts)-1])
	if owner == "" || repo == "" {
		return ""
	}
	return owner + "/" + repo
}
