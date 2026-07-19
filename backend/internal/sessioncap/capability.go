// Package sessioncap issues verifier capabilities bound to one session and project.
package sessioncap

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
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
	manager, err := load(root)
	if os.IsNotExist(err) {
		return create(root)
	}
	if errors.Is(err, errPartialKey) {
		if removeErr := root.Remove(keyFileName); removeErr != nil {
			return nil, fmt.Errorf("remove incomplete capability key: %w", removeErr)
		}
		return create(root)
	}
	return manager, err
}

var errPartialKey = errors.New("incomplete capability key")

func load(root *os.Root) (*Manager, error) {
	file, err := root.OpenFile(keyFileName, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat capability key: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("capability key must be a regular file")
	}
	if info.Size() != keyBytes {
		return nil, errPartialKey
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
	tempEntropy := make([]byte, 12)
	if _, err := rand.Read(tempEntropy); err != nil {
		return nil, fmt.Errorf("name capability key temporary file: %w", err)
	}
	tempName := fmt.Sprintf(".%s.tmp-%x", keyFileName, tempEntropy)
	file, err := root.OpenFile(tempName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create capability key temporary file: %w", err)
	}
	defer func() { _ = root.Remove(tempName) }()
	if n, err := file.Write(manager.key[:]); err != nil || n != keyBytes {
		_ = file.Close()
		if err == nil {
			err = io.ErrShortWrite
		}
		return nil, fmt.Errorf("write capability key: wrote %d bytes: %w", n, err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("sync capability key: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close capability key: %w", err)
	}
	if err := root.Link(tempName, keyFileName); err != nil {
		if os.IsExist(err) {
			return load(root)
		}
		return nil, fmt.Errorf("publish capability key: %w", err)
	}
	if dir, err := root.Open("."); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
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
