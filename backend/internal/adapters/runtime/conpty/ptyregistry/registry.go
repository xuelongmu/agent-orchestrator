// Package ptyregistry is a sideband JSON list of live Windows pty-host
// processes so ao stop can find and graceful-kill them even when session
// metadata is lost. Ported from agent-orchestrator's windows-pty-registry.ts.
package ptyregistry

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Entry is one registered pty-host process.
type Entry struct {
	SessionID    string `json:"sessionId"`
	PtyHostPID   int    `json:"ptyHostPid"`
	PipePath     string `json:"pipePath"`
	RegisteredAt string `json:"registeredAt"` // RFC3339; set by caller
	Generation   string `json:"generation,omitempty"`
}

// pidAlive is the PID-liveness probe. Tests replace it with a fake.
// defaultPidAlive is provided in build-tagged files (pidalive_unix.go /
// pidalive_windows.go).
var pidAlive = defaultPidAlive

// removeLegacyFile is replaceable in tests so cleanup failures can be covered
// without depending on platform-specific file-lock semantics.
var removeLegacyFile = os.Remove

// readEntryData is replaceable in tests to model transient Windows sharing or
// antivirus access failures without changing filesystem ACLs process-wide.
var readEntryData = os.ReadFile

// readAggregateData is replaceable in tests to model a configured legacy
// aggregate that temporarily cannot be read. Existing aggregate errors are
// never proof that no detached hosts exist.
var readAggregateData = os.ReadFile

// removeEntryFile is replaceable in tests to deterministically interleave a
// same-session republish with pruning/unregister. Generation-specific paths
// ensure removing an observed generation can never remove its replacement.
var removeEntryFile = os.Remove

// registryFile resolves windows-pty-hosts.json under AO_DATA_DIR when set so
// isolated daemon stores also have isolated crash-recovery registries. The
// default remains ~/.ao/windows-pty-hosts.json for compatibility with existing
// installs. Uses os.UserHomeDir() so tests can redirect the default safely.
func registryFile() (string, error) {
	if dataDir, ok := os.LookupEnv("AO_DATA_DIR"); ok && dataDir != "" {
		return registryFileFor(dataDir)
	}
	return registryFileFor("")
}

