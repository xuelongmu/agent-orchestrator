// Package charter reconciles standing project charters without turning every
// orchestrator into a permanently supervised process.
package charter

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/observe"
)

const defaultTick = 30 * time.Second

// Store is the read-only durable state needed by the charter observer.
type Store interface {
	ListProjects(context.Context) ([]domain.ProjectRecord, error)
	ListSessions(context.Context, domain.ProjectID) ([]domain.SessionRecord, error)
}

// Messenger is the guarded automated-delivery boundary.
type Messenger interface {
	SendAutomatedIfIdle(context.Context, domain.SessionID, string, time.Time) error
}

// Config supplies testable timing and logging dependencies.
type Config struct {
	Tick   time.Duration
	Clock  func() time.Time
	Logger *slog.Logger
}

type projectState struct {
	mode        domain.OrchestrationMode
	paused      bool
	lastAttempt time.Time
}

// Observer periodically wakes exactly one idle orchestrator for projects with
// a live, unpaused charter policy. Mission projects are never scheduled.
type Observer struct {
	store     Store
	messenger Messenger
	tick      time.Duration
	clock     func() time.Time
	logger    *slog.Logger
	state     map[domain.ProjectID]projectState
}

// New constructs a charter observer. Configuration is re-read from storage on
// every poll, so mode, interval, pause, and resume changes apply at runtime.
func New(store Store, messenger Messenger, cfg Config) *Observer {
	if cfg.Tick <= 0 {
		cfg.Tick = defaultTick
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Observer{store: store, messenger: messenger, tick: cfg.Tick, clock: cfg.Clock, logger: cfg.Logger, state: make(map[domain.ProjectID]projectState)}
}

// Start begins the daemon-owned polling loop.
func (o *Observer) Start(ctx context.Context) <-chan struct{} {
	return observe.StartPollLoop(ctx, o.tick, o.Poll, o.logger, "charter observer")
}

// Poll reconciles all active projects once.
func (o *Observer) Poll(ctx context.Context) error {
	projects, err := o.store.ListProjects(ctx)
	if err != nil {
		return fmt.Errorf("list projects: %w", err)
	}
	now := o.clock().UTC()
	seen := make(map[domain.ProjectID]struct{}, len(projects))
	for _, project := range projects {
		id := domain.ProjectID(project.ID)
		seen[id] = struct{}{}
		policy := project.Config.Orchestration.WithDefaults()
		previous, known := o.state[id]
		if !known {
			// A daemon restart must not create an immediate duplicate wake.
			o.state[id] = projectState{mode: policy.Mode, paused: policy.Paused, lastAttempt: now}
			continue
		}

		current := previous
		current.mode = policy.Mode
		current.paused = policy.Paused
		becameEligible := policy.Mode == domain.OrchestrationModeCharter && !policy.Paused &&
			(previous.mode != domain.OrchestrationModeCharter || previous.paused)
		if becameEligible {
			current.lastAttempt = time.Time{}
		}
		o.state[id] = current

		if policy.Mode != domain.OrchestrationModeCharter || policy.Paused {
			continue
		}
		interval := time.Duration(policy.CheckInIntervalMinutes) * time.Minute
		if !current.lastAttempt.IsZero() && now.Sub(current.lastAttempt) < interval {
			continue
		}

		sessions, err := o.store.ListSessions(ctx, id)
		if err != nil {
			current.lastAttempt = now
			o.state[id] = current
			o.logger.Warn("charter session scan failed", "projectID", id, "err", err)
			continue
		}
		orchestrators := liveOrchestrators(sessions)
		if len(orchestrators) != 1 {
			current.lastAttempt = now
			o.state[id] = current
			if len(orchestrators) > 1 {
				o.logger.Warn("charter check-in skipped: multiple live orchestrators", "projectID", id, "count", len(orchestrators))
			}
			continue
		}
		orchestrator := orchestrators[0]
		if orchestrator.Activity.State != domain.ActivityIdle {
			continue
		}
		// Claim the interval before the guarded send. Success, refusal, and
		// runtime failure all wait for the next interval instead of retrying a
		// potentially ambiguous pane write.
		current.lastAttempt = now
		o.state[id] = current
		if err := o.messenger.SendAutomatedIfIdle(ctx, orchestrator.ID, charterPrompt(id), orchestrator.Activity.LastActivityAt); err != nil {
			o.logger.Warn("charter check-in delivery failed", "projectID", id, "sessionID", orchestrator.ID, "err", err)
		}
	}
	for id := range o.state {
		if _, ok := seen[id]; !ok {
			delete(o.state, id)
		}
	}
	return nil
}

func liveOrchestrators(sessions []domain.SessionRecord) []domain.SessionRecord {
	out := make([]domain.SessionRecord, 0, 1)
	for _, session := range sessions {
		if session.Kind == domain.KindOrchestrator && !session.IsTerminated {
			out = append(out, session)
		}
	}
	return out
}

func charterPrompt(projectID domain.ProjectID) string {
	return fmt.Sprintf(`[AO charter check-in]
Project %s has a standing charter and its orchestrator is idle. Refresh durable project, session, tracker, pull-request, check, and review state. Continue only genuinely actionable work within the project's current rules. Do not duplicate active ownership or create work merely to stay busy. If nothing is actionable, remain idle.`, projectID)
}
