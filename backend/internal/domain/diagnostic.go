package domain

import (
	"regexp"
	"strings"
	"time"
)

// DiagnosticTrigger identifies the lifecycle observation that caused AO to
// snapshot a session's terminal. It is deliberately small and stable so the
// API and CLI can explain why the diagnostic exists without parsing prose.
type DiagnosticTrigger string

// Supported diagnostic triggers cover ambiguous runtime probes, confirmed
// runtime death, blocked/error hooks, and terminal lifecycle boundaries.
const (
	DiagnosticRuntimeProbeFailed DiagnosticTrigger = "runtime_probe_failed"
	DiagnosticRuntimeDead        DiagnosticTrigger = "runtime_dead"
	DiagnosticBlocked            DiagnosticTrigger = "blocked"
	DiagnosticStopFailure        DiagnosticTrigger = "stop_failure"
	DiagnosticAgentExited        DiagnosticTrigger = "agent_exited"
	DiagnosticTerminated         DiagnosticTrigger = "terminated"
)

// LifecycleDiagnostic is the last privacy-scrubbed terminal evidence captured
// at an abnormal or terminal lifecycle boundary. It intentionally stores only
// a bounded terminal tail and the structured hook error category when one was
// supplied; raw hook payloads and environment data are never persisted.
type LifecycleDiagnostic struct {
	Trigger       DiagnosticTrigger `json:"trigger"`
	TerminalTail  string            `json:"terminalTail,omitempty"`
	HookErrorType string            `json:"hookErrorType,omitempty"`
	CapturedAt    time.Time         `json:"capturedAt"`
}

const maxDiagnosticRunes = 8192

var (
	diagnosticAssignmentSecret = regexp.MustCompile(`(?i)(\b[A-Z0-9_]*(?:TOKEN|SECRET|PASSWORD|PASSWD|API_KEY|APIKEY|PRIVATE_KEY|SIGNING_KEY)[A-Z0-9_]*\s*[:=]\s*)(?:"[^"]*"|'[^']*'|[^\s,;}]+)`)
	diagnosticJSONSecret       = regexp.MustCompile(`(?i)((?:["']?(?:access[_-]?token|refresh[_-]?token|api[_-]?key|password|passwd|secret)["']?)\s*:\s*)(?:"[^"]*"|'[^']*'|[^\s,;}]+)`)
	diagnosticBearer           = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]+`)
	diagnosticKnownToken       = regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{16,}|github_pat_[A-Za-z0-9_]{16,}|sk-(?:ant-)?[A-Za-z0-9_-]{16,})\b`)
)

// SanitizeDiagnosticTail removes terminal control sequences, redacts common
// credential forms, and keeps only the bounded end of the output. It is for
// durable diagnostics, not pane delivery (which uses SanitizeControlChars).
func SanitizeDiagnosticTail(raw string) string {
	plain := stripTerminalSequences(raw)
	plain = strings.ReplaceAll(plain, "\r\n", "\n")
	plain = strings.ReplaceAll(plain, "\r", "\n")
	safe := SanitizeControlChars(plain)
	safe = diagnosticAssignmentSecret.ReplaceAllString(safe, `${1}[REDACTED]`)
	safe = diagnosticJSONSecret.ReplaceAllString(safe, `${1}[REDACTED]`)
	safe = diagnosticBearer.ReplaceAllString(safe, "Bearer [REDACTED]")
	safe = diagnosticKnownToken.ReplaceAllString(safe, "[REDACTED]")
	safe = strings.TrimSpace(safe)
	runes := []rune(safe)
	if len(runes) > maxDiagnosticRunes {
		safe = "…" + string(runes[len(runes)-maxDiagnosticRunes:])
	}
	return safe
}

// stripTerminalSequences removes ANSI/VT escape sequences before the generic
// control-character pass. Removing only ESC would leave visible fragments such
// as "[31m" and could retain OSC payloads (including a terminal title).
func stripTerminalSequences(s string) string {
	b := []byte(s)
	var out strings.Builder
	out.Grow(len(b))
	for i := 0; i < len(b); {
		if b[i] != 0x1b {
			out.WriteByte(b[i])
			i++
			continue
		}
		i++
		if i >= len(b) {
			break
		}
		switch b[i] {
		case '[': // CSI: final byte is in 0x40..0x7e.
			i++
			for i < len(b) {
				c := b[i]
				i++
				if c >= 0x40 && c <= 0x7e {
					break
				}
			}
		case ']', 'P', '_', '^': // OSC/DCS/APC/PM: BEL or ST terminates.
			i++
			for i < len(b) {
				if b[i] == 0x07 {
					i++
					break
				}
				if b[i] == 0x1b && i+1 < len(b) && b[i+1] == '\\' {
					i += 2
					break
				}
				i++
			}
		default:
			// Two-byte Fe escape.
			i++
		}
	}
	return out.String()
}
