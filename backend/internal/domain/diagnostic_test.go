package domain

import (
	"strings"
	"testing"
)

func TestSanitizeDiagnosticTailStripsTerminalSequencesRedactsSecretsAndBoundsTail(t *testing.T) {
	raw := "\x1b]0;private title\x07\x1b[31mfailed\x1b[0m\rAuthorization: Bearer abc.def.ghi\n" +
		"API_KEY=sk-abcdefghijklmnopqrstuvwxyz\n" + strings.Repeat("x", maxDiagnosticRunes+20)
	got := SanitizeDiagnosticTail(raw)
	if strings.Contains(got, "\x1b") || strings.Contains(got, "private title") || strings.Contains(got, "abc.def.ghi") || strings.Contains(got, "abcdefghijklmnopqrstuvwxyz") {
		t.Fatalf("unsafe diagnostic tail = %q", got)
	}
	if len([]rune(got)) != maxDiagnosticRunes+1 || !strings.HasPrefix(got, "…") {
		t.Fatalf("bounded rune length = %d, want %d with ellipsis", len([]rune(got)), maxDiagnosticRunes+1)
	}
}

func TestSanitizeDiagnosticTailRedactsBareSecretAssignments(t *testing.T) {
	tests := []struct {
		name  string
		input string
		key   string
		value string
	}{
		{name: "token unquoted", input: "TOKEN=token-value", key: "TOKEN", value: "token-value"},
		{name: "secret single quoted", input: "SECRET='secret value'", key: "SECRET", value: "secret value"},
		{name: "password double quoted", input: `PASSWORD="password value"`, key: "PASSWORD", value: "password value"},
		{name: "api key unquoted", input: "API_KEY=api-key-value", key: "API_KEY", value: "api-key-value"},
		{name: "private key double quoted", input: `PRIVATE_KEY="private key value"`, key: "PRIVATE_KEY", value: "private key value"},
		{name: "signing key unquoted", input: "SIGNING_KEY=signing-key-value", key: "SIGNING_KEY", value: "signing-key-value"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SanitizeDiagnosticTail(tt.input)
			if strings.Contains(got, tt.value) {
				t.Fatalf("secret value was retained: %q", got)
			}
			if got != tt.key+"=[REDACTED]" {
				t.Fatalf("sanitized assignment = %q, want %q", got, tt.key+"=[REDACTED]")
			}
		})
	}
}
