package scratch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestCreateRestoreAndDestroy(t *testing.T) {
	root := t.TempDir()
	ws, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg := ports.WorkspaceConfig{ProjectID: "demo", SessionID: "demo-1", WorkspaceKind: domain.WorkspaceKindScratch}
	created, err := ws.Create(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if created.WorkspaceKind != domain.WorkspaceKindScratch || created.Branch != "" {
		t.Fatalf("created = %#v, want branchless scratch workspace", created)
	}
	if info, err := os.Stat(created.Path); err != nil || !info.IsDir() {
		t.Fatalf("scratch path = %q: info=%v err=%v", created.Path, info, err)
	}
	if _, err := os.Stat(filepath.Join(created.Path, ".git")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("scratch workspace unexpectedly contains .git: %v", err)
	}
	cfg.Path = created.Path
	restored, err := ws.Restore(context.Background(), cfg)
	if err != nil || restored.Path != created.Path {
		t.Fatalf("Restore() = %#v, %v", restored, err)
	}
	if err := ws.Destroy(context.Background(), created); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(created.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("destroy left workspace behind: %v", err)
	}
	if _, err := ws.Restore(context.Background(), cfg); !errors.Is(err, ports.ErrWorkspaceStale) {
		t.Fatalf("restore destroyed workspace error = %v, want ErrWorkspaceStale", err)
	}
}

func TestDestroyRetriesTransientRemovalFailure(t *testing.T) {
	ws, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	created, err := ws.Create(context.Background(), ports.WorkspaceConfig{ProjectID: "demo", SessionID: "demo-1"})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	ws.removeAll = func(path string) error {
		calls++
		if calls < 3 {
			return errors.New("sharing violation")
		}
		return os.RemoveAll(path)
	}
	if err := ws.Destroy(context.Background(), created); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("remove attempts = %d, want 3", calls)
	}
}

func TestRejectsUnsafeIDsAndDestroyPaths(t *testing.T) {
	ws, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ws.Create(context.Background(), ports.WorkspaceConfig{ProjectID: "../demo", SessionID: "demo-1"}); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("unsafe create error = %v", err)
	}
	if err := ws.Destroy(context.Background(), ports.WorkspaceInfo{Path: filepath.Clean(filepath.Join(ws.managedRoot, "..", "outside"))}); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("unsafe destroy error = %v", err)
	}
}
