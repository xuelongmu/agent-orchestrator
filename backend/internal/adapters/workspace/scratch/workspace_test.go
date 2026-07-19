package scratch

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	if err := ws.Destroy(context.Background(), ports.WorkspaceInfo{ProjectID: "demo", SessionID: "demo-1", Path: filepath.Join(ws.managedRoot, "demo", "other")}); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("unsafe destroy error = %v", err)
	}
}

func TestDestroyRejectsMismatchedWorkspaceIdentity(t *testing.T) {
	ws, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	created, err := ws.Create(context.Background(), ports.WorkspaceConfig{ProjectID: "demo", SessionID: "demo-1"})
	if err != nil {
		t.Fatal(err)
	}
	wrong := created
	wrong.SessionID = "demo-2"
	if err := ws.Destroy(context.Background(), wrong); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("mismatched destroy error = %v, want ErrUnsafePath", err)
	}
	if _, err := os.Stat(created.Path); err != nil {
		t.Fatalf("mismatched destroy removed owned workspace: %v", err)
	}
}

func TestDestroyDoesNotFollowSubstitutedWorkspaceLink(t *testing.T) {
	tests := []struct {
		name string
		link func(string, string) error
	}{
		{name: "symlink", link: func(target, path string) error { return os.Symlink(target, path) }},
	}
	if runtime.GOOS == "windows" {
		tests = append(tests, struct {
			name string
			link func(string, string) error
		}{name: "junction", link: createJunction})
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ws, err := New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			victim, err := ws.Create(context.Background(), ports.WorkspaceConfig{ProjectID: "demo", SessionID: "victim"})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(victim.Path, "keep.txt"), []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
			attacker, err := ws.Create(context.Background(), ports.WorkspaceConfig{ProjectID: "demo", SessionID: "attacker"})
			if err != nil {
				t.Fatal(err)
			}
			moved := attacker.Path + "-moved"
			if err := os.Rename(attacker.Path, moved); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(moved, "own.txt"), []byte("own"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := tc.link(victim.Path, attacker.Path); err != nil {
				t.Skipf("link creation unavailable: %v", err)
			}

			if err := ws.Destroy(context.Background(), attacker); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(victim.Path, "keep.txt")); err != nil {
				t.Fatalf("destroy followed substituted %s into victim: %v", tc.name, err)
			}
			if _, err := os.Stat(filepath.Join(moved, "own.txt")); err != nil {
				t.Fatalf("destroy removed relocated owned directory: %v", err)
			}
			if _, err := os.Lstat(attacker.Path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("substituted %s still exists: %v", tc.name, err)
			}
		})
	}
}

func createJunction(target, path string) error {
	output, err := exec.Command("cmd", "/c", "mklink", "/J", path, target).CombinedOutput()
	if err != nil {
		return errors.New(string(output))
	}
	return nil
}
