package ptyregistry

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// withFakePidAlive replaces the pidAlive var for the duration of the test.
func withFakePidAlive(t *testing.T, fn func(pid int) bool) {
	t.Helper()
	orig := pidAlive
	pidAlive = fn
	t.Cleanup(func() { pidAlive = orig })
}

// setupHome points HOME at a temp dir and returns the expected registry path.
func setupHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("AO_DATA_DIR", "")
	t.Setenv("AO_RUN_FILE", "")
	return filepath.Join(dir, ".ao", "windows-pty-hosts.json")
}

func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339)
}

func TestRegistryUsesAODataDir(t *testing.T) {
	home := t.TempDir()
	dataDir := filepath.Join(t.TempDir(), "isolated", "data")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("AO_DATA_DIR", dataDir)
	t.Setenv("AO_RUN_FILE", filepath.Join(dataDir, "running.json"))
	withFakePidAlive(t, func(int) bool { return true })

	e := Entry{SessionID: "s1", PtyHostPID: 1234, PipePath: `\\.\pipe\ao-s1`, RegisteredAt: nowRFC3339()}
	if err := Register(e); err != nil {
		t.Fatal(err)
	}

	registryPath, _ := entryFileFor(dataDir, "s1")
	if _, err := os.Stat(registryPath); err != nil {
		t.Fatalf("registry not written under AO_DATA_DIR: %v", err)
	}
	legacyPath := filepath.Join(home, ".ao", "windows-pty-hosts.json")
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("registry unexpectedly escaped isolated data dir: %v", err)
	}

	got, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].SessionID != "s1" {
		t.Fatalf("expected isolated registry entry [s1], got %v", got)
	}
}

func TestListMigratesLegacyRegistryIntoAODataDir(t *testing.T) {
	home := t.TempDir()
	dataDir := filepath.Join(t.TempDir(), "isolated", "data")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("AO_DATA_DIR", dataDir)
	t.Setenv("AO_RUN_FILE", "")
	withFakePidAlive(t, func(int) bool { return true })

	legacyPath := filepath.Join(home, ".ao", "windows-pty-hosts.json")
	configuredPath, _ := entryFileFor(dataDir, "legacy")
	legacy := []Entry{
		{SessionID: "legacy", PtyHostPID: 111, PipePath: `\\.\pipe\ao-legacy`, RegisteredAt: nowRFC3339()},
	}
	writeRegistryFixture(t, legacyPath, legacy)

	got, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].SessionID != "legacy" || got[0].PtyHostPID != 111 {
		t.Fatalf("legacy entry was not adopted: %v", got)
	}
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("legacy registry was not removed after migration: %v", err)
	}

	data, err := os.ReadFile(configuredPath)
	if err != nil {
		t.Fatal(err)
	}
	var migrated Entry
	if err := json.Unmarshal(data, &migrated); err != nil {
		t.Fatal(err)
	}
	if migrated.SessionID != "legacy" {
		t.Fatalf("configured registry did not receive legacy entries: %v", migrated)
	}
}

func TestInitializedRegistryDoesNotImportLegacyEntries(t *testing.T) {
	home := t.TempDir()
	dataDir := filepath.Join(t.TempDir(), "initialized", "data")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("AO_DATA_DIR", dataDir)
	t.Setenv("AO_RUN_FILE", "")
	withFakePidAlive(t, func(int) bool { return true })

	legacyPath := filepath.Join(home, ".ao", "windows-pty-hosts.json")
	configuredPath := filepath.Join(dataDir, "windows-pty-hosts.json")
	writeRegistryFixture(t, legacyPath, []Entry{
		{SessionID: "default-store", PtyHostPID: 111, PipePath: `\\.\pipe\ao-default`, RegisteredAt: nowRFC3339()},
	})
	writeRegistryFixture(t, configuredPath, []Entry{
		{SessionID: "configured-store", PtyHostPID: 222, PipePath: `\\.\pipe\ao-configured`, RegisteredAt: nowRFC3339()},
	})

	got, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].SessionID != "configured-store" {
		t.Fatalf("initialized registry imported default entries: %v", got)
	}
	if _, err := os.Stat(legacyPath); err != nil {
		t.Fatalf("initialized registry removed default registry: %v", err)
	}
}

