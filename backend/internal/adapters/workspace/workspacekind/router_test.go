package workspacekind

import (
	"context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type recordingWorkspace struct {
	name      string
	called    int
	destroyed int
}

func (w *recordingWorkspace) Create(_ context.Context, cfg ports.WorkspaceConfig) (ports.WorkspaceInfo, error) {
	w.called++
	return ports.WorkspaceInfo{Path: w.name, WorkspaceKind: cfg.WorkspaceKind.WithDefault()}, nil
}
func (w *recordingWorkspace) Restore(ctx context.Context, cfg ports.WorkspaceConfig) (ports.WorkspaceInfo, error) {
	return w.Create(ctx, cfg)
}
func (w *recordingWorkspace) Destroy(context.Context, ports.WorkspaceInfo) error {
	w.destroyed++
	return nil
}
func (*recordingWorkspace) ForceDestroy(context.Context, ports.WorkspaceInfo) error { return nil }
func (*recordingWorkspace) StashUncommitted(context.Context, ports.WorkspaceInfo) (string, error) {
	return "", nil
}
func (*recordingWorkspace) ApplyPreserved(context.Context, ports.WorkspaceInfo, string) error {
	return nil
}

func TestCreateRoutesByKindAndDefaultsToWorktree(t *testing.T) {
	worktree := &recordingWorkspace{name: "worktree"}
	scratch := &recordingWorkspace{name: "scratch"}
	dir := &recordingWorkspace{name: "dir"}
	router, err := New(worktree, scratch, dir)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		kind domain.WorkspaceKind
		want *recordingWorkspace
	}{
		{kind: "", want: worktree},
		{kind: domain.WorkspaceKindWorktree, want: worktree},
		{kind: domain.WorkspaceKindScratch, want: scratch},
		{kind: domain.WorkspaceKindDir, want: dir},
	} {
		before := tc.want.called
		if _, err := router.Create(context.Background(), ports.WorkspaceConfig{WorkspaceKind: tc.kind}); err != nil {
			t.Fatalf("Create(%q): %v", tc.kind, err)
		}
		if tc.want.called != before+1 {
			t.Fatalf("Create(%q) did not call expected adapter", tc.kind)
		}
	}

	if _, err := router.Create(context.Background(), ports.WorkspaceConfig{WorkspaceKind: "clone"}); err == nil {
		t.Fatal("Create(clone) succeeded, want unknown-kind error")
	}
	if err := router.Destroy(context.Background(), ports.WorkspaceInfo{WorkspaceKind: domain.WorkspaceKindScratch}); err != nil {
		t.Fatal(err)
	}
	if scratch.destroyed != 1 || worktree.destroyed != 0 || dir.destroyed != 0 {
		t.Fatalf("destroy calls: worktree=%d scratch=%d dir=%d", worktree.destroyed, scratch.destroyed, dir.destroyed)
	}
}
