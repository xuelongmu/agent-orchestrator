package devin

import (
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/claudecode"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// DeriveActivityState applies Claude-compatible lifecycle semantics to the
// callbacks Devin emits from its converted hook configuration.
func DeriveActivityState(event string, payload []byte) (domain.ActivityState, bool) {
	return claudecode.DeriveActivityState(event, payload)
}