func TestShouldMigrateLegacyAcceptsDefaultRunFileCaseVariant(t *testing.T) {
	legacyPath := filepath.Join(t.TempDir(), ".ao", "windows-pty-hosts.json")
	configuredPath := filepath.Join(t.TempDir(), "data", "windows-pty-hosts.json")
	defaultRunFile := filepath.Join(filepath.Dir(legacyPath), "running.json")
	t.Setenv("AO_RUN_FILE", strings.ToUpper(defaultRunFile))

	if !shouldMigrateLegacy(configuredPath, legacyPath) {
		t.Fatal("default run-file case variant was treated as an isolated daemon")
	}
}

func TestIsolatedRegistryDoesNotImportLegacyEntries(t *testing.T) {
	home := t.TempDir()
	dataDir := filepath.Join(t.TempDir(), "isolated", "data")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("AO_DATA_DIR", dataDir)
	t.Setenv("AO_RUN_FILE", filepath.Join(t.TempDir(), "isolated", "running.json"))
	withFakePidAlive(t, func(int) bool { return true })

	legacyPath := filepath.Join(home, ".ao", "windows-pty-hosts.json")
	writeRegistryFixture(t, legacyPath, []Entry{
		{SessionID: "default-store", PtyHostPID: 111, PipePath: `\\.\pipe\ao-default`, RegisteredAt: nowRFC3339()},
	})

	got, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("isolated registry imported default entries: %v", got)
	}
	if _, err := os.Stat(legacyPath); err != nil {
		t.Fatalf("isolated registry removed default registry: %v", err)
	}
}

func TestListIgnoresLegacyCleanupFailure(t *testing.T) {
	home := t.TempDir()
	dataDir := filepath.Join(t.TempDir(), "migrated", "data")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("AO_DATA_DIR", dataDir)
	t.Setenv("AO_RUN_FILE", "")
	withFakePidAlive(t, func(int) bool { return true })

	legacyPath := filepath.Join(home, ".ao", "windows-pty-hosts.json")
	writeRegistryFixture(t, legacyPath, []Entry{
		{SessionID: "legacy", PtyHostPID: 111, PipePath: `\\.\pipe\ao-legacy`, RegisteredAt: nowRFC3339()},
	})
	originalRemove := removeLegacyFile
	removeLegacyFile = func(string) error { return os.ErrPermission }
	t.Cleanup(func() { removeLegacyFile = originalRemove })

	got, err := List()
	if err != nil {
		t.Fatalf("List returned cleanup error: %v", err)
	}
	if len(got) != 1 || got[0].SessionID != "legacy" {
		t.Fatalf("List lost readable legacy entries: %v", got)
	}
	configuredPath, _ := entryFileFor(dataDir, "legacy")
	if _, err := os.Stat(configuredPath); err != nil {
		t.Fatalf("configured registry was not written before cleanup: %v", err)
	}
}

func writeRegistryFixture(t *testing.T, path string, entries []Entry) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRegisterThenList(t *testing.T) {
	setupHome(t)
	withFakePidAlive(t, func(int) bool { return true })

	e := Entry{SessionID: "s1", PtyHostPID: 1234, PipePath: `\\.\pipe\ao-s1`, RegisteredAt: nowRFC3339()}
	if err := Register(e); err != nil {
		t.Fatal(err)
	}

	got, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].SessionID != "s1" {
		t.Fatalf("expected [s1], got %v", got)
	}
}

func TestRegisterReplaceSameID(t *testing.T) {
	setupHome(t)
	withFakePidAlive(t, func(int) bool { return true })

	e1 := Entry{SessionID: "s1", PtyHostPID: 111, PipePath: `\\.\pipe\ao-s1-a`, RegisteredAt: nowRFC3339()}
	e2 := Entry{SessionID: "s1", PtyHostPID: 222, PipePath: `\\.\pipe\ao-s1-b`, RegisteredAt: nowRFC3339()}
	if err := Register(e1); err != nil {
		t.Fatal(err)
	}
	if err := Register(e2); err != nil {
		t.Fatal(err)
	}

	got, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(got))
	}
	if got[0].PtyHostPID != 222 {
		t.Fatalf("expected PID 222, got %d", got[0].PtyHostPID)
	}
}

func TestConcurrentRegistrationsPreserveEverySession(t *testing.T) {
	setupHome(t)
	withFakePidAlive(t, func(int) bool { return true })
	const count = 64
	var wg sync.WaitGroup
	errs := make(chan error, count)
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- Register(Entry{SessionID: fmt.Sprintf("s-%02d", i), PtyHostPID: 1000 + i, PipePath: fmt.Sprintf("127.0.0.1:%d", 2000+i), RegisteredAt: nowRFC3339()})
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != count {
		t.Fatalf("concurrent registrations lost entries: got=%d want=%d entries=%v", len(got), count, got)
	}
}

