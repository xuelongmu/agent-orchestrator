package sqlite

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestTrackerIntakeSessionLookupUsesProjectIssueIndex(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "ao.db")+pragmas)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := migrate(db); err != nil {
		t.Fatal(err)
	}

	rows, err := db.Query(`EXPLAIN QUERY PLAN
SELECT id FROM sessions
WHERE project_id = ? AND issue_id = ? AND is_terminated = FALSE
  AND (workspace_path <> '' OR runtime_handle_id <> '' OR agent_session_id <> '' OR prompt <> '')`, "demo", "github:acme/demo#28")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plans []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plans = append(plans, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	detail := strings.Join(plans, "\n")
	if !strings.Contains(detail, "idx_sessions_project_issue") {
		t.Fatalf("query plan does not use idx_sessions_project_issue:\n%s", detail)
	}
}
