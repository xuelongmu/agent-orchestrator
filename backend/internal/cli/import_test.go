package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/coordination"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/legacyimport"
	"github.com/aoagents/agent-orchestrator/backend/internal/runfile"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

func writeLegacyProject(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), ".agent-orchestrator")
	if err := os.MkdirAll(filepath.Join(root, "projects", "alpha", "sessions"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.yaml"),
		[]byte("projects:\n  alpha:\n    path: /repos/alpha\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestImportCommand_NoLegacyData(t *testing.T) {
	setConfigEnv(t)
	empty := filepath.Join(t.TempDir(), "nope")
	out, _, err := executeCLI(t, Deps{}, "import", "--from", empty, "--yes")
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if !strings.Contains(out, "Nothing to import") {
		t.Fatalf("out = %q, want 'Nothing to import'", out)
	}
}

func TestImportCommand_ImportsProjectJSON(t *testing.T) {
	setConfigEnv(t)
	root := writeLegacyProject(t)

	out, _, err := executeCLI(t, Deps{}, "import", "--from", root, "--yes", "--json")
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	var rep legacyimport.Report
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("parse report %q: %v", out, err)
	}
	if rep.ProjectsImported != 1 {
		t.Fatalf("projectsImported = %d, want 1", rep.ProjectsImported)
	}
}

func TestImportCommand_SurfacesParseErrorOnce(t *testing.T) {
	setConfigEnv(t)
	// A tab-indented line is a YAML syntax error (not a *yaml.TypeError), so
	// loadLegacyConfig surfaces it exactly like issue #2186 describes.
	root := filepath.Join(t.TempDir(), ".agent-orchestrator")
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.yaml"),
		[]byte("projects:\n\talpha:\n  path: /repos/alpha\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, stderr, err := executeCLI(t, Deps{}, "import", "--from", root, "--yes")
	if err == nil {
		t.Fatal("import: want non-nil error for a config.yaml that fails to parse")
	}
	if got := ExitCode(err); got != 1 {
		t.Fatalf("ExitCode = %d, want 1 (runtime failure)", got)
	}
	// The parse error must reach the user exactly once. main.go prints the
	// returned error, so the command itself must not also Fprintf it; that
	// doubled the output before this fix.
	count := strings.Count(stderr, "parse legacy config.yaml") + strings.Count(err.Error(), "parse legacy config.yaml")
	if count != 1 {
		t.Fatalf("parse error appeared %d time(s) (stderr=%q, err=%q), want exactly 1", count, stderr, err)
	}
}

func TestImportCommand_RefusesWhenDaemonRunning(t *testing.T) {
	cfg := setConfigEnv(t)
	root := writeLegacyProject(t)

	// A run-file owned by this (alive) process makes the daemon look live.
	if err := runfile.Write(cfg.runFile, runfile.Info{PID: os.Getpid(), Port: 3001, StartedAt: time.Now()}); err != nil {
		t.Fatalf("write run-file: %v", err)
	}

	_, _, err := executeCLI(t, Deps{}, "import", "--from", root, "--yes")
	if err == nil || !strings.Contains(err.Error(), "daemon is running") {
		t.Fatalf("err = %v, want refusal because daemon is running", err)
	}
}

func TestImportCommandRefusesWhenDaemonClaimsAfterPreflight(t *testing.T) {
	cfg := setConfigEnv(t)
	root := writeLegacyProject(t)
	var holderStore *sqlite.Store
	var holderLease *coordination.Lease
	deps := Deps{OpenExclusiveStore: func(ctx context.Context, dataDir string, ownerPID int) (*sqlite.Store, *coordination.Lease, error) {
		// runImport's run-file preflight and confirmation have already completed
		// when executeImport calls OpenExclusiveStore. Claim as a concurrently starting
		// daemon in this exact TOCTOU window, then return the import connection.
		var err error
		holderStore, err = sqlite.Open(dataDir)
		if err != nil {
			return nil, nil, err
		}
		holderLease, err = coordination.Acquire(context.Background(), holderStore, os.Getpid())
		if err != nil {
			_ = holderStore.Close()
			return nil, nil, err
		}
		return coordination.OpenExclusive(ctx, dataDir, ownerPID)
	}}
	t.Cleanup(func() {
		if holderLease != nil {
			_ = holderLease.Release(context.Background())
		}
		if holderStore != nil {
			_ = holderStore.Close()
		}
	})

	_, _, err := executeCLI(t, deps, "import", "--from", root, "--yes")
	if err == nil || !strings.Contains(err.Error(), "exclusive database writer leased") {
		t.Fatalf("err = %v, want post-preflight lease refusal (data dir %s)", err, cfg.dataDir)
	}
}

func TestImportCommandDryRunDoesNotTakeWriterLease(t *testing.T) {
	cfg := setConfigEnv(t)
	root := writeLegacyProject(t)
	holderStore, err := sqlite.Open(cfg.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holderStore.Close() })
	holderLease, err := coordination.Acquire(context.Background(), holderStore, os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holderLease.Release(context.Background()) })

	out, _, err := executeCLI(t, Deps{}, "import", "--from", root, "--dry-run", "--yes")
	if err != nil {
		t.Fatalf("dry-run under an active writer lease: %v", err)
	}
	if !strings.Contains(out, "Dry run -- no changes written") {
		t.Fatalf("dry-run output = %q", out)
	}
}

