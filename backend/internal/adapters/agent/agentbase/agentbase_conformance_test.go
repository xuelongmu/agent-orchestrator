package agentbase_test

import (
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/agentbase"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/agentconformance"
)

func TestBaseConformance(t *testing.T) {
	agentconformance.RunBaseDefaults(t, agentbase.Base{})
}
