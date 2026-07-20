// Package activitystate holds the standard mapping from an AO hook sub-command
// name onto an activity state. Most adapters install the same
// lifecycle callbacks and derive activity identically from the event name
// alone; they share this deriver rather than each carrying a copy. Adapters
// that inspect the hook payload for finer grained state (claude-code, codex,
// droid) keep their own deriver.
package activitystate

import "github.com/aoagents/agent-orchestrator/backend/internal/domain"

// StandardDeriveActivityState maps a hook sub-command name onto an AO activity
// state. The bool is false when the event carries no activity signal. The
// payload is ignored: this is the name-only mapping shared by adapters whose
// hooks report activity purely through which callback fired.
//
//   - session-start / user-prompt-submit                         → active
//   - pre-tool-use / post-tool-use / post-tool-use-failure       → active
//   - stop                                                       → idle
//   - permission-request                                         → waiting_input
//
// permission-request maps to waiting_input, preserving the standard adapters'
// existing semantics: it suppresses automated nudges (NeedsInput) while
// leaving user-initiated sends deliverable.
func StandardDeriveActivityState(event string, _ []byte) (domain.ActivityState, bool) {
	switch event {
	case "session-start", "user-prompt-submit", "pre-tool-use", "post-tool-use", "post-tool-use-failure":
		return domain.ActivityActive, true
	case "stop":
		return domain.ActivityIdle, true
	case "permission-request":
		return domain.ActivityWaitingInput, true
	default:
		return "", false
	}
}
