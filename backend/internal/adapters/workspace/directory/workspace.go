// Package directory exposes an existing project directory as a shared,
// non-git-isolated session workspace.
package directory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Workspace exposes the registered project directory without filesystem
// isolation and never removes that shared directory.
type Workspace struct{}

var _ ports.Workspace = (*Workspace)(nil)

// New returns a shared-directory workspace adapter.
func New() *Workspace { return &Workspace{} }

// Create attaches a session to its registered project directory.
func (*Workspace) Create(_ context.Context, cfg ports.WorkspaceConfig) (ports.WorkspaceInfo, error) {
	return workspaceInfo(cfg, cfg.RepoPath)
}

// Restore reattaches a session to its persisted shared directory.
func (*Workspace) Restore(_ context.Context, cfg ports.WorkspaceConfig) (ports.WorkspaceInfo, error) {
	return workspaceInfo(cfg, cfg.Path)
}

// Destroy intentionally leaves the shared directory untouched.
func (*Workspace) Destroy(context.Context, ports.WorkspaceInfo) error { return nil }

// ForceDestroy intentionally leaves the shared directory untouched.
func (w *Workspace) ForceDestroy(ctx context.Context, info ports.WorkspaceInfo) error {
	return w.Destroy(ctx, info)
}

// StashUncommitted is a no-op because directory workspaces have no AO-managed
// git preservation lifecycle.
func (*Workspace) StashUncommitted(context.Context, ports.WorkspaceInfo) (string, error) {
	return "", nil
}

// ApplyPreserved accepts only the empty preservation reference used by non-git
// workspace lifecycle paths.
func (*Workspace) ApplyPreserved(_ context.Context, _ ports.WorkspaceInfo, ref string) error {
	if ref != "" {
		return errors.New("directory: preserved git state is not supported")
	}
	return nil
}

func workspaceInfo(cfg ports.WorkspaceConfig, rawPath string) (ports.WorkspaceInfo, error) {
	if cfg.ProjectID == "" || cfg.SessionID == "" {
		return ports.WorkspaceInfo{}, errors.New("directory: project id and session id are required")
	}
	if strings.TrimSpace(rawPath) == "" {
		return ports.WorkspaceInfo{}, errors.New("directory: project path is required")
	}
	path, err := filepath.Abs(rawPath)
	if err != nil {
		return ports.WorkspaceInfo{}, fmt.Errorf("directory: resolve path: %w", err)
	}
	path = filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = filepath.Clean(resolved)
	}
	info, err := os.Stat(path)
	if err != nil {
		return ports.WorkspaceInfo{}, fmt.Errorf("directory: inspect %q: %w", path, err)
	}
	if !info.IsDir() {
		return ports.WorkspaceInfo{}, fmt.Errorf("directory: %q is not a directory", path)
	}
	return ports.WorkspaceInfo{Path: path, WorkspaceKind: domain.WorkspaceKindDir, SessionID: cfg.SessionID, ProjectID: cfg.ProjectID}, nil
}
