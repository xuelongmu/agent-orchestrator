package agentconformance_test

import (
	"slices"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/activitydispatch"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/agentbase"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/agentconformance"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/registry"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestBaseDefaults(t *testing.T) {
	agentconformance.RunBaseDefaults(t, agentbase.Base{})
}

// TestShippedAdapters demonstrates that the reusable kit works from an
// external consumer package. Besides guarding the helper itself, this credits
// its execution to agentconformance's package coverage rather than relying on a
// different package's tests to happen to invoke it.
func TestShippedAdapters(t *testing.T) {
	binaries := map[domain.AgentHarness][]string{}
	for _, harness := range domain.AllHarnesses {
		binaries[harness] = []string{string(harness)}
	}
	binaries[domain.HarnessClaudeCode] = []string{"claude"}
	binaries[domain.HarnessContinue] = []string{"cn"}
	binaries[domain.HarnessCursor] = []string{"cursor-agent", "agent"}
	binaries[domain.HarnessKiro] = []string{"kiro-cli"}

	hookTokens := make([]string, 0, len(activitydispatch.Derivers))
	for token := range activitydispatch.Derivers {
		hookTokens = append(hookTokens, token)
	}
	slices.Sort(hookTokens)

	agentconformance.RunRegistry(t, registry.Constructors(), agentconformance.RegistryOptions{
		KnownHarnesses:  domain.AllHarnesses,
		BinaryNames:     binaries,
		KnownHookTokens: hookTokens,
		SupportsHookToken: func(token string) bool {
			_, ok := activitydispatch.Derivers[token]
			return ok
		},
		DispatchHook: activitydispatch.Derive,
		AllowsMetadataOnlyHook: func(token, event string) bool {
			return event == "session-start" && slices.Contains([]string{"claude-code", "codex", "droid", "agy", "grok"}, token)
		},
	})
}
