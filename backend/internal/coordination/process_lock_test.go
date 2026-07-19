package coordination

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

func TestProcessLockContentionAndRelease(t *testing.T) {
	dataDir := t.TempDir()
	holder, err := acquireProcessLock(dataDir)
	if err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	if contender, err := acquireProcessLock(dataDir); !errors.Is(err, errProcessLocked) || contender != nil {
		t.Fatalf("contender lock=%+v err=%v, want nonblocking contention", contender, err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("contender blocked for %s", elapsed)
	}
	if err := holder.Close(); err != nil {
		t.Fatal(err)
	}

	replacement, err := acquireProcessLock(dataDir)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	if err := replacement.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, processLockFile)); err != nil {
		t.Fatalf("persistent lock file: %v", err)
	}
}

func TestProcessLockCrashHelper(t *testing.T) {
	dataDir := os.Getenv("AO_PROCESS_LOCK_HELPER_DIR")
	if dataDir == "" {
		return
	}
	lock, err := acquireProcessLock(dataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper lock: %v\n", err)
		os.Exit(2)
	}
	_ = lock // Intentionally leaked: the parent hard-kills this process.
	fmt.Println("LOCKED")
	time.Sleep(time.Hour)
}

func TestProcessLockReleasedWhenProcessCrashes(t *testing.T) {
	dataDir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProcessLockCrashHelper$")
	cmd.Env = append(os.Environ(), "AO_PROCESS_LOCK_HELPER_DIR="+dataDir)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() || scanner.Text() != "LOCKED" {
		t.Fatalf("helper readiness=%q err=%v", scanner.Text(), scanner.Err())
	}
	if contender, err := acquireProcessLock(dataDir); !errors.Is(err, errProcessLocked) || contender != nil {
		t.Fatalf("live-helper contender=%+v err=%v", contender, err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("hard-killed helper exited successfully")
	}

	replacement, err := acquireProcessLock(dataDir)
	if err != nil {
		t.Fatalf("acquire after helper crash: %v", err)
	}
	if err := replacement.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenExclusiveLocksBeforeDynamicNMinusOneMigration(t *testing.T) {
	dataDir := t.TempDir()
	store, err := sqlite.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "ao.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	_, sourceFile, _, _ := runtime.Caller(0)
	migrationsDir := filepath.Join(filepath.Dir(sourceFile), "..", "storage", "sqlite", "migrations")
	goose.SetBaseFS(os.DirFS(migrationsDir))
	currentVersion, err := goose.GetDBVersion(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := goose.Down(db, "."); err != nil {
		t.Fatalf("migrate dynamically to N-1: %v", err)
	}
	nMinusOne, err := goose.GetDBVersion(db)
	if err != nil {
		t.Fatal(err)
	}
	if nMinusOne >= currentVersion {
		t.Fatalf("down migration version=%d, want less than current %d", nMinusOne, currentVersion)
	}

	holder, err := acquireProcessLock(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if contenderStore, contenderLease, err := OpenExclusive(context.Background(), dataDir, os.Getpid()); !errors.Is(err, errProcessLocked) || contenderStore != nil || contenderLease != nil {
		t.Fatalf("contender store=%+v lease=%+v err=%v, want pre-migration contention", contenderStore, contenderLease, err)
	}
	stillNMinusOne, err := goose.GetDBVersion(db)
	if err != nil {
		t.Fatal(err)
	}
	if stillNMinusOne != nMinusOne {
		t.Fatalf("contender migrated locked DB to %d, want N-1 version %d", stillNMinusOne, nMinusOne)
	}
	if err := holder.Close(); err != nil {
		t.Fatal(err)
	}

	migratedStore, lease, err := OpenExclusive(context.Background(), dataDir, os.Getpid())
	if err != nil {
		t.Fatalf("open after bootstrap lock release: %v", err)
	}
	if err := lease.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := migratedStore.Close(); err != nil {
		t.Fatal(err)
	}
	migratedVersion, err := goose.GetDBVersion(db)
	if err != nil {
		t.Fatal(err)
	}
	if migratedVersion != currentVersion {
		t.Fatalf("migration version=%d, want restored current %d", migratedVersion, currentVersion)
	}
}

type releaseFailStore struct {
	store
	err error
}

func (s *releaseFailStore) ReleaseCoordinationClaim(ctx context.Context, key, ownerToken string) (bool, error) {
	if s.err != nil {
		return false, s.err
	}
	return s.store.ReleaseCoordinationClaim(ctx, key, ownerToken)
}

func TestLeaseReleasesDurableClaimBeforeOSLock(t *testing.T) {
	dataDir := t.TempDir()
	store, lease, err := OpenExclusive(context.Background(), dataDir, os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	releaseFailure := errors.New("durable release failed")
	hooked := &releaseFailStore{store: lease.store, err: releaseFailure}
	lease.store = hooked
	if err := lease.Release(context.Background()); !errors.Is(err, releaseFailure) {
		t.Fatalf("release error=%v, want durable failure", err)
	}
	if contender, err := acquireProcessLock(dataDir); !errors.Is(err, errProcessLocked) || contender != nil {
		t.Fatalf("OS lock released before durable claim: lock=%+v err=%v", contender, err)
	}

	hooked.err = nil
	if err := lease.Release(context.Background()); err != nil {
		t.Fatalf("retry release: %v", err)
	}
	replacement, err := acquireProcessLock(dataDir)
	if err != nil {
		t.Fatalf("OS lock held after durable release: %v", err)
	}
	if err := replacement.Close(); err != nil {
		t.Fatal(err)
	}
}

var _ store = (*releaseFailStore)(nil)
