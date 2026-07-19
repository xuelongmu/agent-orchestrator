package devin

import (
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestDeriveActivityStateUsesClaudeCompatibility(t *testing.T) {
	tests := []struct {
		event   string
		payload string
		want    domain.ActivityState
		ok      bool
	}{
		{event: "session-start"},
		{event: "user-prompt-submit", want: domain.ActivityActive, ok: true},
		{event: "stop", want: domain.ActivityIdle, ok: true},
		{event: "session-end", payload: `{"reason":"logout"}`, want: domain.ActivityExited, ok: true},
		{event: "session-end", payload: `{"reason":"resume"}`},
		{event: "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.event+tt.payload, func(t *testing.T) {
			got, ok := DeriveActivityState(tt.event, []byte(tt.payload))
			if got != tt.want || ok != tt.ok {
				t.Fatalf("DeriveActivityState = (%q, %t), want (%q, %t)", got, ok, tt.want, tt.ok)
			}
		})
	}
}
