package directory

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestCreateUsesProjectPathAndDestroyPreservesIt(t *testing.T) {
	path := t.TempDir()
	marker := filepath.Join(path, "shared.txt")
	if err := os.WriteFile(marker, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	ws := New()
	created, err := ws.Create(context.Background(), ports.WorkspaceConfig{ProjectID: "demo", SessionID: "demo-1", RepoPath: path})
	if err != nil {
		t.Fatal(err)
	}
	if created.WorkspaceKind != domain.WorkspaceKindDir || created.Branch != "" || created.Path != path {
		t.Fatalf("created = %#v", created)
	}
	if err := ws.Destroy(context.Background(), created); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("shared directory was modified during destroy: %v", err)
	}
}