func TestImportCommandDryRunDoesNotCreateAbsentSourceStorage(t *testing.T) {
	cfg := setConfigEnv(t)
	root := writeLegacyProject(t)
	before := captureDirectoryState(t, cfg.dataDir)

	rep := runImportDryRunJSON(t, root)
	if rep.ProjectsImported != 1 || rep.ProjectsSkipped != 0 {
		t.Fatalf("dry-run projects: imported=%d skipped=%d, want 1/0", rep.ProjectsImported, rep.ProjectsSkipped)
	}
	assertDirectoryStateUnchanged(t, cfg.dataDir, before)
	if _, err := os.Stat(filepath.Join(cfg.dataDir, "ao.db.lock")); !os.IsNotExist(err) {
		t.Fatalf("dry-run created source lock: err=%v", err)
	}
}

func TestImportCommandDryRunPreservesExistingSourceAndSkipsProjects(t *testing.T) {
	cfg := setConfigEnv(t)
	root := writeLegacyProject(t)
	store, err := sqlite.Open(cfg.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertProject(context.Background(), storedAlphaProject()); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	before := captureDirectoryState(t, cfg.dataDir)

	rep := runImportDryRunJSON(t, root)
	if rep.ProjectsImported != 0 || rep.ProjectsSkipped != 1 {
		t.Fatalf("dry-run projects: imported=%d skipped=%d, want 0/1", rep.ProjectsImported, rep.ProjectsSkipped)
	}
	assertDirectoryStateUnchanged(t, cfg.dataDir, before)
	if _, err := os.Stat(filepath.Join(cfg.dataDir, "ao.db.lock")); !os.IsNotExist(err) {
		t.Fatalf("dry-run created source lock: err=%v", err)
	}
}

func TestImportCommandDryRunCopiesPersistedWALWithoutMutatingSource(t *testing.T) {
	cfg := setConfigEnv(t)
	root := writeLegacyProject(t)
	store, err := sqlite.Open(cfg.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	checkpointDB, err := sql.Open("sqlite", "file:"+filepath.Join(cfg.dataDir, "ao.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := checkpointDB.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		_ = checkpointDB.Close()
		t.Fatalf("checkpoint database before WAL-only write: %v", err)
	}
	if err := checkpointDB.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertProject(context.Background(), storedAlphaProject()); err != nil {
		t.Fatal(err)
	}
	walInfo, err := os.Stat(filepath.Join(cfg.dataDir, "ao.db-wal"))
	if err != nil {
		t.Fatalf("stat persisted WAL: %v", err)
	}
	if walInfo.Size() == 0 {
		t.Fatal("persisted WAL is empty; test would not exercise WAL reconciliation")
	}
	before := captureDirectoryState(t, cfg.dataDir)
	mainOnlyDir := t.TempDir()
	if err := copyImportSnapshotFile(
		filepath.Join(cfg.dataDir, "ao.db"),
		filepath.Join(mainOnlyDir, "ao.db"),
	); err != nil {
		t.Fatalf("copy main database without sidecars: %v", err)
	}
	mainOnlyStore, err := sqlite.Open(mainOnlyDir)
	if err != nil {
		t.Fatalf("open main-only database snapshot: %v", err)
	}
	mainOnlyReport, err := legacyimport.Run(context.Background(), mainOnlyStore, legacyimport.Options{
		Root:   root,
		DryRun: true,
	})
	closeErr := mainOnlyStore.Close()
	if err != nil {
		t.Fatalf("plan from main-only database snapshot: %v", err)
	}
	if closeErr != nil {
		t.Fatalf("close main-only database snapshot: %v", closeErr)
	}
	if mainOnlyReport.ProjectsImported != 1 || mainOnlyReport.ProjectsSkipped != 0 {
		t.Fatalf("main-only snapshot projects: imported=%d skipped=%d, want 1/0 to prove project exists only in WAL",
			mainOnlyReport.ProjectsImported, mainOnlyReport.ProjectsSkipped)
	}

	rep := runImportDryRunJSON(t, root)
	if rep.ProjectsImported != 0 || rep.ProjectsSkipped != 1 {
		t.Fatalf("dry-run projects from WAL snapshot: imported=%d skipped=%d, want 0/1", rep.ProjectsImported, rep.ProjectsSkipped)
	}
	assertDirectoryStateUnchanged(t, cfg.dataDir, before)
	if _, err := os.Stat(filepath.Join(cfg.dataDir, "ao.db.lock")); !os.IsNotExist(err) {
		t.Fatalf("dry-run created source lock: err=%v", err)
	}
}

func TestExecuteImportDryRunJoinsRunAndSnapshotCleanupErrors(t *testing.T) {
	cfg := setConfigEnv(t)
	root := writeLegacyProject(t)
	store, err := sqlite.Open(cfg.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	cleanupErr := errors.New("injected snapshot cleanup failure")
	var leakedSnapshot string
	deps := Deps{}.withDefaults()
	deps.RemoveAll = func(path string) error {
		leakedSnapshot = path
		return cleanupErr
	}
	t.Cleanup(func() {
		if leakedSnapshot != "" {
			_ = os.RemoveAll(leakedSnapshot)
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = (&commandContext{deps: deps}).executeImport(ctx, config.Config{DataDir: cfg.dataDir}, legacyimport.Options{
		Root:   root,
		DryRun: true,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("executeImport error = %v, want joined context cancellation", err)
	}
	if !errors.Is(err, cleanupErr) {
		t.Fatalf("executeImport error = %v, want joined cleanup failure", err)
	}
	if leakedSnapshot == "" {
		t.Fatal("snapshot cleanup was not attempted")
	}
	assertPathWithin(t, cfg.dataDir, leakedSnapshot)
}

func storedAlphaProject() domain.ProjectRecord {
	return domain.ProjectRecord{
		ID:           "alpha",
		Path:         "/existing/alpha",
		DisplayName:  "Alpha",
		RegisteredAt: time.Unix(1_700_000_000, 0).UTC(),
	}
}

func runImportDryRunJSON(t *testing.T, root string) legacyimport.Report {
	t.Helper()
	out, _, err := executeCLI(t, Deps{}, "import", "--from", root, "--dry-run", "--yes", "--json")
	if err != nil {
		t.Fatalf("dry-run import: %v", err)
	}
	var rep legacyimport.Report
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("parse dry-run report %q: %v", out, err)
	}
	return rep
}

type sourceEntryState struct {
	Mode    os.FileMode
	Size    int64
	ModTime time.Time
	Bytes   []byte
}

type sourceDirectoryState struct {
	Exists bool
	Files  map[string]sourceEntryState
}

// captureDirectoryState records the exact relative inventory plus stable file
// metadata and bytes. In particular, it catches Windows-only SQLite behavior
// that creates an empty -wal/-shm sidecar merely by opening the source.
func captureDirectoryState(t *testing.T, root string) sourceDirectoryState {
	t.Helper()
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return sourceDirectoryState{}
	} else if err != nil {
		t.Fatalf("stat source directory: %v", err)
	}

	state := sourceDirectoryState{Exists: true, Files: make(map[string]sourceEntryState)}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		entry := sourceEntryState{Mode: info.Mode()}
		if rel == "." {
			entry.ModTime = info.ModTime()
		}
		if info.Mode().IsRegular() {
			entry.Size = info.Size()
			entry.ModTime = info.ModTime()
			entry.Bytes, err = os.ReadFile(path)
			if err != nil {
				return err
			}
		}
		state.Files[filepath.ToSlash(rel)] = entry
		return nil
	})
	if err != nil {
		t.Fatalf("capture source directory: %v", err)
	}
	return state
}

func assertDirectoryStateUnchanged(t *testing.T, root string, before sourceDirectoryState) {
	t.Helper()
	after := captureDirectoryState(t, root)
	var differences []string
	if before.Exists != after.Exists {
		differences = append(differences, fmt.Sprintf("exists: %v -> %v", before.Exists, after.Exists))
	}
	for path, want := range before.Files {
		got, ok := after.Files[path]
		if !ok {
			differences = append(differences, path+": removed")
			continue
		}
		if want.Mode != got.Mode || want.Size != got.Size || !want.ModTime.Equal(got.ModTime) || !bytes.Equal(want.Bytes, got.Bytes) {
			differences = append(differences, fmt.Sprintf(
				"%s: mode %v/%v size %d/%d modtime %s/%s bytesEqual=%v",
				path, want.Mode, got.Mode, want.Size, got.Size, want.ModTime, got.ModTime, bytes.Equal(want.Bytes, got.Bytes),
			))
		}
	}
	for path := range after.Files {
		if _, ok := before.Files[path]; !ok {
			differences = append(differences, path+": created")
		}
	}
	if len(differences) > 0 {
		t.Fatalf("source directory changed during dry-run:\n  %s", strings.Join(differences, "\n  "))
	}
}

func assertPathWithin(t *testing.T, root, path string) {
	t.Helper()
	rel, err := filepath.Rel(root, path)
	if err != nil {
		t.Fatalf("relate snapshot %q to data dir %q: %v", path, root, err)
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		t.Fatalf("snapshot %q is outside data dir %q", path, root)
	}
}
