package ptyregistry

import (
	"encoding/json"
	"os"
	"path/filepath"
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

	registryPath := filepath.Join(dataDir, "windows-pty-hosts.json")
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
	configuredPath := filepath.Join(dataDir, "windows-pty-hosts.json")
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
	var migrated []Entry
	if err := json.Unmarshal(data, &migrated); err != nil {
		t.Fatal(err)
	}
	if len(migrated) != 1 || migrated[0].SessionID != "legacy" {
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
	if _, err := os.Stat(filepath.Join(dataDir, "windows-pty-hosts.json")); err != nil {
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
	regPath := setupHome(t)

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
	data, err := os.ReadFile(regPath)
	if err != nil {
		t.Fatal(err)
	}
	var disk []Entry
	if err := json.Unmarshal(data, &disk); err != nil {
		t.Fatal(err)
	}
	if len(disk) != 1 || disk[0].SessionID != "s1" {
		t.Fatalf("disk should have only s1, got %v", disk)
	}
}

func TestEmptyResultDeletesFile(t *testing.T) {
	regPath := setupHome(t)
	withFakePidAlive(t, func(int) bool { return true })

	e := Entry{SessionID: "s1", PtyHostPID: 1, PipePath: `\\.\pipe\ao-s1`, RegisteredAt: nowRFC3339()}
	if err := Register(e); err != nil {
		t.Fatal(err)
	}
	// Unregister last entry -> file should be deleted.
	if err := Unregister("s1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(regPath); !os.IsNotExist(err) {
		t.Fatal("expected registry file to be deleted")
	}
}

func TestClearDeletesFile(t *testing.T) {
	regPath := setupHome(t)
	withFakePidAlive(t, func(int) bool { return true })

	e := Entry{SessionID: "s1", PtyHostPID: 1, PipePath: `\\.\pipe\ao-s1`, RegisteredAt: nowRFC3339()}
	if err := Register(e); err != nil {
		t.Fatal(err)
	}
	if err := Clear(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(regPath); !os.IsNotExist(err) {
		t.Fatal("expected registry file to be deleted after Clear")
	}
}

func TestMalformedJSONReturnsEmpty(t *testing.T) {
	setupHome(t)
	withFakePidAlive(t, func(int) bool { return true })

	// Write malformed JSON directly.
	path, _ := registryFile()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not json {{{"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty on malformed JSON, got %v", got)
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
	regPath := setupHome(t)
	withFakePidAlive(t, func(int) bool { return true })

	e := Entry{SessionID: "s1", PtyHostPID: 99, PipePath: `\\.\pipe\ao-s1`, RegisteredAt: nowRFC3339()}
	if err := Register(e); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(regPath)
	if err != nil {
		t.Fatal(err)
	}
	var entries []Entry
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatalf("registry file is not valid JSON: %v", err)
	}
	if len(entries) != 1 || entries[0].PtyHostPID != 99 {
		t.Fatalf("unexpected entries: %v", entries)
	}
}
