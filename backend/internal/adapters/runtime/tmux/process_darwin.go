//go:build darwin

package tmux

import "fmt"

// Darwin exposes process-exit observation through kqueue but no public,
// unprivileged signal API bound to that observed process object. kill(2) always
// resolves the numeric PID again, leaving an unavoidable reuse window after the
// last NOTE_EXIT poll. Best-effort descendant reaping therefore fails closed on
// Darwin rather than risking delivery to an unrelated process.
func platformOpenProcess(pid int) (processObservation, error) {
	return processObservation{}, fmt.Errorf("exact process signal handles are unavailable on darwin for pid %d", pid)
}
