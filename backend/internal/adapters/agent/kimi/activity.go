package kimi

import (
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/activitystate"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// DeriveActivityState extends the standard name-only callback mapping with
// Kimi's explicit signal that a permission decision has completed.
func DeriveActivityState(event string, payload []byte) (domain.ActivityState, bool) {
	if event == "permission-result" {
		return domain.ActivityActive, true
	}
	return activitystate.StandardDeriveActivityState(event, payload)
}
