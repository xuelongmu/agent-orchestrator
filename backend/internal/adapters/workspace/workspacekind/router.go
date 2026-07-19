// Package workspacekind routes workspace lifecycle operations by the durable
// per-session workspace kind.
package workspacekind

import (
	"context"
	"errors"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Router delegates each workspace operation to the adapter selected by the
// durable workspace kind.
type Router struct {
	worktree ports.Workspace
	scratch  ports.Workspace
	dir      ports.Workspace
}

var _ ports.Workspace = (*Router)(nil)
var _ ports.WorkspaceProject = (*Router)(nil)

// New constructs a workspace-kind router from all supported adapters.
func New(worktree, scratch, dir ports.Workspace) (*Router, error) {
	if worktree == nil || scratch == nil || dir == nil {
		return nil, errors.New("workspace router: worktree, scratch, and dir adapters are required")
	}
	return &Router{worktree: worktree, scratch: scratch, dir: dir}, nil
}

func (r *Router) adapter(kind domain.WorkspaceKind) (ports.Workspace, error) {
	switch kind.WithDefault() {
	case domain.WorkspaceKindWorktree:
		return r.worktree, nil
	case domain.WorkspaceKindScratch:
		return r.scratch, nil
	case domain.WorkspaceKindDir:
		return r.dir, nil
	default:
		return nil, fmt.Errorf("workspace router: unknown workspace kind %q", kind)
	}
}

// Create delegates workspace creation to the requested kind.
func (r *Router) Create(ctx context.Context, cfg ports.WorkspaceConfig) (ports.WorkspaceInfo, error) {
	a, err := r.adapter(cfg.WorkspaceKind)
	if err != nil {
		return ports.WorkspaceInfo{}, err
	}
	return a.Create(ctx, cfg)
}

// Restore delegates workspace restoration to the persisted kind.
func (r *Router) Restore(ctx context.Context, cfg ports.WorkspaceConfig) (ports.WorkspaceInfo, error) {
	a, err := r.adapter(cfg.WorkspaceKind)
	if err != nil {
		return ports.WorkspaceInfo{}, err
	}
	return a.Restore(ctx, cfg)
}

// Destroy delegates safe workspace teardown to the persisted kind.
func (r *Router) Destroy(ctx context.Context, info ports.WorkspaceInfo) error {
	a, err := r.adapter(info.WorkspaceKind)
	if err != nil {
		return err
	}
	return a.Destroy(ctx, info)
}

// ForceDestroy delegates forced teardown to the persisted kind.
func (r *Router) ForceDestroy(ctx context.Context, info ports.WorkspaceInfo) error {
	a, err := r.adapter(info.WorkspaceKind)
	if err != nil {
		return err
	}
	return a.ForceDestroy(ctx, info)
}

// StashUncommitted delegates state preservation to the persisted kind.
func (r *Router) StashUncommitted(ctx context.Context, info ports.WorkspaceInfo) (string, error) {
	a, err := r.adapter(info.WorkspaceKind)
	if err != nil {
		return "", err
	}
	return a.StashUncommitted(ctx, info)
}

// ApplyPreserved delegates preserved-state replay to the persisted kind.
func (r *Router) ApplyPreserved(ctx context.Context, info ports.WorkspaceInfo, ref string) error {
	a, err := r.adapter(info.WorkspaceKind)
	if err != nil {
		return err
	}
	return a.ApplyPreserved(ctx, info, ref)
}

// CreateWorkspaceProject preserves the existing multi-repository worktree
// extension by delegating it to the worktree adapter.
func (r *Router) CreateWorkspaceProject(ctx context.Context, cfg ports.WorkspaceProjectConfig) (ports.WorkspaceProjectInfo, error) {
	a, ok := r.worktree.(ports.WorkspaceProject)
	if !ok {
		return ports.WorkspaceProjectInfo{}, errors.New("workspace router: worktree adapter does not support workspace projects")
	}
	return a.CreateWorkspaceProject(ctx, cfg)
}

// DestroyWorkspaceProject preserves the existing multi-repository worktree
// extension by delegating it to the worktree adapter.
func (r *Router) DestroyWorkspaceProject(ctx context.Context, info ports.WorkspaceProjectInfo) error {
	a, ok := r.worktree.(ports.WorkspaceProject)
	if !ok {
		return errors.New("workspace router: worktree adapter does not support workspace projects")
	}
	return a.DestroyWorkspaceProject(ctx, info)
}
