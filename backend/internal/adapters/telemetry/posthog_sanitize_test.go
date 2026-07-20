package telemetry

import (
	"strings"
	"testing"
)

func TestSanitizeRemoteValueRedactsUnsafeCommandMetadata(t *testing.T) {
	tests := []struct {
		name string
		key  string
		raw  string
		want string
	}{
		{
			name: "URL command",
			key:  "command",
			raw:  "https://gitlab.com/org/repo/-/merge_requests/9",
			want: "<unknown>",
		},
		{
			name: "absolute path command",
			key:  "command",
			raw:  `/Users/name/private/project`,
			want: "<unknown>",
		},
		{
			name: "free text command path",
			key:  "command_path",
			raw:  "ao Review this private change; do not share it",
			want: "ao <unknown>",
		},
		{
			name: "flag in command path",
			key:  "command_path",
			raw:  "ao status --format private-value",
			want: "ao <unknown>",
		},
		{
			name: "unknown before final token",
			key:  "command_path",
			raw:  "ao <unknown> private",
			want: "ao <unknown>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := sanitizeRemoteValue(tt.key, tt.raw)
			if !ok || got != tt.want {
				t.Fatalf("sanitizeRemoteValue(%q, %q) = (%v, %v), want (%q, true)",
					tt.key, tt.raw, got, ok, tt.want)
			}
			if strings.Contains(got.(string), tt.raw) {
				t.Fatalf("raw value leaked through sanitizer: %q", got)
			}
		})
	}
}

func TestSanitizeRemoteValueAllowsCommandMetadata(t *testing.T) {
	tests := []struct {
		key   string
		value string
	}{
		{key: "command", value: "claim-pr"},
		{key: "command", value: "<unknown>"},
		{key: "command_path", value: "ao session claim-pr"},
		{key: "command_path", value: "ao session rename <unknown>"},
	}

	for _, tt := range tests {
		got, ok := sanitizeRemoteValue(tt.key, tt.value)
		if !ok || got != tt.value {
			t.Fatalf("sanitizeRemoteValue(%q, %q) = (%v, %v), want unchanged", tt.key, tt.value, got, ok)
		}
	}
}

func TestSanitizeRemoteValueBoundsOtherStrings(t *testing.T) {
	raw := strings.Repeat("x", maxRemoteStringLength+100)
	got, ok := sanitizeRemoteValue("error_kind", raw)
	if !ok {
		t.Fatal("sanitizeRemoteValue dropped allowlisted string instead of bounding it")
	}
	if got := got.(string); len(got) != maxRemoteStringLength {
		t.Fatalf("sanitized string length = %d, want %d", len(got), maxRemoteStringLength)
	}
}
