package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestValidateDependenciesRejectsCycle(t *testing.T) {
	db := dependencyTestDB(t)
	for _, statement := range []string{
		`INSERT INTO sessions (id, project_id) VALUES ('ao-a', 'ao'), ('ao-b', 'ao')`,
		`INSERT INTO session_dependencies (session_id, depends_on_session_id) VALUES ('ao-b', 'ao-a')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}

	err := validateDependencies(context.Background(), db, "ao-a", "ao", []domain.SessionID{"ao-b"})
	if !errors.Is(err, ports.ErrDependencyCycle) {
		t.Fatalf("cycle validation error = %v, want ErrDependencyCycle", err)
	}
}

func dependencyTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "dependencies.db")+"?_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, statement := range []string{
		`CREATE TABLE sessions (id TEXT PRIMARY KEY, project_id TEXT NOT NULL)`,
		`CREATE TABLE session_dependencies (
            session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
            depends_on_session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE RESTRICT,
            PRIMARY KEY (session_id, depends_on_session_id),
            CHECK (session_id <> depends_on_session_id)
        )`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	return db
}