func registryFileFor(dataDir string) (string, error) {
	if dataDir != "" {
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

// sameRegistryPath follows Windows path identity. The ConPTY registry is a
// Windows-only sideband, so casing differences must not split one daemon
// namespace into two logical stores.
func sameRegistryPath(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

// shouldMigrateLegacy distinguishes moving the default daemon's data store
// from running a second isolated daemon. A custom AO_RUN_FILE identifies a
// separate daemon namespace, whose registry must never read or remove entries
// owned by the default ~/.ao/running.json instance.
func shouldMigrateLegacy(configuredPath, legacyPath string) bool {
	if sameRegistryPath(configuredPath, legacyPath) {
		return false
	}
	runFile, customRunFile := os.LookupEnv("AO_RUN_FILE")
	if !customRunFile || runFile == "" {
		return true
	}
	defaultRunFile := filepath.Join(filepath.Dir(legacyPath), "running.json")
	return sameRegistryPath(runFile, defaultRunFile)
}

// readRawFile reads and defensively parses an aggregate registry. A missing
// file is empty; an existing unreadable or malformed file is a transient probe
// failure so callers retain it rather than concluding every host is dead.
func readRawFile(path string) ([]Entry, error) {
	data, err := readAggregateData(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("ptyregistry: read aggregate %q: %w", path, err)
	}
	var parsed []json.RawMessage
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("ptyregistry: parse aggregate %q: %w", path, err)
	}
	out := make([]Entry, 0, len(parsed))
	for index, raw := range parsed {
		var e Entry
		if err := json.Unmarshal(raw, &e); err != nil {
			return nil, fmt.Errorf("ptyregistry: parse aggregate entry %q: %w", path, err)
		}
		// A syntactically valid but partial entry is still an uncertain live
		// host, not proof of absence. Fail closed and retain the aggregate.
		if e.SessionID == "" || e.PtyHostPID == 0 || e.PipePath == "" {
			return nil, fmt.Errorf("ptyregistry: parse aggregate entry %q at index %d: session id, host pid, and pipe path are required", path, index)
		}
		out = append(out, e)
	}
	return out, nil
}

func readRawFor(dataDir string) (entries []Entry, legacyPath string, migrateLegacy bool, err error) {
	path, err := registryFileFor(dataDir)
	if err != nil {
		return nil, "", false, err
	}
	entries, err = readRawFile(path)
	if err != nil {
		return nil, "", false, err
	}
	// A configured registry means this store has already initialized under the
	// new layout. Legacy import is a one-time upgrade path, not an ongoing merge
	// between independently scoped stores.
	if _, err := os.Stat(path); err == nil || !errors.Is(err, os.ErrNotExist) {
		return entries, "", false, nil
	}

	legacyPath, err = legacyRegistryFile()
	if err != nil || !shouldMigrateLegacy(path, legacyPath) {
		return entries, "", false, err
	}
	legacy, err := readRawFile(legacyPath)
	if err != nil {
		return nil, "", false, err
	}
	if len(legacy) == 0 {
		return entries, "", false, nil
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
	return entries, legacyPath, true, nil
}

// writeRaw atomically writes entries to the registry file. When entries is
// empty it deletes the file instead (mirrors writeRaw in the TS source).
func writeRaw(entries []Entry) error {
	dataDir, _ := os.LookupEnv("AO_DATA_DIR")
	return writeRawFor(dataDir, entries)
}

func writeRawFor(dataDir string, entries []Entry) error {
	path, err := registryFileFor(dataDir)
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

// Register adds or replaces the entry for entry.SessionID. registeredAt must
// be set by the caller (e.g. time.Now().UTC().Format(time.RFC3339)).
func Register(entry Entry) error {
	dataDir, _ := os.LookupEnv("AO_DATA_DIR")
	return RegisterAt(dataDir, entry)
}

// RegisterAt publishes an entry in an explicit daemon data namespace. It is
// used by the parent runtime so discovery never depends on ambient process env.
func RegisterAt(dataDir string, entry Entry) error {
	if entry.SessionID == "" || entry.PtyHostPID == 0 || entry.PipePath == "" {
		return errors.New("ptyregistry: session id, host pid, and pipe path are required")
	}
	path, err := entryPathFor(dataDir, entry)
	if err != nil {
		return err
	}
	return writeEntryFile(path, entry)
}

// Unregister removes the entry for sessionID. No-op if absent.
func Unregister(sessionID string) error {
	dataDir, _ := os.LookupEnv("AO_DATA_DIR")
	return UnregisterAt(dataDir, sessionID)
}

// UnregisterAt removes an entry from an explicit daemon data namespace.
func UnregisterAt(dataDir, sessionID string) error {
	// Migrate a legacy aggregate first so removing the per-session file cannot
	// be undone by a later List importing the old entry again.
	if _, err := ListAt(dataDir); err != nil {
		return err
	}
	paths, err := entryPathsFor(dataDir, sessionID)
	if err != nil {
		return err
	}
	for _, path := range paths {
		if err := removeEntryFile(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// UnregisterGenerationAt removes only the exact host generation owned by a
// runtime. A replacement with the same session id has a distinct path and is
// therefore immune to stale Destroy or cleanup calls.
func UnregisterGenerationAt(dataDir, sessionID, generation string) error {
	path, err := entryPathFor(dataDir, Entry{SessionID: sessionID, Generation: generation})
	if err != nil {
		return err
	}
	if err := removeEntryFile(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// List returns all entries whose PtyHostPID is still alive, auto-pruning dead
// ones. The file is rewritten if any entries were pruned.
func List() ([]Entry, error) {
	dataDir, _ := os.LookupEnv("AO_DATA_DIR")
	return ListAt(dataDir)
}

// ListAt reads live entries from an explicit daemon data namespace.
func ListAt(dataDir string) ([]Entry, error) {
	dir, err := registryDirFor(dataDir)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]Entry)
	entries, readErr := os.ReadDir(dir)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return nil, readErr
	}
	for _, file := range entries {
		if file.IsDir() || filepath.Ext(file.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, file.Name())
		entry, err := readEntryFile(path)
		if err != nil {
			return nil, err
		}
		if !pidAlive(entry.PtyHostPID) {
			_ = removeEntryFile(path)
			continue
		}
		if current, exists := byID[entry.SessionID]; !exists || newerEntry(entry, current) {
			byID[entry.SessionID] = entry
		}
	}

	// One-time migration from the old aggregate registry. Per-session entries
	// win, and every migrated entry is published independently before either
	// aggregate file is removed.
	legacy, legacyPath, migrateLegacy, err := readRawFor(dataDir)
	if err != nil {
		return nil, err
	}
	for _, entry := range legacy {
		if _, exists := byID[entry.SessionID]; exists || !pidAlive(entry.PtyHostPID) {
			continue
		}
		if err := RegisterAt(dataDir, entry); err != nil {
			return nil, err
		}
		byID[entry.SessionID] = entry
	}
	configured, err := registryFileFor(dataDir)
	if err != nil {
		return nil, err
	}
	if err := os.Remove(configured); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if migrateLegacy {
		_ = removeLegacyFile(legacyPath)
	}

	live := make([]Entry, 0, len(byID))
	for _, entry := range byID {
		live = append(live, entry)
	}
	sort.Slice(live, func(i, j int) bool { return live[i].SessionID < live[j].SessionID })
	return live, nil
}

// Lookup resolves one session without reading unrelated entry files. A
// malformed or temporarily unreadable entry for session A must not prevent
// liveness recovery for session B.
func Lookup(sessionID string) (Entry, bool, error) {
	dataDir, _ := os.LookupEnv("AO_DATA_DIR")
	return LookupAt(dataDir, sessionID)
}

// LookupAt resolves one session in an explicit daemon data namespace.
func LookupAt(dataDir, sessionID string) (Entry, bool, error) {
	entries, err := LookupAllAt(dataDir, sessionID)
	if err != nil {
		return Entry{}, false, err
	}
	if len(entries) == 0 {
		return Entry{}, false, nil
	}
	return entries[0], true, nil
}

// LookupAll returns every live generation for one session, newest first.
func LookupAll(sessionID string) ([]Entry, error) {
	dataDir, _ := os.LookupEnv("AO_DATA_DIR")
	return LookupAllAt(dataDir, sessionID)
}

// LookupAllAt returns a bounded snapshot of one session's live generations.
// It never opens entry files belonging to another session.
func LookupAllAt(dataDir, sessionID string) ([]Entry, error) {
	paths, err := entryPathsFor(dataDir, sessionID)
	if err != nil {
		return nil, err
	}
	selected := make([]Entry, 0, len(paths)+1)
	for _, path := range paths {
		entry, err := readEntryFile(path)
		if err != nil {
			return nil, err
		}
		if entry.SessionID != sessionID {
			return nil, fmt.Errorf("ptyregistry: entry %q belongs to session %q, want %q", path, entry.SessionID, sessionID)
		}
		if !pidAlive(entry.PtyHostPID) {
			_ = removeEntryFile(path)
			continue
		}
		selected = append(selected, entry)
	}
	if len(selected) > 0 {
		// Per-generation files are the authoritative current layout. An
		// obsolete aggregate may be corrupt after an interrupted upgrade, but
		// it must not wedge a valid keyed host that is already discoverable.
		sort.Slice(selected, func(i, j int) bool { return newerEntry(selected[i], selected[j]) })
		return selected, nil
	}

	// Compatibility fallback for the pre-per-session aggregate. Publish only
	// the requested entry; leave aggregate-wide migration/cleanup to ListAt so
	// unrelated corrupt files cannot couple keyed recovery.
	legacy, _, _, err := readRawFor(dataDir)
	if err != nil {
		return nil, err
	}
	for _, entry := range legacy {
		if entry.SessionID != sessionID || !pidAlive(entry.PtyHostPID) {
			continue
		}
		duplicate := false
		for _, current := range selected {
			if current.Generation == entry.Generation && current.PtyHostPID == entry.PtyHostPID && current.PipePath == entry.PipePath {
				duplicate = true
				break
			}
		}
		if !duplicate {
			if err := RegisterAt(dataDir, entry); err != nil {
				return nil, err
			}
			selected = append(selected, entry)
		}
	}
	sort.Slice(selected, func(i, j int) bool { return newerEntry(selected[i], selected[j]) })
	return selected, nil
}

func registryDirFor(dataDir string) (string, error) {
	file, err := registryFileFor(dataDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(file), "windows-pty-hosts"), nil
}

func entryFileFor(dataDir, sessionID string) (string, error) {
	dir, err := registryDirFor(dataDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, hex.EncodeToString([]byte(sessionID))+".json"), nil
}

func generationEntryFileFor(dataDir, sessionID, generation string) (string, error) {
	dir, err := registryDirFor(dataDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, hex.EncodeToString([]byte(sessionID))+"."+hex.EncodeToString([]byte(generation))+".json"), nil
}

func entryPathFor(dataDir string, entry Entry) (string, error) {
	if entry.Generation == "" {
		return entryFileFor(dataDir, entry.SessionID)
	}
	return generationEntryFileFor(dataDir, entry.SessionID, entry.Generation)
}

func entryPathsFor(dataDir, sessionID string) ([]string, error) {
	dir, err := registryDirFor(dataDir)
	if err != nil {
		return nil, err
	}
	files, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	base := hex.EncodeToString([]byte(sessionID))
	legacy := base + ".json"
	prefix := base + "."
	paths := make([]string, 0, 2)
	for _, file := range files {
		if file.IsDir() {
			continue
		}
		name := file.Name()
		if name == legacy || (strings.HasPrefix(name, prefix) && filepath.Ext(name) == ".json") {
			paths = append(paths, filepath.Join(dir, name))
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func newerEntry(candidate, current Entry) bool {
	candidateTime, candidateErr := time.Parse(time.RFC3339Nano, candidate.RegisteredAt)
	currentTime, currentErr := time.Parse(time.RFC3339Nano, current.RegisteredAt)
	if candidateErr == nil && currentErr == nil && !candidateTime.Equal(currentTime) {
		return candidateTime.After(currentTime)
	}
	if candidate.RegisteredAt != current.RegisteredAt {
		return candidate.RegisteredAt > current.RegisteredAt
	}
	return candidate.Generation > current.Generation
}

func readEntryFile(path string) (Entry, error) {
	data, err := readEntryData(path)
	if err != nil {
		return Entry{}, err
	}
	var entry Entry
	if err := json.Unmarshal(data, &entry); err != nil {
		return Entry{}, fmt.Errorf("ptyregistry: parse %q: %w", path, err)
	}
	if entry.SessionID == "" || entry.PtyHostPID == 0 || entry.PipePath == "" {
		return Entry{}, fmt.Errorf("ptyregistry: malformed entry %q", path)
	}
	return entry, nil
}

func writeEntryFile(path string, entry Entry) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "pty-host-*.json.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return atomicReplace(tmpName, path)
}

// Clear deletes the registry file. Best-effort; used by tests and recovery.
func Clear() error {
	dataDir, _ := os.LookupEnv("AO_DATA_DIR")
	dir, err := registryDirFor(dataDir)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
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
