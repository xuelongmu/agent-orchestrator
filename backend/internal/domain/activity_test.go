package domain

import "testing"

// TestActivityState_StickyAndNeedsInput pins the two independent state
// families: sticky states must survive the passage of time, and needs-input
// states are those where the user is the unblocker (waiting_input = awaiting
// the next instruction, blocked = pending permission/approval decision).
func TestActivityState_StickyAndNeedsInput(t *testing.T) {
	tests := []struct {
		state            ActivityState
		sticky           bool
		needsInput       bool
		paused           bool
		blocksAutomation bool
	}{
		{ActivityActive, false, false, false, false},
		{ActivityIdle, false, false, false, false},
		{ActivityWaitingInput, true, true, true, false},
		{ActivityBlocked, true, true, true, true},
		{ActivityRateLimited, true, false, true, true},
		{ActivityExited, false, false, false, false},
	}
	for _, tt := range tests {
		t.Run(string(tt.state), func(t *testing.T) {
			if got := tt.state.IsSticky(); got != tt.sticky {
				t.Errorf("IsSticky() = %v, want %v", got, tt.sticky)
			}
			if got := tt.state.NeedsInput(); got != tt.needsInput {
				t.Errorf("NeedsInput() = %v, want %v", got, tt.needsInput)
			}
			if got := tt.state.PausesAutomation(); got != tt.paused {
				t.Errorf("PausesAutomation() = %v, want %v", got, tt.paused)
			}
			if got := tt.state.BlocksAutomatedDelivery(); got != tt.blocksAutomation {
				t.Errorf("BlocksAutomatedDelivery() = %v, want %v", got, tt.blocksAutomation)
			}
		})
	}
}