func TestUnregisterRemoves(t *testing.T) {
	setupHome(t)
	withFakePidAlive(t, func(int) bool { return true })

	e := Entry{SessionID: "s1", PtyHostPID: 1234, PipePath: `\\.\pipe\ao-s1`, RegisteredAt: nowRFC3339()}
	if err := Register(e); err != nil {
		t.Fatal(err)
	}
	if err := Unregister("s1"); err != nil {
		t.Fatal(err)
	}
	got, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty, got %v", got)
	}
}

func TestUnregisterNoOpWhenAbsent(t *testing.T) {
	setupHome(t)
	withFakePidAlive(t, func(int) bool { return true })

	if err := Unregister("nonexistent"); err != nil {
		t.Fatal(err)
	}
}

func TestListPrunesDeadPIDs(t *testing.T) {
	setupHome(t)

	// PID 1 alive, PID 2 dead.
	alive := map[int]bool{1: true, 2: false}
	withFakePidAlive(t, func(pid int) bool { return alive[pid] })

	e1 := Entry{SessionID: "s1", PtyHostPID: 1, PipePath: `\\.\pipe\ao-s1`, RegisteredAt: nowRFC3339()}
	e2 := Entry{SessionID: "s2", PtyHostPID: 2, PipePath: `\\.\pipe\ao-s2`, RegisteredAt: nowRFC3339()}
	if err := Register(e1); err != nil {
		t.Fatal(err)
	}
	if err := Register(e2); err != nil {
		t.Fatal(err)
	}

	got, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].SessionID != "s1" {
		t.Fatalf("expected [s1], got %v", got)
	}

	// Verify the on-disk file was rewritten with only the live entry.
	entryPath, _ := entryFileFor("", "s1")
	data, err := os.ReadFile(entryPath)
	if err != nil {
		t.Fatal(err)
	}
	var disk Entry
	if err := json.Unmarshal(data, &disk); err != nil {
		t.Fatal(err)
	}
	if disk.SessionID != "s1" {
		t.Fatalf("disk should have only s1, got %v", disk)
	}
}

func TestEmptyResultDeletesFile(t *testing.T) {
	setupHome(t)
	withFakePidAlive(t, func(int) bool { return true })

	e := Entry{SessionID: "s1", PtyHostPID: 1, PipePath: `\\.\pipe\ao-s1`, RegisteredAt: nowRFC3339()}
	if err := Register(e); err != nil {
		t.Fatal(err)
	}
	regPath, _ := entryFileFor("", "s1")
	// Unregister last entry -> file should be deleted.
	if err := Unregister("s1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(regPath); !os.IsNotExist(err) {
		t.Fatal("expected registry file to be deleted")
	}
}

func TestClearDeletesFile(t *testing.T) {
	setupHome(t)
	withFakePidAlive(t, func(int) bool { return true })

	e := Entry{SessionID: "s1", PtyHostPID: 1, PipePath: `\\.\pipe\ao-s1`, RegisteredAt: nowRFC3339()}
	if err := Register(e); err != nil {
		t.Fatal(err)
	}
	regPath, _ := entryFileFor("", "s1")
	if err := Clear(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(regPath); !os.IsNotExist(err) {
		t.Fatal("expected registry file to be deleted after Clear")
	}
}

func TestConfiguredAggregateFailuresRetainFence(t *testing.T) {
	withFakePidAlive(t, func(int) bool { return true })
	for _, tc := range []struct {
		name      string
		contents  []byte
		readError error
	}{
		{name: "unreadable", contents: []byte(`[{"sessionId":"live","ptyHostPid":123,"pipePath":"127.0.0.1:1"}]`), readError: os.ErrPermission},
		{name: "malformed", contents: []byte("not json {{{")},
		{name: "missing pipe", contents: []byte(`[{"sessionId":"live","ptyHostPid":123,"pipePath":""}]`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := t.TempDir()
			path, err := registryFileFor(dataDir)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, tc.contents, 0o600); err != nil {
				t.Fatal(err)
			}
			originalRead := readAggregateData
			if tc.readError != nil {
				readAggregateData = func(candidate string) ([]byte, error) {
					if sameRegistryPath(candidate, path) {
						return nil, tc.readError
					}
					return originalRead(candidate)
				}
			}
			t.Cleanup(func() { readAggregateData = originalRead })

			if _, err := ListAt(dataDir); err == nil {
				t.Fatal("configured aggregate failure produced an authoritative empty list")
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("configured aggregate was removed after list failure: %v", err)
			}
			if _, err := LookupAllAt(dataDir, "live"); err == nil {
				t.Fatal("configured aggregate failure produced an authoritative keyed miss")
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("configured aggregate was removed after keyed lookup failure: %v", err)
			}
		})
	}
}

