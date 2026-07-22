package project_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/project"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

func TestManagerOrchestrationPolicyPreservesProjectConfig(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	row := domain.ProjectRecord{ID: "demo", Path: t.TempDir(), RegisteredAt: time.Now(), Config: domain.ProjectConfig{AgentRules: "keep me", Env: map[string]string{"KEY": "value"}}}
	if err := store.UpsertProject(ctx, row); err != nil {
		t.Fatal(err)
	}
	manager := project.New(store)

	got, err := manager.GetOrchestration(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if got.Policy.Mode != domain.OrchestrationModeMission || got.Policy.CheckInIntervalMinutes != 30 {
		t.Fatalf("default policy = %#v", got.Policy)
	}

	set, err := manager.SetOrchestration(ctx, "demo", project.SetOrchestrationInput{Policy: domain.OrchestrationPolicyConfig{Mode: domain.OrchestrationModeCharter, CheckInIntervalMinutes: 15}})
	if err != nil {
		t.Fatal(err)
	}
	if set.Policy.Mode != domain.OrchestrationModeCharter || set.Policy.CheckInIntervalMinutes != 15 {
		t.Fatalf("set policy = %#v", set.Policy)
	}

	paused, err := manager.SetOrchestrationPaused(ctx, "demo", true)
	if err != nil {
		t.Fatal(err)
	}
	if !paused.Policy.Paused || paused.Policy.Mode != domain.OrchestrationModeCharter || paused.Policy.CheckInIntervalMinutes != 15 {
		t.Fatalf("paused policy = %#v", paused.Policy)
	}

	persisted, ok, err := store.GetProject(ctx, "demo")
	if err != nil || !ok {
		t.Fatalf("get project: ok=%t err=%v", ok, err)
	}
	if persisted.Config.AgentRules != "keep me" || persisted.Config.Env["KEY"] != "value" {
		t.Fatalf("unrelated config changed: %#v", persisted.Config)
	}

	_, err = manager.SetOrchestration(ctx, "demo", project.SetOrchestrationInput{Policy: domain.OrchestrationPolicyConfig{Mode: "forever"}})
	wantCode(t, err, "INVALID_ORCHESTRATION_POLICY")
}
