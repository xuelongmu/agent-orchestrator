package domain

import (
	"strings"
	"testing"
)

func TestProjectConfigVerificationValidation(t *testing.T) {
	tests := []struct {
		name string
		cfg  VerificationCommand
		want string
	}{
		{name: "backend", cfg: VerificationCommand{Argv: []string{"go", "test", "./..."}, WorkingDirectory: "backend", TimeoutSeconds: 60}},
		{name: "bad name", cfg: VerificationCommand{Argv: []string{"go"}}, want: "profile name"},
		{name: "empty", cfg: VerificationCommand{}, want: "executable"},
		{name: "escape", cfg: VerificationCommand{Argv: []string{"go"}, WorkingDirectory: "../outside"}, want: "must not escape"},
		{name: "long", cfg: VerificationCommand{Argv: []string{"go"}, TimeoutSeconds: 3601}, want: "between 0 and 3600"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := (ProjectConfig{WorkspaceKind: WorkspaceKindWorktree, Verification: map[string]VerificationCommand{tt.name: tt.cfg}}).Validate()
			if tt.want == "" && err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
			if tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
				t.Fatalf("Validate() error = %v, want substring %q", err, tt.want)
			}
		})
	}
}