func TestListRetainsUnreadableOrMalformedLiveEntry(t *testing.T) {
	setupHome(t)
	withFakePidAlive(t, func(int) bool { return true })
	entry := Entry{SessionID: "live", PtyHostPID: 123, PipePath: "127.0.0.1:1234", RegisteredAt: nowRFC3339()}
	if err := Register(entry); err != nil {
		t.Fatal(err)
	}
	path, _ := entryFileFor("", entry.SessionID)

	originalRead := readEntryData
	readEntryData = func(string) ([]byte, error) { return nil, os.ErrPermission }
	if _, err := List(); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("unreadable live entry error = %v, want permission error", err)
	}
	readEntryData = originalRead
	t.Cleanup(func() { readEntryData = originalRead })
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("unreadable live entry was deleted: %v", err)
	}

	if err := os.WriteFile(path, []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := List(); err == nil {
		t.Fatal("malformed live entry produced a dead conclusion")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("malformed live entry was deleted: %v", err)
	}
}

func TestMissingFileReturnsEmpty(t *testing.T) {
	setupHome(t)
	withFakePidAlive(t, func(int) bool { return true })

	got, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty for missing file, got %v", got)
	}
}

func TestAtomicWriteProducesValidJSON(t *testing.T) {
	setupHome(t)
	withFakePidAlive(t, func(int) bool { return true })

	e := Entry{SessionID: "s1", PtyHostPID: 99, PipePath: `\\.\pipe\ao-s1`, RegisteredAt: nowRFC3339()}
	if err := Register(e); err != nil {
		t.Fatal(err)
	}

	entryPath, _ := entryFileFor("", "s1")
	data, err := os.ReadFile(entryPath)
	if err != nil {
		t.Fatal(err)
	}
	var entries Entry
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatalf("registry file is not valid JSON: %v", err)
	}
	if entries.PtyHostPID != 99 {
		t.Fatalf("unexpected entries: %v", entries)
	}
}

func TestListDeadGenerationCannotDeleteConcurrentRepublish(t *testing.T) {
	dataDir := t.TempDir()
	old := Entry{SessionID: "same", PtyHostPID: 10, PipePath: "127.0.0.1:10", RegisteredAt: "2026-01-01T00:00:00.100Z", Generation: "old"}
	newer := Entry{SessionID: "same", PtyHostPID: 20, PipePath: "127.0.0.1:20", RegisteredAt: "2026-01-01T00:00:00.200Z", Generation: "new"}
	withFakePidAlive(t, func(pid int) bool { return pid == newer.PtyHostPID })
	if err := RegisterAt(dataDir, old); err != nil {
		t.Fatal(err)
	}
	oldPath, _ := entryPathFor(dataDir, old)
	readStarted := make(chan struct{})
	allowRead := make(chan struct{})
	originalRead := readEntryData
	readEntryData = func(path string) ([]byte, error) {
		data, err := originalRead(path)
		if path == oldPath {
			close(readStarted)
			<-allowRead
		}
		return data, err
	}
	t.Cleanup(func() { readEntryData = originalRead })
	done := make(chan error, 1)
	go func() { _, err := ListAt(dataDir); done <- err }()
	<-readStarted
	if err := RegisterAt(dataDir, newer); err != nil {
		t.Fatal(err)
	}
	close(allowRead)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got, ok, err := LookupAt(dataDir, "same")
	if err != nil || !ok || got.Generation != "new" {
		t.Fatalf("concurrent republish lost: got=%+v ok=%v err=%v", got, ok, err)
	}
}

