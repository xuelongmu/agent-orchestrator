package agentconformance

import (
	"context"
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// BaseDefaults is the contract supplied by agentbase.Base. Keeping this as a
// structural interface lets other base implementations reuse the same kit.
type BaseDefaults interface {
	GetConfigSpec(context.Context) (ports.ConfigSpec, error)
	GetPromptDeliveryStrategy(context.Context, ports.LaunchConfig) (ports.PromptDeliveryStrategy, error)
	GetAgentHooks(context.Context, ports.WorkspaceHookConfig) error
	GetRestoreCommand(context.Context, ports.RestoreConfig) ([]string, bool, error)
	SessionInfo(context.Context, ports.SessionRef) (ports.SessionInfo, bool, error)
}

// RunBaseDefaults verifies the behavior promised by agentbase.Base, including
// the cancellation semantics inherited by adapters that do not override it.
func RunBaseDefaults(t *testing.T, base BaseDefaults) {
	t.Helper()
	t.Run("empty config", func(t *testing.T) {
		spec, err := base.GetConfigSpec(context.Background())
		if err != nil || len(spec.Fields) != 0 {
			t.Errorf("GetConfigSpec = (%#v, %v), want empty spec", spec, err)
		}
	})
	t.Run("in-command prompt", func(t *testing.T) {
		strategy, err := base.GetPromptDeliveryStrategy(context.Background(), ports.LaunchConfig{})
		if err != nil || strategy != ports.PromptDeliveryInCommand {
			t.Errorf("GetPromptDeliveryStrategy = (%q, %v), want %q", strategy, err, ports.PromptDeliveryInCommand)
		}
	})
	t.Run("no-op hooks", func(t *testing.T) {
		if err := base.GetAgentHooks(context.Background(), ports.WorkspaceHookConfig{}); err != nil {
			t.Errorf("GetAgentHooks: %v", err)
		}
	})
	t.Run("no restore", func(t *testing.T) {
		cmd, ok, err := base.GetRestoreCommand(context.Background(), ports.RestoreConfig{})
		if err != nil || ok || cmd != nil {
			t.Errorf("GetRestoreCommand = (%v, %t, %v), want (nil, false, nil)", cmd, ok, err)
		}
	})
	t.Run("no session info", func(t *testing.T) {
		info, ok, err := base.SessionInfo(context.Background(), ports.SessionRef{})
		if err != nil || ok || info.AgentSessionID != "" || info.Title != "" || info.Summary != "" || len(info.Metadata) != 0 {
			t.Errorf("SessionInfo = (%#v, %t, %v), want zero, false, nil", info, ok, err)
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	checks := map[string]func() error{
		"GetConfigSpec":             func() error { _, err := base.GetConfigSpec(ctx); return err },
		"GetPromptDeliveryStrategy": func() error { _, err := base.GetPromptDeliveryStrategy(ctx, ports.LaunchConfig{}); return err },
		"GetAgentHooks":             func() error { return base.GetAgentHooks(ctx, ports.WorkspaceHookConfig{}) },
		"GetRestoreCommand":         func() error { _, _, err := base.GetRestoreCommand(ctx, ports.RestoreConfig{}); return err },
		"SessionInfo":               func() error { _, _, err := base.SessionInfo(ctx, ports.SessionRef{}); return err },
	}
	for name, check := range checks {
		t.Run(name+" canceled", func(t *testing.T) {
			if err := check(); !errors.Is(err, context.Canceled) {
				t.Errorf("error = %v, want context.Canceled", err)
			}
		})
	}
}
