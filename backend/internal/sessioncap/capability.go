// Package sessioncap issues verifier capabilities bound to one session and project.
package sessioncap

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"os"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

const (
	// EnvVerificationCapability is injected only into the owning session runtime.
	EnvVerificationCapability = "AO_VERIFY_CAPABILITY"
	keyFileName               = "verification-capability.key"
	keyBytes                  = 32
)

// Manager signs and verifies session/project-bound capabilities.
type Manager struct{ key [keyBytes]byte }

// Open loads or atomically creates the daemon's private capability key.
func Open(dataDir string) (*Manager, error) {
	root, err := os.OpenRoot(dataDir)
	if err != nil {
		return nil, fmt.Errorf("open capability root: %w", err)
	}
	defer func() { _ = root.Close() }()
	file, err := root.OpenFile(keyFileName, os.O_RDONLY, 0)
	if os.IsNotExist(err) {
		return create(root)
	}
	if err != nil {
		return nil, fmt.Errorf("open capability key: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != keyBytes {
		return nil, fmt.Errorf("capability key must be a %d-byte regular file", keyBytes)
	}
	manager := &Manager{}
	if _, err := io.ReadFull(file, manager.key[:]); err != nil {
		return nil, fmt.Errorf("read capability key: %w", err)
	}
	return manager, nil
}

// NewEphemeral creates a process-lifetime manager for tests.
func NewEphemeral() (*Manager, error) {
	manager := &Manager{}
	if _, err := rand.Read(manager.key[:]); err != nil {
		return nil, fmt.Errorf("generate capability key: %w", err)
	}
	return manager, nil
}

func create(root *os.Root) (*Manager, error) {
	manager, err := NewEphemeral()
	if err != nil {
		return nil, err
	}
	file, err := root.OpenFile(keyFileName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create capability key: %w", err)
	}
	if _, err := file.Write(manager.key[:]); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("write capability key: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close capability key: %w", err)
	}
	return manager, nil
}

// Token returns an opaque capability scoped to session and project.
func (m *Manager) Token(session domain.SessionID, project domain.ProjectID) string {
	mac := hmac.New(sha256.New, m.key[:])
	_, _ = io.WriteString(mac, string(session))
	_, _ = mac.Write([]byte{0})
	_, _ = io.WriteString(mac, string(project))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Verify performs constant-time verification for the requested session/project.
func (m *Manager) Verify(session domain.SessionID, project domain.ProjectID, capability string) bool {
	provided, err := base64.RawURLEncoding.DecodeString(capability)
	if err != nil {
		return false
	}
	expected, _ := base64.RawURLEncoding.DecodeString(m.Token(session, project))
	return hmac.Equal(provided, expected)
}
