// Package ptyregistry is a sideband JSON list of live Windows pty-host
// processes so ao stop can find and graceful-kill them even when session
// metadata is lost. Ported from agent-orchestrator's windows-pty-registry.ts.
package ptyregistry

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// Entry is one registered pty-host process.
type Entry struct {
	SessionID    string `json:"sessionId"`
	PtyHostPID   int    `json:"ptyHostPid"`
	PipePath     string `json:"pipePath"`
	RegisteredAt string `json:"registeredAt"` // RFC3339; set by caller
}

// pidAlive is the PID-liveness probe. Tests replace it with a fake.
// defaultPidAlive is provided in build-tagged files (pidalive_unix.go /
// pidalive_windows.go).
var pidAlive = defaultPidAlive

// removeLegacyFile is replaceable in tests so cleanup failures can be covered
// without depending on platform-specific file-lock semantics.
var removeLegacyFile = os.Remove

// registryFile resolves windows-pty-hosts.json under AO_DATA_DIR when set so
// isolated daemon stores also have isolated crash-recovery registries. The
// default remains ~/.ao/windows-pty-hosts.json for compatibility with existing
// installs. Uses os.UserHomeDir() so tests can redirect the default safely.
func registryFile() (string, error) {
	if dataDir, ok := os.LookupEnv("AO_DATA_DIR"); ok && dataDir != "" {
		return filepath.Join(dataDir, "windows-pty-hosts.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".ao", "windows-pty-hosts.json"), nil
}

func legacyRegistryFile() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".ao", "windows-pty-hosts.json"), nil
}

// shouldMigrateLegacy distinguishes moving the default daemon's data store
// from running a second isolated daemon. A custom AO_RUN_FILE identifies a
// separate daemon namespace, whose registry must never read or remove entries
// owned by the default ~/.ao/running.json instance.
func shouldMigrateLegacy(configuredPath, legacyPath string) bool {
	if filepath.Clean(configuredPath) == filepath.Clean(legacyPath) {
		return false
	}
	runFile, customRunFile := os.LookupEnv("AO_RUN_FILE")
	if !customRunFile || runFile == "" {
		return true
	}
	defaultRunFile := filepath.Join(filepath.Dir(legacyPath), "running.json")
	return filepath.Clean(runFile) == filepath.Clean(defaultRunFile)
}

// readRaw reads and defensively parses the registry. Missing file or malformed
// JSON both return an empty slice (mirrors readRaw in the TS source).
func readRawFile(path string) []Entry {
	data, err := os.ReadFile(path)
	if err != nil {
		// Missing file is fine.
		return nil
	}
	var parsed []json.RawMessage
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil
	}
	out := make([]Entry, 0, len(parsed))
	for _, raw := range parsed {
		var e Entry
		if err := json.Unmarshal(raw, &e); err != nil {
			continue
		}
		// Drop entries missing required fields (mirrors TS filter).
		if e.SessionID == "" || e.PtyHostPID == 0 || e.PipePath == "" {
			continue
		}
		out = append(out, e)
	}
	return out
}

// readRaw also reads the pre-AO_DATA_DIR registry while upgrading an existing
// install. Entries in the configured registry win on duplicate session IDs.
// The caller removes the legacy file only after the merged list is safely
// written to the configured location.
func readRaw() (entries []Entry, legacyPath string, migrateLegacy bool) {
	path, err := registryFile()
	if err != nil {
		return nil, "", false
	}
	entries = readRawFile(path)
	// A configured registry means this store has already initialized under the
	// new layout. Legacy import is a one-time upgrade path, not an ongoing merge
	// between independently scoped stores.
	if _, err := os.Stat(path); err == nil || !errors.Is(err, os.ErrNotExist) {
		return entries, "", false
	}

	legacyPath, err = legacyRegistryFile()
	if err != nil || !shouldMigrateLegacy(path, legacyPath) {
		return entries, "", false
	}
	legacy := readRawFile(legacyPath)
	if len(legacy) == 0 {
		return entries, "", false
	}

	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		seen[entry.SessionID] = struct{}{}
	}
	for _, entry := range legacy {
		if _, ok := seen[entry.SessionID]; ok {
			continue
		}
		seen[entry.SessionID] = struct{}{}
		entries = append(entries, entry)
	}
	return entries, legacyPath, true
}

// writeRaw atomically writes entries to the registry file. When entries is
// empty it deletes the file instead (mirrors writeRaw in the TS source).
func writeRaw(entries []Entry) error {
	path, err := registryFile()
	if err != nil {
		return err
	}

	if len(entries) == 0 {
		err := os.Remove(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}

	// Atomic write: temp file in same dir then rename (same filesystem).
	tmp, err := os.CreateTemp(dir, "pty-hosts-*.json.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		// Best-effort cleanup of temp file on failure.
		_ = os.Remove(tmpName)
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func writeMigrated(entries []Entry, legacyPath string, migrateLegacy bool) error {
	if err := writeRaw(entries); err != nil {
		return err
	}
	if !migrateLegacy {
		return nil
	}
	// The configured registry is now authoritative. Cleanup is deliberately
	// best-effort: callers must retain the readable entries even if Windows has
	// the old file locked or its ACL prevents removal.
	_ = removeLegacyFile(legacyPath)
	return nil
}

// Register adds or replaces the entry for entry.SessionID. registeredAt must
// be set by the caller (e.g. time.Now().UTC().Format(time.RFC3339)).
func Register(entry Entry) error {
	all, legacyPath, migrateLegacy := readRaw()
	next := make([]Entry, 0)
	for _, e := range all {
		if e.SessionID != entry.SessionID {
			next = append(next, e)
		}
	}
	next = append(next, entry)
	return writeMigrated(next, legacyPath, migrateLegacy)
}

// Unregister removes the entry for sessionID. No-op if absent.
func Unregister(sessionID string) error {
	all, legacyPath, migrateLegacy := readRaw()
	next := make([]Entry, 0, len(all))
	for _, e := range all {
		if e.SessionID != sessionID {
			next = append(next, e)
		}
	}
	if len(next) == len(all) && !migrateLegacy {
		return nil // absent, no-op
	}
	return writeMigrated(next, legacyPath, migrateLegacy)
}

// List returns all entries whose PtyHostPID is still alive, auto-pruning dead
// ones. The file is rewritten if any entries were pruned.
func List() ([]Entry, error) {
	all, legacyPath, migrateLegacy := readRaw()
	live := make([]Entry, 0, len(all))
	for _, e := range all {
		if pidAlive(e.PtyHostPID) {
			live = append(live, e)
		}
	}
	if len(live) != len(all) || migrateLegacy {
		if err := writeMigrated(live, legacyPath, migrateLegacy); err != nil {
			return live, err
		}
	}
	return live, nil
}

// Clear deletes the registry file. Best-effort; used by tests and recovery.
func Clear() error {
	if err := writeRaw(nil); err != nil {
		return err
	}
	configured, err := registryFile()
	if err != nil {
		return err
	}
	legacy, err := legacyRegistryFile()
	if err != nil {
		return err
	}
	if !shouldMigrateLegacy(configured, legacy) {
		return nil
	}
	if err := os.Remove(legacy); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