func TestUnregisterSnapshotCannotDeleteConcurrentRepublish(t *testing.T) {
	dataDir := t.TempDir()
	withFakePidAlive(t, func(int) bool { return true })
	old := Entry{SessionID: "same", PtyHostPID: 10, PipePath: "127.0.0.1:10", RegisteredAt: "2026-01-01T00:00:00.100Z", Generation: "old"}
	newer := Entry{SessionID: "same", PtyHostPID: 20, PipePath: "127.0.0.1:20", RegisteredAt: "2026-01-01T00:00:00.200Z", Generation: "new"}
	if err := RegisterAt(dataDir, old); err != nil {
		t.Fatal(err)
	}
	oldPath, _ := entryPathFor(dataDir, old)
	removeStarted := make(chan struct{})
	allowRemove := make(chan struct{})
	originalRemove := removeEntryFile
	removeEntryFile = func(path string) error {
		if path == oldPath {
			close(removeStarted)
			<-allowRemove
		}
		return originalRemove(path)
	}
	t.Cleanup(func() { removeEntryFile = originalRemove })
	done := make(chan error, 1)
	go func() { done <- UnregisterAt(dataDir, "same") }()
	<-removeStarted
	if err := RegisterAt(dataDir, newer); err != nil {
		t.Fatal(err)
	}
	close(allowRemove)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got, ok, err := LookupAt(dataDir, "same")
	if err != nil || !ok || got.Generation != "new" {
		t.Fatalf("concurrent unregister removed replacement: got=%+v ok=%v err=%v", got, ok, err)
	}
}

func TestLookupAtIsolatesOtherSessionCorruption(t *testing.T) {
	dataDir := t.TempDir()
	withFakePidAlive(t, func(int) bool { return true })
	valid := Entry{SessionID: "session-b", PtyHostPID: 20, PipePath: "127.0.0.1:20", RegisteredAt: nowRFC3339(), Generation: "b"}
	if err := RegisterAt(dataDir, valid); err != nil {
		t.Fatal(err)
	}
	corruptPath, _ := generationEntryFileFor(dataDir, "session-a", "a")
	if err := os.MkdirAll(filepath.Dir(corruptPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(corruptPath, []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, ok, err := LookupAt(dataDir, "session-b")
	if err != nil || !ok || got.Generation != "b" {
		t.Fatalf("corrupt session A wedged B: got=%+v ok=%v err=%v", got, ok, err)
	}
}

func TestKeyedEntryIsAuthoritativeOverCorruptAggregate(t *testing.T) {
	dataDir := t.TempDir()
	withFakePidAlive(t, func(int) bool { return true })
	valid := Entry{SessionID: "session-b", PtyHostPID: 20, PipePath: "127.0.0.1:20", RegisteredAt: nowRFC3339(), Generation: "b"}
	if err := RegisterAt(dataDir, valid); err != nil {
		t.Fatal(err)
	}
	aggregatePath, err := registryFileFor(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(aggregatePath, []byte("partial-[{"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, ok, err := LookupAt(dataDir, "session-b")
	if err != nil || !ok || got.Generation != valid.Generation {
		t.Fatalf("valid keyed host was wedged by obsolete aggregate: got=%+v ok=%v err=%v", got, ok, err)
	}
	if _, err := LookupAllAt(dataDir, "legacy-only"); err == nil {
		t.Fatal("legacy-only lookup silently treated corrupt aggregate as empty")
	}
	if _, err := os.Stat(aggregatePath); err != nil {
		t.Fatalf("corrupt aggregate was removed after keyed fallback failure: %v", err)
	}
}

func TestLookupNewestGenerationUsesNanosecondTimestamp(t *testing.T) {
	dataDir := t.TempDir()
	withFakePidAlive(t, func(int) bool { return true })
	older := Entry{SessionID: "rapid", PtyHostPID: 10, PipePath: "127.0.0.1:10", RegisteredAt: "2026-01-01T00:00:00.100000000Z", Generation: "zzzz"}
	newer := Entry{SessionID: "rapid", PtyHostPID: 20, PipePath: "127.0.0.1:20", RegisteredAt: "2026-01-01T00:00:00.200000000Z", Generation: "aaaa"}
	if err := RegisterAt(dataDir, older); err != nil {
		t.Fatal(err)
	}
	if err := RegisterAt(dataDir, newer); err != nil {
		t.Fatal(err)
	}
	got, ok, err := LookupAt(dataDir, "rapid")
	if err != nil || !ok || got.Generation != newer.Generation {
		t.Fatalf("rapid generation selection=%+v ok=%v err=%v", got, ok, err)
	}
}
