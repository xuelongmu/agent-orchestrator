package hooksjson

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

const testCommand = "ao hooks test session-start"

func TestInstallMigratesLegacyBareCommandAndPreservesSettings(t *testing.T) {
	workspace, hooksPath, manager := newTestManager(t)
	legacy := map[string]any{
		"type":    "command",
		"command": "python .claude/hooks/session_start.py",
		"timeout": float64(7),
		"custom":  map[string]any{"owner": "user"},
	}
	otherEvent := []any{map[string]any{
		"matcher": "other",
		"hooks":   []any{map[string]any{"type": "command", "command": "leave unchanged"}},
	}}
	writeJSON(t, hooksPath, map[string]any{
		"theme":  "dark",
		"custom": map[string]any{"enabled": true},
		"hooks": map[string]any{
			"SessionStart": []any{legacy},
			"OtherEvent":   otherEvent,
		},
	})

	if err := manager.Install(context.Background(), workspace); err != nil {
		t.Fatalf("Install() error = %v", err)
	}

	settings := readJSONObject(t, hooksPath)
	if settings["theme"] != "dark" || !reflect.DeepEqual(settings["custom"], map[string]any{"enabled": true}) {
		t.Fatalf("unrelated top-level settings changed: %#v", settings)
	}
	hooks := settings["hooks"].(map[string]any)
	if !reflect.DeepEqual(hooks["OtherEvent"], otherEvent) {
		t.Fatalf("unmanaged event changed: got %#v, want %#v", hooks["OtherEvent"], otherEvent)
	}

	groups := hooks["SessionStart"].([]any)
	if len(groups) != 2 {
		t.Fatalf("SessionStart group count = %d, want 2", len(groups))
	}
	migrated := groups[0].(map[string]any)
	if migrated["matcher"] != "" {
		t.Fatalf("migrated matcher = %#v, want empty string", migrated["matcher"])
	}
	if got := migrated["hooks"].([]any); len(got) != 1 || !reflect.DeepEqual(got[0], legacy) {
		t.Fatalf("migrated hooks = %#v, want original entry %#v", got, legacy)
	}
	assertCommandCount(t, groups, testCommand, 1)
}

func TestInstallPreservesMixedValidAndLegacyEntryOrdering(t *testing.T) {
	workspace, hooksPath, manager := newTestManager(t)
	writeJSON(t, hooksPath, map[string]any{
		"hooks": map[string]any{
			"SessionStart": []any{
				map[string]any{
					"matcher": "startup",
					"label":   "keep-group-field",
					"hooks": []any{
						map[string]any{"type": "command", "command": "first", "entryField": "keep"},
					},
				},
				map[string]any{"type": "command", "command": "second", "legacyField": "keep"},
			},
		},
	})

	if err := manager.Install(context.Background(), workspace); err != nil {
		t.Fatalf("Install() error = %v", err)
	}

	groups := sessionStartGroups(t, hooksPath)
	if len(groups) != 2 {
		t.Fatalf("SessionStart group count = %d, want 2", len(groups))
	}
	first := groups[0].(map[string]any)
	if first["label"] != "keep-group-field" {
		t.Fatalf("valid group field was lost: %#v", first)
	}
	firstHooks := first["hooks"].([]any)
	if len(firstHooks) != 2 || firstHooks[0].(map[string]any)["command"] != "first" || firstHooks[1].(map[string]any)["command"] != testCommand {
		t.Fatalf("valid group hook ordering = %#v", firstHooks)
	}
	if firstHooks[0].(map[string]any)["entryField"] != "keep" {
		t.Fatalf("valid hook field was lost: %#v", firstHooks[0])
	}
	second := groups[1].(map[string]any)
	if second["matcher"] != "" || second["hooks"].([]any)[0].(map[string]any)["command"] != "second" {
		t.Fatalf("legacy entry moved or was not migrated: %#v", second)
	}
	if second["hooks"].([]any)[0].(map[string]any)["legacyField"] != "keep" {
		t.Fatalf("legacy hook field was lost: %#v", second)
	}
}

