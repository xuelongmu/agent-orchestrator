package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// Agent handoff limits keep the durable completion summary safe to render and
// bounded when a session detail read requests it. Lengths are measured in bytes
// because they ultimately bound the JSON and SQLite payload.
const (
	MaxHandoffChangedFiles             = 128
	MaxHandoffVerificationCommands     = 32
	MaxHandoffChangedFileBytes         = 1024
	MaxHandoffVerificationCommandBytes = 4096
	MaxHandoffResidualRiskBytes        = 8192
	MaxHandoffPayloadBytes             = 256 * 1024
)

// AgentHandoff is the immutable, machine-readable completion summary an agent
// explicitly submits for its session. Submitting it does not alter activity,
// terminal, or scheduling state.
type AgentHandoff struct {
	ChangedFiles         []string `json:"changedFiles" maxItems:"128" description:"Changed paths; each item is bounded by the server to 1024 UTF-8 bytes."`
	VerificationCommands []string `json:"verificationCommands" maxItems:"32" description:"Commands run; each item is bounded by the server to 4096 UTF-8 bytes."`
	ResidualRisk         string   `json:"residualRisk" description:"Remaining risk, bounded by the server to 8192 UTF-8 bytes."`
}

// Equal compares the exact typed payload. Order, whitespace, and repeated
// entries are significant so an exact replay is idempotent while any changed
// submission is rejected.
func (h AgentHandoff) Equal(other AgentHandoff) bool {
	if h.ResidualRisk != other.ResidualRisk || len(h.ChangedFiles) != len(other.ChangedFiles) || len(h.VerificationCommands) != len(other.VerificationCommands) {
		return false
	}
	for i := range h.ChangedFiles {
		if h.ChangedFiles[i] != other.ChangedFiles[i] {
			return false
		}
	}
	for i := range h.VerificationCommands {
		if h.VerificationCommands[i] != other.VerificationCommands[i] {
			return false
		}
	}
	return true
}

// ValidateAgentHandoff enforces the bounded persistence/API contract without
// normalizing the payload (normalization would weaken exact replay semantics).
func ValidateAgentHandoff(h AgentHandoff) error {
	_, err := encodeAgentHandoff(h)
	return err
}

func validateAgentHandoffFields(h AgentHandoff) error {
	if h.ChangedFiles == nil || h.VerificationCommands == nil {
		return errors.New("changedFiles and verificationCommands are required arrays")
	}
	if len(h.ChangedFiles) > MaxHandoffChangedFiles {
		return fmt.Errorf("changedFiles has %d entries; maximum is %d", len(h.ChangedFiles), MaxHandoffChangedFiles)
	}
	if len(h.VerificationCommands) > MaxHandoffVerificationCommands {
		return fmt.Errorf("verificationCommands has %d entries; maximum is %d", len(h.VerificationCommands), MaxHandoffVerificationCommands)
	}
	for i, path := range h.ChangedFiles {
		if !utf8.ValidString(path) {
			return fmt.Errorf("changedFiles[%d] must be valid UTF-8", i)
		}
		if strings.TrimSpace(path) == "" {
			return fmt.Errorf("changedFiles[%d] must not be empty", i)
		}
		if len(path) > MaxHandoffChangedFileBytes {
			return fmt.Errorf("changedFiles[%d] is too long", i)
		}
	}
	for i, command := range h.VerificationCommands {
		if !utf8.ValidString(command) {
			return fmt.Errorf("verificationCommands[%d] must be valid UTF-8", i)
		}
		if strings.TrimSpace(command) == "" {
			return fmt.Errorf("verificationCommands[%d] must not be empty", i)
		}
		if len(command) > MaxHandoffVerificationCommandBytes {
			return fmt.Errorf("verificationCommands[%d] is too long", i)
		}
	}
	if !utf8.ValidString(h.ResidualRisk) {
		return errors.New("residualRisk must be valid UTF-8")
	}
	if len(h.ResidualRisk) > MaxHandoffResidualRiskBytes {
		return errors.New("residualRisk is too long")
	}
	return nil
}

// EncodeAgentHandoff returns the sole canonical JSON persistence payload.
func EncodeAgentHandoff(h AgentHandoff) (string, error) {
	return encodeAgentHandoff(h)
}

func encodeAgentHandoff(h AgentHandoff) (string, error) {
	if err := validateAgentHandoffFields(h); err != nil {
		return "", err
	}
	payload, err := json.Marshal(h)
	if err != nil {
		return "", fmt.Errorf("encode agent handoff: %w", err)
	}
	if len(payload) > MaxHandoffPayloadBytes {
		return "", errors.New("agent handoff payload is too large")
	}
	return string(payload), nil
}

// DecodeAgentHandoff strictly parses and validates one durable JSON payload.
func DecodeAgentHandoff(payload string) (AgentHandoff, error) {
	if !utf8.ValidString(payload) {
		return AgentHandoff{}, errors.New("agent handoff payload must be valid UTF-8")
	}
	if len(payload) > MaxHandoffPayloadBytes {
		return AgentHandoff{}, errors.New("agent handoff payload is too large")
	}
	if err := rejectDuplicateHandoffKeys(payload); err != nil {
		return AgentHandoff{}, err
	}
	dec := json.NewDecoder(bytes.NewBufferString(payload))
	dec.DisallowUnknownFields()
	var handoff AgentHandoff
	if err := dec.Decode(&handoff); err != nil {
		return AgentHandoff{}, fmt.Errorf("decode agent handoff: %w", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return AgentHandoff{}, errors.New("decode agent handoff: expected one JSON value")
		}
		return AgentHandoff{}, fmt.Errorf("decode agent handoff: %w", err)
	}
	if _, err := encodeAgentHandoff(handoff); err != nil {
		return AgentHandoff{}, fmt.Errorf("validate agent handoff: %w", err)
	}
	return handoff, nil
}

// rejectDuplicateHandoffKeys scans the object before ordinary struct decode.
// encoding/json otherwise silently keeps the last duplicate, which would
// normalize distinct wire payloads and violate exact replay semantics.
func rejectDuplicateHandoffKeys(payload string) error {
	dec := json.NewDecoder(bytes.NewBufferString(payload))
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("decode agent handoff: %w", err)
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return errors.New("decode agent handoff: expected JSON object")
	}
	seen := map[string]struct{}{}
	required := map[string]struct{}{
		"changedFiles":         {},
		"verificationCommands": {},
		"residualRisk":         {},
	}
	for dec.More() {
		keyToken, err := dec.Token()
		if err != nil {
			return fmt.Errorf("decode agent handoff: %w", err)
		}
		key, ok := keyToken.(string)
		if !ok {
			return errors.New("decode agent handoff: expected object key")
		}
		if _, allowed := required[key]; !allowed {
			return fmt.Errorf("decode agent handoff: unknown or incorrectly-cased key %q", key)
		}
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("decode agent handoff: duplicate key %q", key)
		}
		seen[key] = struct{}{}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return fmt.Errorf("decode agent handoff: %w", err)
		}
	}
	if _, err := dec.Token(); err != nil {
		return fmt.Errorf("decode agent handoff: %w", err)
	}
	for key := range required {
		if _, ok := seen[key]; !ok {
			return fmt.Errorf("decode agent handoff: missing required key %q", key)
		}
	}
	if tok, err := dec.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("decode agent handoff: unexpected trailing token %v", tok)
		}
		return fmt.Errorf("decode agent handoff: %w", err)
	}
	return nil
}
