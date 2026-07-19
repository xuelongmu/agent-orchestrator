package sqlite

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestMigration0034BackfillsExistingPRDesignContracts(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "ao.db")+pragmas)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	upTo(t, db, 33)
	if _, err := db.Exec(`INSERT INTO projects (id, path, registered_at) VALUES ('mer', '/tmp/mer', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sessions (id, project_id, num, activity_last_at, created_at, updated_at) VALUES ('mer-1', 'mer', 1, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO pr (url, session_id, number, updated_at) VALUES ('https://github.com/o/r/pull/1', 'mer-1', 1, '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	upTo(t, db, 34)
	var contract string
	if err := db.QueryRow(`SELECT markdown FROM pr_design_contract WHERE pr_url = 'https://github.com/o/r/pull/1'`).Scan(&contract); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(contract, "UNTRUSTED") && !strings.Contains(contract, "Trust boundary") {
		t.Fatalf("backfilled contract lacks trust boundary: %q", contract)
	}
}