func TestInstallIsIdempotent(t *testing.T) {
	workspace, hooksPath, manager := newTestManager(t)
	writeJSON(t, hooksPath, map[string]any{
		"hooks": map[string]any{
			"SessionStart": []any{map[string]any{"type": "command", "command": "user"}},
		},
	})

	if err := manager.Install(context.Background(), workspace); err != nil {
		t.Fatalf("first Install() error = %v", err)
	}
	first, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Install(context.Background(), workspace); err != nil {
		t.Fatalf("second Install() error = %v", err)
	}
	second, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(second) != string(first) {
		t.Fatalf("repeat install changed file\nfirst:\n%s\nsecond:\n%s", first, second)
	}
	assertCommandCount(t, sessionStartGroups(t, hooksPath), testCommand, 1)
}

func TestInstallHandlesNullTopLevelHooks(t *testing.T) {
	workspace, hooksPath, manager := newTestManager(t)
	writeJSON(t, hooksPath, map[string]any{
		"theme": "dark",
		"hooks": nil,
	})

	if err := manager.Install(context.Background(), workspace); err != nil {
		t.Fatalf("Install() error = %v", err)
	}

	settings := readJSONObject(t, hooksPath)
	if settings["theme"] != "dark" {
		t.Fatalf("unrelated setting changed: %#v", settings)
	}
	groups := settings["hooks"].(map[string]any)["SessionStart"].([]any)
	assertCommandCount(t, groups, testCommand, 1)
}

func TestInstallNormalizesLegacyCommandsInUnmanagedEvents(t *testing.T) {
	workspace, hooksPath, manager := newTestManager(t)
	legacy := map[string]any{"type": "command", "command": "user-other", "custom": "keep"}
	malformed := map[string]any{"type": "command", "command": "opaque-event"}
	writeJSON(t, hooksPath, map[string]any{
		"hooks": map[string]any{
			"OtherEvent":     []any{legacy},
			"MalformedEvent": malformed,
		},
	})

	if err := manager.Install(context.Background(), workspace); err != nil {
		t.Fatalf("Install() error = %v", err)
	}

	hooks := readJSONObject(t, hooksPath)["hooks"].(map[string]any)
	otherGroups := hooks["OtherEvent"].([]any)
	if len(otherGroups) != 1 {
		t.Fatalf("OtherEvent group count = %d, want 1", len(otherGroups))
	}
	group := otherGroups[0].(map[string]any)
	if group["matcher"] != "" || !reflect.DeepEqual(group["hooks"].([]any), []any{legacy}) {
		t.Fatalf("unmanaged legacy event was not normalized: %#v", group)
	}
	if !reflect.DeepEqual(hooks["MalformedEvent"], malformed) {
		t.Fatalf("malformed unmanaged event changed: got %#v, want %#v", hooks["MalformedEvent"], malformed)
	}
}

func TestInstallPreservesHybridCommandEntries(t *testing.T) {
	workspace, hooksPath, manager := newTestManager(t)
	hybrids := []any{
		map[string]any{"type": "command", "command": "user-null", "hooks": nil, "custom": "keep-null"},
		map[string]any{"type": "command", "command": "user-string", "hooks": "not-an-array", "custom": "keep-string"},
	}
	writeJSON(t, hooksPath, map[string]any{
		"hooks": map[string]any{"SessionStart": hybrids},
	})

	if err := manager.Install(context.Background(), workspace); err != nil {
		t.Fatalf("Install() error = %v", err)
	}

	groups := sessionStartGroups(t, hooksPath)
	if len(groups) != len(hybrids)+1 {
		t.Fatalf("SessionStart entry count = %d, want %d", len(groups), len(hybrids)+1)
	}
	if !reflect.DeepEqual(groups[:len(hybrids)], hybrids) {
		t.Fatalf("hybrid entries changed:\ngot  %#v\nwant %#v", groups[:len(hybrids)], hybrids)
	}
	assertCommandCount(t, groups, testCommand, 1)
}

