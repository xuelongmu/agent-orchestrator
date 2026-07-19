package devin

import (
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/claudecode"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// DeriveActivityState applies Claude-compatible lifecycle semantics to the
// callbacks Devin emits from its converted hook configuration. Devin's
// session-start is also an activity signal: its startup payload has no native
// session id, so active keeps the context-injection callback reportable.
func DeriveActivityState(event string, payload []byte) (domain.ActivityState, bool) {
	if event == "session-start" {
		return domain.ActivityActive, true
	}
	return claudecode.DeriveActivityState(event, payload)
}
