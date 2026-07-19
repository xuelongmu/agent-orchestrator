// Package scratch provides ephemeral, non-git session workspaces.
package scratch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

const removeAttempts = 6

var ErrUnsafePath = errors.New("scratch: unsafe workspace path")

type Workspace struct {
	managedRoot string
	removeAll   func(string) error
}

var _ ports.Workspace = (*Workspace)(nil)

func New(managedRoot string) (*Workspace, error) {
	if strings.TrimSpace(managedRoot) == "" {
		return nil, errors.New("scratch: managed root is required")
	}
	root, err := filepath.Abs(managedRoot)
	if err != nil {
		return nil, fmt.Errorf("scratch: managed root: %w", err)
	}
	root = filepath.Clean(root)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("scratch: create managed root: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = filepath.Clean(resolved)
	}
	return &Workspace{managedRoot: root, removeAll: os.RemoveAll}, nil
}

func (w *Workspace) Create(_ context.Context, cfg ports.WorkspaceConfig) (ports.WorkspaceInfo, error) {
	if err := validateConfig(cfg); err != nil {
		return ports.WorkspaceInfo{}, err
	}
	projectRoot := filepath.Join(w.managedRoot, string(cfg.ProjectID))
	if err := os.MkdirAll(projectRoot, 0o700); err != nil {
		return ports.WorkspaceInfo{}, fmt.Errorf("scratch: create project root: %w", err)
	}
	projectRoot, err := w.validateManagedPath(filepath.Clean(projectRoot))
	if err != nil {
		return ports.WorkspaceInfo{}, err
	}
	path, err := os.MkdirTemp(projectRoot, string(cfg.SessionID)+"-")
	if err != nil {
		return ports.WorkspaceInfo{}, fmt.Errorf("scratch: create workspace: %w", err)
	}
	return ports.WorkspaceInfo{
		Path:          filepath.Clean(path),
		WorkspaceKind: domain.WorkspaceKindScratch,
		SessionID:     cfg.SessionID,
		ProjectID:     cfg.ProjectID,
	}, nil
}

func (w *Workspace) Restore(_ context.Context, cfg ports.WorkspaceConfig) (ports.WorkspaceInfo, error) {
	if err := validateConfig(cfg); err != nil {
		return ports.WorkspaceInfo{}, err
	}
	path, err := w.validateManagedPath(cfg.Path)
	if err != nil {
		return ports.WorkspaceInfo{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ports.WorkspaceInfo{}, fmt.Errorf("scratch: restore %q: %w", path, ports.ErrWorkspaceStale)
		}
		return ports.WorkspaceInfo{}, fmt.Errorf("scratch: restore %q: %w", path, err)
	}
	if !info.IsDir() {
		return ports.WorkspaceInfo{}, fmt.Errorf("scratch: restore %q: not a directory", path)
	}
	return ports.WorkspaceInfo{Path: path, WorkspaceKind: domain.WorkspaceKindScratch, SessionID: cfg.SessionID, ProjectID: cfg.ProjectID}, nil
}

func (w *Workspace) Destroy(ctx context.Context, info ports.WorkspaceInfo) error {
	path, err := w.validateManagedPath(info.Path)
	if err != nil {
		return err
	}
	if err := w.removeDirWithRetry(ctx, path); err != nil {
		return fmt.Errorf("scratch: remove %q: %w", path, err)
	}
	return nil
}

func (w *Workspace) ForceDestroy(ctx context.Context, info ports.WorkspaceInfo) error {
	return w.Destroy(ctx, info)
}

func (*Workspace) StashUncommitted(context.Context, ports.WorkspaceInfo) (string, error) {
	return "", nil
}

func (*Workspace) ApplyPreserved(_ context.Context, _ ports.WorkspaceInfo, ref string) error {
	if ref != "" {
		return errors.New("scratch: preserved git state is not supported")
	}
	return nil
}

func (w *Workspace) removeDirWithRetry(ctx context.Context, path string) error {
	var last error
	for attempt := 0; attempt < removeAttempts; attempt++ {
		if err := w.removeAll(path); err == nil {
			return nil
		} else {
			last = err
		}
		if attempt == removeAttempts-1 {
			break
		}
		delay := time.Duration(25*(1<<attempt)) * time.Millisecond
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return last
}

func (w *Workspace) validateManagedPath(path string) (string, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", fmt.Errorf("%w: invalid path %q", ErrUnsafePath, path)
	}
	clean := filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(clean); err == nil {
		clean = filepath.Clean(resolved)
	}
	rel, err := filepath.Rel(w.managedRoot, clean)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w: %q is outside managed root %q", ErrUnsafePath, clean, w.managedRoot)
	}
	return clean, nil
}

func validateConfig(cfg ports.WorkspaceConfig) error {
	if cfg.ProjectID == "" || cfg.SessionID == "" {
		return errors.New("scratch: project id and session id are required")
	}
	for name, value := range map[string]string{"project id": string(cfg.ProjectID), "session id": string(cfg.SessionID)} {
		if value == "." || value == ".." || strings.ContainsAny(value, `/\`) {
			return fmt.Errorf("%w: %s %q contains path traversal", ErrUnsafePath, name, value)
		}
	}
	return nil
}