func TestInstallPreservesMalformedAndNonCommandEntries(t *testing.T) {
	workspace, hooksPath, manager := newTestManager(t)
	original := []any{
		nil,
		"opaque",
		float64(42),
		map[string]any{"type": "prompt", "command": "not a command hook", "custom": true},
		map[string]any{"type": "command", "command": float64(123)},
		map[string]any{"matcher": "", "hooks": "not-an-array", "custom": "keep"},
		map[string]any{"matcher": "", "hooks": nil, "custom": "keep-null"},
		map[string]any{"matcher": float64(42), "hooks": []any{}, "custom": "keep-matcher"},
		map[string]any{
			"matcher": "other",
			"custom":  "keep-hook-list",
			"hooks": []any{
				nil,
				"opaque-hook",
				float64(7),
				map[string]any{"type": float64(42), "command": false, "custom": "keep-hook"},
			},
		},
	}
	writeJSON(t, hooksPath, map[string]any{
		"hooks": map[string]any{"SessionStart": original},
	})

	if err := manager.Install(context.Background(), workspace); err != nil {
		t.Fatalf("Install() error = %v", err)
	}

	groups := sessionStartGroups(t, hooksPath)
	if len(groups) != len(original)+1 {
		t.Fatalf("SessionStart entry count = %d, want %d", len(groups), len(original)+1)
	}
	if !reflect.DeepEqual(groups[:len(original)], original) {
		t.Fatalf("malformed/non-command entries changed:\ngot  %#v\nwant %#v", groups[:len(original)], original)
	}
	assertCommandCount(t, groups, testCommand, 1)
}

func TestInstallRejectsMalformedEventWithoutChangingFile(t *testing.T) {
	workspace, hooksPath, manager := newTestManager(t)
	writeJSON(t, hooksPath, map[string]any{
		"custom": "keep",
		"hooks": map[string]any{
			"SessionStart": map[string]any{"type": "command", "command": "not-an-array"},
		},
	})
	before, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatal(err)
	}

	if err := manager.Install(context.Background(), workspace); err == nil {
		t.Fatal("Install() error = nil, want malformed event error")
	}
	after, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("malformed event changed file\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestInstallNormalizesExistingLegacyAOCommandWithoutDuplicate(t *testing.T) {
	workspace, hooksPath, manager := newTestManager(t)
	writeJSON(t, hooksPath, map[string]any{
		"hooks": map[string]any{
			"SessionStart": []any{map[string]any{
				"type": "command", "command": testCommand, "custom": "keep",
			}},
		},
	})

	if err := manager.Install(context.Background(), workspace); err != nil {
		t.Fatalf("Install() error = %v", err)
	}

	groups := sessionStartGroups(t, hooksPath)
	if len(groups) != 1 {
		t.Fatalf("SessionStart group count = %d, want 1", len(groups))
	}
	assertCommandCount(t, groups, testCommand, 1)
	group := groups[0].(map[string]any)
	if group["matcher"] != "startup" {
		t.Fatalf("legacy AO hook matcher = %#v, want startup", group["matcher"])
	}
	hook := group["hooks"].([]any)[0].(map[string]any)
	if hook["custom"] != "keep" {
		t.Fatalf("legacy AO hook fields were lost: %#v", hook)
	}
}

func newTestManager(t *testing.T) (workspace, hooksPath string, manager Manager) {
	t.Helper()
	workspace = t.TempDir()
	hooksPath = filepath.Join(workspace, ".agent", "settings.json")
	startup := "startup"
	manager = Manager{
		Label:         "test",
		CommandPrefix: "ao hooks test ",
		Timeout:       10,
		Path: func(workspacePath string) string {
			return filepath.Join(workspacePath, ".agent", "settings.json")
		},
		Managed: []HookSpec{{Event: "SessionStart", Matcher: &startup, Command: testCommand}},
	}
	return workspace, hooksPath, manager
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readJSONObject(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func sessionStartGroups(t *testing.T, path string) []any {
	t.Helper()
	settings := readJSONObject(t, path)
	return settings["hooks"].(map[string]any)["SessionStart"].([]any)
}

func assertCommandCount(t *testing.T, groups []any, command string, want int) {
	t.Helper()
	count := 0
	for _, groupValue := range groups {
		group, ok := groupValue.(map[string]any)
		if !ok {
			continue
		}
		hooks, ok := group["hooks"].([]any)
		if !ok {
			continue
		}
		for _, hookValue := range hooks {
			hook, ok := hookValue.(map[string]any)
			if ok && hook["command"] == command {
				count++
			}
		}
	}
	if count != want {
		t.Fatalf("command %q count = %d, want %d (groups: %#v)", command, count, want, groups)
	}
}
