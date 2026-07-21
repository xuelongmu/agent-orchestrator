package charter

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

type fakeStore struct {
	projects []domain.ProjectRecord
	sessions map[domain.ProjectID][]domain.SessionRecord
}

func (f *fakeStore) ListProjects(context.Context) ([]domain.ProjectRecord, error) {
	return f.projects, nil
}
func (f *fakeStore) ListSessions(_ context.Context, id domain.ProjectID) ([]domain.SessionRecord, error) {
	return f.sessions[id], nil
}

type fakeMessenger struct {
	ids []domain.SessionID
	err error
}

func (f *fakeMessenger) SendAutomatedIfIdle(_ context.Context, id domain.SessionID, _ string, _ time.Time) error {
	f.ids = append(f.ids, id)
	return f.err
}

func TestObserverMissionCharterTransitionsAndActivityGate(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{
		projects: []domain.ProjectRecord{{ID: "demo"}},
		sessions: map[domain.ProjectID][]domain.SessionRecord{
			"demo": {{ID: "orch", ProjectID: "demo", Kind: domain.KindOrchestrator, FirstSignalAt: now, Activity: domain.Activity{State: domain.ActivityIdle}}},
		},
	}
	messenger := &fakeMessenger{}
	observer := New(store, messenger, Config{Clock: func() time.Time { return now }, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	ctx := context.Background()

	// Existing projects default to bounded missions and are never scheduled.
	if err := observer.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	if err := observer.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(messenger.ids) != 0 {
		t.Fatalf("mission deliveries = %v", messenger.ids)
	}

	// Changing the live policy to charter makes the next idle poll eligible.
	store.projects[0].Config.Orchestration = domain.OrchestrationPolicyConfig{Mode: domain.OrchestrationModeCharter, CheckInIntervalMinutes: 30}
	if err := observer.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(messenger.ids) != 1 || messenger.ids[0] != "orch" {
		t.Fatalf("charter deliveries = %v", messenger.ids)
	}

	// The interval suppresses duplicate wakes, and non-idle activity remains
	// ineligible even after the interval elapses.
	if err := observer.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	now = now.Add(31 * time.Minute)
	store.sessions["demo"][0].Activity.State = domain.ActivityBlocked
	if err := observer.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(messenger.ids) != 1 {
		t.Fatalf("blocked delivery count = %d", len(messenger.ids))
	}

	// Resume is a runtime transition and becomes immediately eligible.
	store.projects[0].Config.Orchestration.Paused = true
	if err := observer.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	store.sessions["demo"][0].Activity.State = domain.ActivityIdle
	store.projects[0].Config.Orchestration.Paused = false
	if err := observer.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(messenger.ids) != 2 {
		t.Fatalf("resumed delivery count = %d", len(messenger.ids))
	}
}

func TestObserverSkipsAmbiguousOrchestratorsAndThrottlesFailures(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	policy := domain.OrchestrationPolicyConfig{Mode: domain.OrchestrationModeCharter, CheckInIntervalMinutes: 1}
	store := &fakeStore{projects: []domain.ProjectRecord{{ID: "demo", Config: domain.ProjectConfig{Orchestration: policy}}}, sessions: map[domain.ProjectID][]domain.SessionRecord{}}
	messenger := &fakeMessenger{err: errors.New("runtime unavailable")}
	observer := New(store, messenger, Config{Clock: func() time.Time { return now }, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	ctx := context.Background()

	if err := observer.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	store.sessions["demo"] = []domain.SessionRecord{
		{ID: "one", Kind: domain.KindOrchestrator, FirstSignalAt: now, Activity: domain.Activity{State: domain.ActivityIdle}},
		{ID: "two", Kind: domain.KindOrchestrator, FirstSignalAt: now, Activity: domain.Activity{State: domain.ActivityIdle}},
	}
	if err := observer.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(messenger.ids) != 0 {
		t.Fatalf("ambiguous deliveries = %v", messenger.ids)
	}

	now = now.Add(time.Minute)
	store.sessions["demo"] = store.sessions["demo"][:1]
	if err := observer.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(messenger.ids) != 1 {
		t.Fatalf("failure attempts = %d", len(messenger.ids))
	}
	if err := observer.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(messenger.ids) != 1 {
		t.Fatalf("failure retried too soon: %d", len(messenger.ids))
	}
}

func TestObserverRequiresRealActivitySignalBeforeCharterDelivery(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	policy := domain.OrchestrationPolicyConfig{Mode: domain.OrchestrationModeCharter, CheckInIntervalMinutes: 1}
	store := &fakeStore{
		projects: []domain.ProjectRecord{{ID: "demo", Config: domain.ProjectConfig{Orchestration: policy}}},
		sessions: map[domain.ProjectID][]domain.SessionRecord{
			"demo": {{ID: "orch", Kind: domain.KindOrchestrator, Activity: domain.Activity{State: domain.ActivityIdle}}},
		},
	}
	messenger := &fakeMessenger{}
	observer := New(store, messenger, Config{Clock: func() time.Time { return now }, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	ctx := context.Background()

	if err := observer.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if err := observer.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(messenger.ids) != 0 {
		t.Fatalf("no-signal deliveries = %v", messenger.ids)
	}

	store.sessions["demo"][0].FirstSignalAt = now
	if err := observer.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(messenger.ids) != 1 || messenger.ids[0] != "orch" {
		t.Fatalf("signaled idle deliveries = %v", messenger.ids)
	}
}
