package tmux

import "fmt"

// newSessionArgs builds args for `tmux new-session -d -s <id> -x 220 -y 50
// -c <cwd> <shell> -c <launchCmd>`. The shell -c form runs the launch command
// inside the configured shell so exported env vars and quoting work correctly.
func newSessionArgs(id, cwd, shellPath, launchCmd string) []string {
	return []string{
		"new-session", "-d",
		"-s", id,
		"-x", "220",
		"-y", "50",
		"-c", cwd,
		shellPath, "-c", launchCmd,
	}
}

// setStatusOffArgs hides the tmux status bar for the given session.
// set-option uses pane-targeting syntax which does not accept the `=` prefix,
// so we pass the session name directly.
func setStatusOffArgs(id string) []string {
	return []string{"set-option", "-t", id, "status", "off"}
}

// setMouseOnArgs enables tmux mouse mode so the terminal's SGR mouse-wheel
// reports scroll the pane via copy-mode; without it, wheel scrolling no-ops.
// Pane-targeting, so no `=` prefix (see setStatusOffArgs).
func setMouseOnArgs(id string) []string {
	return []string{"set-option", "-t", id, "mouse", "on"}
}

// setWindowSizeLargestArgs makes tmux size the session's window to the LARGEST
// attached client rather than the most recently active one (the default is
// "latest"). A session can be viewed by several clients at once — e.g. the
// desktop app and the phone. Under "latest", a small phone attaching (or
// becoming active on a session switch) shrinks the shared window for the desktop
// too, giving the desktop a stripped-down view. "largest" ignores smaller
// viewers while a bigger one is attached, so a secondary client can never strip
// down the primary's view; when the big client detaches, tmux recomputes and the
// window follows the remaining largest client. Pane-targeting, so no `=` prefix
// (see setStatusOffArgs).
func setWindowSizeLargestArgs(id string) []string {
	return []string{"set-option", "-t", id, "window-size", "largest"}
}

// killSessionArgs builds args for `tmux kill-session -t =<id>`. The `=` prefix
// requests exact-name matching so a session "foo" does not accidentally match
// "foobar" (tmux otherwise does unique-prefix matching).
func killSessionArgs(id string) []string {
	return []string{"kill-session", "-t", exactSessionTarget(id)}
}

// hasSessionArgs builds args for `tmux has-session -t =<id>`. The `=` prefix
// requests exact-name matching (see killSessionArgs).
func hasSessionArgs(id string) []string {
	return []string{"has-session", "-t", exactSessionTarget(id)}
}

// exactSessionTarget wraps id in tmux's exact-match prefix `=` so session-
// selection commands (-t) target only the session with that precise name.
// Session-selection commands such as kill-session, has-session, and list-panes
// support this prefix; pane-targeting commands (send-keys, capture-pane,
// set-option) use a plain session name.
func exactSessionTarget(id string) string {
	return "=" + id
}

// listPaneRefsArgs captures stable tmux object IDs around process anchoring.
// pane_dead is included so a stale pane_pid is never treated as a live owner.
func listPaneRefsArgs(id string) []string {
	return []string{
		"list-panes", "-s", "-t", exactSessionTarget(id),
		"-F", "#{pane_id}\t#{window_id}\t#{pane_pid}\t#{pane_dead}",
	}
}

// listAllPaneRefsArgs is used after kill-session. A stable pane can move to a
// different window via break/join/move-pane, while an original window can
// survive via links. Either surviving object excludes its process anchor.
func listAllPaneRefsArgs() []string {
	return []string{"list-panes", "-a", "-F", "#{pane_id}\t#{window_id}"}
}

// sendKeysLiteralArgs builds args for `tmux send-keys -t <id> -l <chunk>`.
// The -l flag stops tmux interpreting words like "Enter" as key names so the
// text is sent verbatim.
func sendKeysLiteralArgs(id, chunk string) []string {
	return []string{"send-keys", "-t", id, "-l", chunk}
}

// sendEnterArgs builds args for `tmux send-keys -t <id> Enter` to submit the
// queued input.
func sendEnterArgs(id string) []string {
	return []string{"send-keys", "-t", id, "Enter"}
}

// sendInterruptArgs builds args for `tmux send-keys -t <id> C-c` to interrupt
// the foreground process without killing the terminal session.
func sendInterruptArgs(id string) []string {
	return []string{"send-keys", "-t", id, "C-c"}
}

// capturePaneArgs builds args for `tmux capture-pane -t <id> -p -S -<lines>`.
// -p prints to stdout; -S -<n> starts n lines back in history.
func capturePaneArgs(id string, lines int) []string {
	return []string{"capture-pane", "-t", id, "-p", "-S", fmt.Sprintf("-%d", lines)}
}
