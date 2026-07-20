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

func TestInstallPreservesLegacyBareUserCommandAndSettings(t *testing.T) {
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
	if !reflect.DeepEqual(groups[0], legacy) {
		t.Fatalf("legacy user command changed: got %#v, want %#v", groups[0], legacy)
	}
	assertCommandCount(t, groups, testCommand, 1)
}

func TestInstallPreservesMixedValidAndLegacyEntryOrdering(t *testing.T) {
	workspace, hooksPath, manager := newTestManager(t)
	legacy := map[string]any{"type": "command", "command": "second", "legacyField": "keep"}
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
				legacy,
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
	if !reflect.DeepEqual(groups[1], legacy) {
		t.Fatalf("legacy user entry moved or changed: got %#v, want %#v", groups[1], legacy)
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
	legacy := map[string]any{"type": "command", "command": "ao hooks test other", "custom": "keep"}
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

func TestInstallPreservesLegacyCommandsWithInvalidMatchers(t *testing.T) {
	workspace, hooksPath, manager := newTestManager(t)
	entries := []any{
		map[string]any{"type": "command", "command": testCommand, "matcher": float64(42), "custom": "keep-number"},
		map[string]any{"type": "command", "command": "ao hooks test null-matcher", "matcher": nil, "custom": "keep-null"},
	}
	writeJSON(t, hooksPath, map[string]any{
		"hooks": map[string]any{"SessionStart": entries},
	})

	if err := manager.Install(context.Background(), workspace); err != nil {
		t.Fatalf("Install() error = %v", err)
	}

	groups := sessionStartGroups(t, hooksPath)
	if len(groups) != len(entries)+1 {
		t.Fatalf("SessionStart entry count = %d, want %d", len(groups), len(entries)+1)
	}
	if !reflect.DeepEqual(groups[:len(entries)], entries) {
		t.Fatalf("invalid-matcher entries changed:\ngot  %#v\nwant %#v", groups[:len(entries)], entries)
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
	for name, malformed := range map[string]any{
		"object": map[string]any{"type": "command", "command": "not-an-array"},
		"null":   nil,
		"string": "not-an-array",
	} {
		t.Run(name, func(t *testing.T) {
			workspace, hooksPath, manager := newTestManager(t)
			writeJSON(t, hooksPath, map[string]any{
				"custom": "keep",
				"hooks":  map[string]any{"SessionStart": malformed},
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
		})
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

func TestInstallDeduplicatesLegacyAndMatchedAOCommands(t *testing.T) {
	workspace, hooksPath, manager := newTestManager(t)
	writeJSON(t, hooksPath, map[string]any{
		"hooks": map[string]any{
			"SessionStart": []any{
				map[string]any{"type": "command", "command": testCommand, "source": "legacy"},
				map[string]any{
					"matcher": "startup",
					"hooks": []any{
						map[string]any{"type": "command", "command": testCommand, "source": "matched"},
						map[string]any{"type": "command", "command": "user-sibling", "custom": "keep"},
					},
				},
			},
		},
	})

	if err := manager.Install(context.Background(), workspace); err != nil {
		t.Fatalf("Install() error = %v", err)
	}

	groups := sessionStartGroups(t, hooksPath)
	assertCommandCount(t, groups, testCommand, 1)
	assertCommandCount(t, groups, "user-sibling", 1)
	if len(groups) != 2 {
		t.Fatalf("deduplicated group count = %d, want 2", len(groups))
	}
	matchedHooks := groups[1].(map[string]any)["hooks"].([]any)
	if len(matchedHooks) != 1 || matchedHooks[0].(map[string]any)["custom"] != "keep" {
		t.Fatalf("co-located user hook changed or was removed: %#v", matchedHooks)
	}
}

func TestInstallLeavesLegacyUserCommandAllSources(t *testing.T) {
	workspace, hooksPath, manager := newTestManager(t)
	writeJSON(t, hooksPath, map[string]any{
		"hooks": map[string]any{
			"SessionStart": []any{map[string]any{
				"type": "command", "command": "user-session-start", "custom": "keep",
			}},
		},
	})

	if err := manager.Install(context.Background(), workspace); err != nil {
		t.Fatalf("Install() error = %v", err)
	}

	groups := sessionStartGroups(t, hooksPath)
	if len(groups) != 2 {
		t.Fatalf("SessionStart group count = %d, want 2", len(groups))
	}
	legacy := groups[0].(map[string]any)
	if legacy["command"] != "user-session-start" || legacy["custom"] != "keep" {
		t.Fatalf("legacy user hook changed: %#v", legacy)
	}
	if _, hasMatcher := legacy["matcher"]; hasMatcher {
		t.Fatalf("legacy user hook gained a matcher: %#v", legacy)
	}
	assertCommandCount(t, groups, testCommand, 1)
}

func TestUninstallRemovesLegacyBareAOCommand(t *testing.T) {
	workspace, hooksPath, manager := newTestManager(t)
	writeJSON(t, hooksPath, map[string]any{
		"hooks": map[string]any{
			"SessionStart": []any{
				map[string]any{"type": "command", "command": testCommand},
				map[string]any{"type": "command", "command": "user-session-start", "custom": "keep"},
			},
		},
	})

	if err := manager.Uninstall(context.Background(), workspace); err != nil {
		t.Fatalf("Uninstall() error = %v", err)
	}

	groups := sessionStartGroups(t, hooksPath)
	assertCommandCount(t, groups, testCommand, 0)
	if len(groups) != 1 {
		t.Fatalf("SessionStart group count = %d, want 1", len(groups))
	}
	hook := groups[0].(map[string]any)
	if hook["command"] != "user-session-start" || hook["custom"] != "keep" {
		t.Fatalf("remaining user hook changed: %#v", hook)
	}
	if _, hasMatcher := hook["matcher"]; hasMatcher {
		t.Fatalf("remaining user hook gained a matcher: %#v", hook)
	}
}

func TestAreInstalledWithLegacyBareCommands(t *testing.T) {
	for _, tc := range []struct {
		name      string
		command   string
		installed bool
	}{
		{name: "AO command", command: testCommand, installed: true},
		{name: "user command", command: "user-session-start", installed: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace, hooksPath, manager := newTestManager(t)
			writeJSON(t, hooksPath, map[string]any{
				"hooks": map[string]any{
					"SessionStart": []any{map[string]any{
						"type": "command", "command": tc.command, "custom": "keep",
					}},
				},
			})
			before, err := os.ReadFile(hooksPath)
			if err != nil {
				t.Fatal(err)
			}

			installed, err := manager.AreInstalled(context.Background(), workspace)
			if err != nil {
				t.Fatalf("AreInstalled() error = %v", err)
			}
			if installed != tc.installed {
				t.Fatalf("AreInstalled() = %v, want %v", installed, tc.installed)
			}
			after, err := os.ReadFile(hooksPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Fatalf("AreInstalled changed file\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
	}
}

func TestInstallUninstallRestoresUserEntries(t *testing.T) {
	workspace, hooksPath, manager := newTestManager(t)
	original := map[string]any{
		"custom": map[string]any{"keep": true},
		"hooks": map[string]any{
			"SessionStart": []any{
				map[string]any{"type": "command", "command": "user-bare", "bareField": "keep"},
				map[string]any{
					"matcher":    "startup",
					"groupField": "keep",
					"hooks": []any{
						map[string]any{"type": "command", "command": "user-grouped", "hookField": "keep"},
					},
				},
			},
			"OtherEvent": []any{map[string]any{"type": "command", "command": "user-other", "custom": "keep"}},
		},
	}
	writeJSON(t, hooksPath, original)

	if err := manager.Install(context.Background(), workspace); err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	if err := manager.Uninstall(context.Background(), workspace); err != nil {
		t.Fatalf("Uninstall() error = %v", err)
	}

	if got := readJSONObject(t, hooksPath); !reflect.DeepEqual(got, original) {
		t.Fatalf("Install/Uninstall did not restore user config:\ngot  %#v\nwant %#v", got, original)
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
