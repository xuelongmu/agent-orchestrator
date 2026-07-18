package conpty

import (
	"strings"
	"sync"
)

// MaxOutputLines is the rolling line-buffer cap, matching MAX_OUTPUT_LINES in pty-host.ts.
const MaxOutputLines = 1000

// Ring is a bounded rolling buffer of terminal output lines, ANSI codes preserved.
// It mirrors the appendOutput state machine from pty-host.ts.
// Concurrent Append and Snapshot/Tail calls are safe.
type Ring struct {
	mu          sync.Mutex
	lines       []string // each entry is "line\n" (or bare text on FlushPartial)
	partialLine string
}

// NewRing returns an empty Ring.
func NewRing() *Ring {
	return &Ring{}
}

// Append mirrors appendOutput from pty-host.ts: prepend the current partialLine,
// split on newlines, store completed lines with "\n" re-appended, keep the last
// element as the new partialLine, then trim to MaxOutputLines.
func (r *Ring) Append(raw []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()

	text := r.partialLine + string(raw)
	parts := strings.Split(text, "\n")
	// The last element is either "" (text ended with \n) or an incomplete line.
	r.partialLine = parts[len(parts)-1]
	for _, line := range parts[:len(parts)-1] {
		r.lines = append(r.lines, line+"\n")
	}
	if len(r.lines) > MaxOutputLines {
		// ponytail: slice off the head; ceiling: O(n) copy on every trim cycle.
		// Upgrade path: circular buffer if trim rate is very high.
		r.lines = r.lines[len(r.lines)-MaxOutputLines:]
	}
}

// FlushPartial pushes any in-progress partial line as a final entry.
// Called on PTY exit to mirror the pty-host.ts onExit handler.
func (r *Ring) FlushPartial() {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.partialLine == "" {
		return
	}
	r.lines = append(r.lines, r.partialLine)
	r.partialLine = ""
}

// Snapshot returns all stored lines concatenated as raw bytes for scrollback replay.
// The in-progress partialLine is NOT included (matches TS outputBuffer.join("")).
func (r *Ring) Snapshot() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()

	return []byte(strings.Join(r.lines, ""))
}

// Tail returns the last n available lines joined as a string, including the
// current unterminated line. Terminal UIs frequently redraw their actionable
// state without a trailing newline, so excluding partialLine hides prompts
// such as Codex's collapsed "[Pasted Content ...]" editor placeholder.
// n <= 0 returns "".
func (r *Ring) Tail(n int) string {
	r.mu.Lock()
	defer r.mu.Unlock()

	if n <= 0 {
		return ""
	}
	available := r.lines
	if r.partialLine != "" {
		available = make([]string, 0, len(r.lines)+1)
		available = append(available, r.lines...)
		available = append(available, r.partialLine)
	}
	start := len(available) - n
	if start < 0 {
		start = 0
	}
	return strings.Join(available[start:], "")
}
