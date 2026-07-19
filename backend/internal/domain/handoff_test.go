package domain

import (
	"strings"
	"testing"
)

func TestAgentHandoffRoundTripAndValidation(t *testing.T) {
	handoff := AgentHandoff{
		ChangedFiles:         []string{"backend/internal/domain/handoff.go"},
		VerificationCommands: []string{"go test ./internal/domain"},
		ResidualRisk:         "The full suite is deferred to CI.",
	}
	payload, err := EncodeAgentHandoff(handoff)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeAgentHandoff(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(handoff) {
		t.Fatalf("round trip = %#v, want %#v", got, handoff)
	}
}

func TestDecodeAgentHandoffRejectsMalformedAndUnknownJSON(t *testing.T) {
	for _, payload := range []string{
		`{`,
		`{"changedFiles":[],"verificationCommands":[],"residualRisk":"","extra":true}`,
		`{"changedFiles":[],"changedFiles":["other"],"verificationCommands":[],"residualRisk":""}`,
		`{"changedFiles":[],"ChangedFiles":["other"],"verificationCommands":[],"residualRisk":""}`,
		`{"changedFiles":[],"verificationCommands":[]}`,
		`{"changedFiles":null,"verificationCommands":[],"residualRisk":""}`,
		string([]byte{'{', '"', 'c', 'h', 'a', 'n', 'g', 'e', 'd', 'F', 'i', 'l', 'e', 's', '"', ':', '[', ']', ',', '"', 'v', 'e', 'r', 'i', 'f', 'i', 'c', 'a', 't', 'i', 'o', 'n', 'C', 'o', 'm', 'm', 'a', 'n', 'd', 's', '"', ':', '[', ']', ',', '"', 'r', 'e', 's', 'i', 'd', 'u', 'a', 'l', 'R', 'i', 's', 'k', '"', ':', '"', 0xff, '"', '}'}),
	} {
		if _, err := DecodeAgentHandoff(payload); err == nil {
			t.Fatalf("DecodeAgentHandoff(%q) succeeded", payload)
		}
	}
}

func TestValidateAgentHandoffRejectsOversizeFields(t *testing.T) {
	valid := AgentHandoff{ChangedFiles: []string{}, VerificationCommands: []string{}}
	tests := []AgentHandoff{
		{ChangedFiles: make([]string, MaxHandoffChangedFiles+1), VerificationCommands: []string{}},
		{ChangedFiles: []string{}, VerificationCommands: make([]string, MaxHandoffVerificationCommands+1)},
		{ChangedFiles: []string{strings.Repeat("x", MaxHandoffChangedFileBytes+1)}, VerificationCommands: []string{}},
		{ChangedFiles: []string{}, VerificationCommands: []string{strings.Repeat("x", MaxHandoffVerificationCommandBytes+1)}},
		{ChangedFiles: []string{}, VerificationCommands: []string{}, ResidualRisk: strings.Repeat("x", MaxHandoffResidualRiskBytes+1)},
		{ChangedFiles: []string{string([]byte{0xff})}, VerificationCommands: []string{}},
	}
	if err := ValidateAgentHandoff(valid); err != nil {
		t.Fatalf("valid empty handoff: %v", err)
	}
	for i, handoff := range tests {
		if err := ValidateAgentHandoff(handoff); err == nil {
			t.Errorf("case %d succeeded", i)
		}
	}
}

func TestValidateAgentHandoffRejectsCanonicalEscapingOverPayloadLimit(t *testing.T) {
	handoff := AgentHandoff{
		ChangedFiles:         make([]string, MaxHandoffChangedFiles),
		VerificationCommands: []string{},
	}
	for i := range handoff.ChangedFiles {
		handoff.ChangedFiles[i] = strings.Repeat("<", MaxHandoffChangedFileBytes)
	}
	rawBytes := len(`{"changedFiles":[],"verificationCommands":[],"residualRisk":""}`) + MaxHandoffChangedFiles*MaxHandoffChangedFileBytes
	if rawBytes >= MaxHandoffPayloadBytes {
		t.Fatalf("test setup raw payload size = %d", rawBytes)
	}
	if err := ValidateAgentHandoff(handoff); err == nil || !strings.Contains(err.Error(), "payload is too large") {
		t.Fatalf("ValidateAgentHandoff() error = %v", err)
	}
}
