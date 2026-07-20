package sqlite

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
)

const sessionRenameGuard = "\n    OR OLD.display_name <> NEW.display_name"

func downTo(t *testing.T, db *sql.DB, version int64) {
	t.Helper()
	gooseMu.Lock()
	defer gooseMu.Unlock()
	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatalf("set dialect: %v", err)
	}
	if err := goose.DownTo(db, "migrations", version); err != nil {
		t.Fatalf("migrate down to %d: %v", version, err)
	}
}

func sessionUpdateTriggerSQL(t *testing.T, db *sql.DB) string {
	t.Helper()
	var trigger string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'trigger' AND name = 'sessions_cdc_update'`).Scan(&trigger); err != nil {
		t.Fatalf("read sessions_cdc_update: %v", err)
	}
	return trigger
}

func clearChangeLog(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`DELETE FROM change_log`); err != nil {
		t.Fatalf("clear change_log: %v", err)
	}
}

func sessionUpdatedCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM change_log WHERE event_type = 'session_updated'`).Scan(&count); err != nil {
		t.Fatalf("count session_updated events: %v", err)
	}
	return count
}

func TestMigration0041SessionRenameCDCRoundTrip(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "ao.db")+pragmas)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	upTo(t, db, 40)
	pre0041Trigger := sessionUpdateTriggerSQL(t, db)
	if strings.Contains(pre0041Trigger, sessionRenameGuard) {
		t.Fatalf("pre-0041 trigger unexpectedly watches display_name:\n%s", pre0041Trigger)
	}
	if _, err := db.Exec(`INSERT INTO projects (id, path, registered_at) VALUES ('mer', '/tmp/mer', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sessions (id, project_id, num, activity_last_at, created_at, updated_at) VALUES ('mer-1', 'mer', 1, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	clearChangeLog(t, db)

	if _, err := db.Exec(`UPDATE sessions SET display_name = 'before upgrade', updated_at = '2026-01-01T00:01:00Z' WHERE id = 'mer-1'`); err != nil {
		t.Fatal(err)
	}
	if got := sessionUpdatedCount(t, db); got != 0 {
		t.Fatalf("pre-0041 rename events = %d, want 0", got)
	}

	upTo(t, db, 41)
	upTrigger := sessionUpdateTriggerSQL(t, db)
	if strings.Count(upTrigger, sessionRenameGuard) != 1 {
		t.Fatalf("0041 trigger display_name guard count = %d, want 1:\n%s", strings.Count(upTrigger, sessionRenameGuard), upTrigger)
	}
	if withoutRenameGuard := strings.Replace(upTrigger, sessionRenameGuard, "", 1); withoutRenameGuard != pre0041Trigger {
		t.Fatalf("0041 changed more than the display_name guard\npre-0041:\n%s\n0041:\n%s", pre0041Trigger, upTrigger)
	}

	clearChangeLog(t, db)
	if _, err := db.Exec(`UPDATE sessions SET display_name = 'after upgrade', updated_at = '2026-01-01T00:02:00Z' WHERE id = 'mer-1'`); err != nil {
		t.Fatal(err)
	}
	if got := sessionUpdatedCount(t, db); got != 1 {
		t.Fatalf("0041 rename events = %d, want 1", got)
	}
	var rawPayload []byte
	if err := db.QueryRow(`SELECT payload FROM change_log WHERE event_type = 'session_updated'`).Scan(&rawPayload); err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(rawPayload, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if _, ok := payload["diagnosticTrigger"]; !ok {
		t.Fatalf("0041 payload lost diagnosticTrigger: %v", payload)
	}
	if _, ok := payload["displayName"]; ok {
		t.Fatalf("0041 payload must remain invalidation-only: %v", payload)
	}

	clearChangeLog(t, db)
	if _, err := db.Exec(`UPDATE sessions SET display_name = 'after upgrade', updated_at = '2026-01-01T00:03:00Z' WHERE id = 'mer-1'`); err != nil {
		t.Fatal(err)
	}
	if got := sessionUpdatedCount(t, db); got != 0 {
		t.Fatalf("same-name rename events = %d, want 0", got)
	}

	downTo(t, db, 40)
	if downTrigger := sessionUpdateTriggerSQL(t, db); downTrigger != pre0041Trigger {
		t.Fatalf("0041 down did not restore the exact pre-0041 trigger\nwant:\n%s\ngot:\n%s", pre0041Trigger, downTrigger)
	}
	clearChangeLog(t, db)
	if _, err := db.Exec(`UPDATE sessions SET display_name = 'after down', updated_at = '2026-01-01T00:04:00Z' WHERE id = 'mer-1'`); err != nil {
		t.Fatal(err)
	}
	if got := sessionUpdatedCount(t, db); got != 0 {
		t.Fatalf("post-down rename events = %d, want 0", got)
	}

	upTo(t, db, 41)
	if reupTrigger := sessionUpdateTriggerSQL(t, db); reupTrigger != upTrigger {
		t.Fatalf("0041 down/up trigger drifted\nfirst up:\n%s\nsecond up:\n%s", upTrigger, reupTrigger)
	}
	clearChangeLog(t, db)
	if _, err := db.Exec(`UPDATE sessions SET display_name = 'after re-up', updated_at = '2026-01-01T00:05:00Z' WHERE id = 'mer-1'`); err != nil {
		t.Fatal(err)
	}
	if got := sessionUpdatedCount(t, db); got != 1 {
		t.Fatalf("post-re-up rename events = %d, want 1", got)
	}
}
