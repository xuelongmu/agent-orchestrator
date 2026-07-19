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

// ErrUnsafePath reports a scratch path or identifier outside the adapter's
// managed root.
var ErrUnsafePath = errors.New("scratch: unsafe workspace path")

// Workspace manages ephemeral, non-git session directories below one guarded
// root.
type Workspace struct {
	managedRoot     string
	managedRootInfo os.FileInfo
	removeAll       func(string) error
}

var _ ports.Workspace = (*Workspace)(nil)

// New constructs a scratch adapter rooted at managedRoot.
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
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return nil, fmt.Errorf("scratch: inspect managed root: %w", err)
	}
	if isLinkLike(rootInfo) || !rootInfo.IsDir() {
		return nil, errors.New("scratch: managed root must be a real directory")
	}
	return &Workspace{managedRoot: root, managedRootInfo: rootInfo, removeAll: os.RemoveAll}, nil
}

// Create makes a new empty directory for the requested session.
func (w *Workspace) Create(_ context.Context, cfg ports.WorkspaceConfig) (ports.WorkspaceInfo, error) {
	if err := validateConfig(cfg); err != nil {
		return ports.WorkspaceInfo{}, err
	}
	projectRoot, err := w.projectRoot(cfg.ProjectID, true)
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

// Restore reattaches an existing scratch directory after validating that it is
// still inside the managed root.
func (w *Workspace) Restore(_ context.Context, cfg ports.WorkspaceConfig) (ports.WorkspaceInfo, error) {
	if err := validateConfig(cfg); err != nil {
		return ports.WorkspaceInfo{}, err
	}
	path, err := w.validateOwnedPath(cfg.ProjectID, cfg.SessionID, cfg.Path)
	if err != nil {
		return ports.WorkspaceInfo{}, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ports.WorkspaceInfo{}, fmt.Errorf("scratch: restore %q: %w", path, ports.ErrWorkspaceStale)
		}
		return ports.WorkspaceInfo{}, fmt.Errorf("scratch: restore %q: %w", path, err)
	}
	if isLinkLike(info) || !info.IsDir() {
		return ports.WorkspaceInfo{}, fmt.Errorf("scratch: restore %q: not a directory", path)
	}
	return ports.WorkspaceInfo{Path: path, WorkspaceKind: domain.WorkspaceKindScratch, SessionID: cfg.SessionID, ProjectID: cfg.ProjectID}, nil
}

// Destroy removes a scratch directory with retries for transient file-handle
// contention.
func (w *Workspace) Destroy(ctx context.Context, info ports.WorkspaceInfo) error {
	path, err := w.validateOwnedPath(info.ProjectID, info.SessionID, info.Path)
	if err != nil {
		return err
	}
	entry, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("scratch: inspect %q: %w", path, err)
	}
	if isLinkLike(entry) {
		if err := removeLinkWithRetry(ctx, path); err != nil {
			return fmt.Errorf("scratch: remove substituted link %q: %w", path, err)
		}
		return nil
	}
	if !entry.IsDir() {
		return fmt.Errorf("%w: workspace entry %q is not a directory", ErrUnsafePath, path)
	}
	if err := w.removeDirWithRetry(ctx, path); err != nil {
		return fmt.Errorf("scratch: remove %q: %w", path, err)
	}
	return nil
}

// ForceDestroy uses the same guarded removal as Destroy because scratch
// workspaces have no dirty-git refusal.
func (w *Workspace) ForceDestroy(ctx context.Context, info ports.WorkspaceInfo) error {
	return w.Destroy(ctx, info)
}

// StashUncommitted is a no-op because scratch workspaces contain no managed git
// state.
func (*Workspace) StashUncommitted(context.Context, ports.WorkspaceInfo) (string, error) {
	return "", nil
}

// ApplyPreserved accepts only the empty preservation reference used by non-git
// workspace lifecycle paths.
func (*Workspace) ApplyPreserved(_ context.Context, _ ports.WorkspaceInfo, ref string) error {
	if ref != "" {
		return errors.New("scratch: preserved git state is not supported")
	}
	return nil
}

func (w *Workspace) removeDirWithRetry(ctx context.Context, path string) error {
	var last error
	for attempt := 0; attempt < removeAttempts; attempt++ {
		err := w.removeAll(path)
		if err == nil {
			return nil
		}
		last = err
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

func (w *Workspace) validateOwnedPath(projectID domain.ProjectID, sessionID domain.SessionID, path string) (string, error) {
	if err := validateConfig(ports.WorkspaceConfig{ProjectID: projectID, SessionID: sessionID}); err != nil {
		return "", err
	}
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", fmt.Errorf("%w: invalid path %q", ErrUnsafePath, path)
	}
	clean := filepath.Clean(path)
	expectedProjectRoot := filepath.Clean(filepath.Join(w.managedRoot, string(projectID)))
	if filepath.Clean(filepath.Dir(clean)) != expectedProjectRoot || !strings.HasPrefix(filepath.Base(clean), string(sessionID)+"-") {
		return "", fmt.Errorf("%w: %q is not owned by project %s session %s", ErrUnsafePath, clean, projectID, sessionID)
	}
	projectRoot, err := w.projectRoot(projectID, false)
	if err != nil {
		return "", err
	}
	if expectedProjectRoot != projectRoot {
		return "", fmt.Errorf("%w: project root identity changed", ErrUnsafePath)
	}
	return clean, nil
}

func (w *Workspace) projectRoot(projectID domain.ProjectID, create bool) (string, error) {
	rootInfo, err := os.Lstat(w.managedRoot)
	if err != nil || isLinkLike(rootInfo) || !rootInfo.IsDir() || !os.SameFile(rootInfo, w.managedRootInfo) {
		return "", fmt.Errorf("%w: managed root identity changed", ErrUnsafePath)
	}
	projectRoot := filepath.Join(w.managedRoot, string(projectID))
	if create {
		if err := os.MkdirAll(projectRoot, 0o700); err != nil {
			return "", fmt.Errorf("scratch: create project root: %w", err)
		}
	}
	info, err := os.Lstat(projectRoot)
	if err != nil {
		return "", fmt.Errorf("scratch: inspect project root: %w", err)
	}
	if isLinkLike(info) || !info.IsDir() {
		return "", fmt.Errorf("%w: project root %q is not a real directory", ErrUnsafePath, projectRoot)
	}
	return filepath.Clean(projectRoot), nil
}

func removeLinkWithRetry(ctx context.Context, path string) error {
	var last error
	for attempt := 0; attempt < removeAttempts; attempt++ {
		if err := os.Remove(path); err == nil || errors.Is(err, os.ErrNotExist) {
			return nil
		} else {
			last = err
		}
		if attempt == removeAttempts-1 {
			break
		}
		timer := time.NewTimer(time.Duration(25*(1<<attempt)) * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return last
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
