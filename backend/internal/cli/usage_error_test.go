package cli

import (
	"strings"
	"testing"
)

func TestUsageErrorCommandReportsOnlyRegisteredCommandPath(t *testing.T) {
	root := NewRootCommand(Deps{})
	tests := []struct {
		name        string
		args        []string
		wantCommand string
		wantPath    string
	}{
		{
			name:        "URL",
			args:        []string{"https://gitlab.com/org/repo/-/merge_requests/9"},
			wantCommand: "<unknown>",
			wantPath:    "ao <unknown>",
		},
		{
			name:        "absolute path",
			args:        []string{`C:\Users\name\private\project`},
			wantCommand: "<unknown>",
			wantPath:    "ao <unknown>",
		},
		{
			name:        "free text",
			args:        []string{"review this internal change without sharing details"},
			wantCommand: "<unknown>",
			wantPath:    "ao <unknown>",
		},
		{
			name:        "flag and its value are not inspected",
			args:        []string{"status", "--format", "private-value"},
			wantCommand: "status",
			wantPath:    "ao status",
		},
		{
			name:        "leading flag",
			args:        []string{"--config", `C:\Users\name\private.yaml`},
			wantCommand: "ao",
			wantPath:    "ao",
		},
		{
			name:        "nested valid commands redact positional value",
			args:        []string{"session", "rename", "private-session", "new-name"},
			wantCommand: "rename",
			wantPath:    "ao session rename <unknown>",
		},
		{
			name:        "nested alias is canonicalized",
			args:        []string{"project", "remove", "private-project"},
			wantCommand: "rm",
			wantPath:    "ao project rm <unknown>",
		},
		{
			name:        "root command is invalid in nested context",
			args:        []string{"session", "status"},
			wantCommand: "session",
			wantPath:    "ao session <unknown>",
		},
		{
			name:        "nested command is invalid at root",
			args:        []string{"rename", "private-session"},
			wantCommand: "<unknown>",
			wantPath:    "ao <unknown>",
		},
		{
			name:        "no arguments",
			wantCommand: "ao",
			wantPath:    "ao",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			command, commandPath := usageErrorCommand(root, tt.args)
			if command != tt.wantCommand || commandPath != tt.wantPath {
				t.Fatalf("usageErrorCommand(%q) = (%q, %q), want (%q, %q)",
					tt.args, command, commandPath, tt.wantCommand, tt.wantPath)
			}
			for _, arg := range tt.args {
				if strings.Contains(arg, "private") &&
					(strings.Contains(command, arg) || strings.Contains(commandPath, arg)) {
					t.Fatalf("raw argument %q leaked into (%q, %q)", arg, command, commandPath)
				}
			}
		})
	}
}
