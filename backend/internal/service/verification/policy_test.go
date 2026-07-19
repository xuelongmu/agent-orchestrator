package verification

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadPolicyUsesStartupFileAndProjectOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "verification.json")
	body := `{"profiles":{"focused":{"argv":["go","test","./internal/foo"],"workingDirectory":"backend"}},"projects":{"ao":{"focused":{"argv":["go","test","./internal/bar"],"workingDirectory":"backend"}}}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil { t.Fatal(err) }
	policy, err := LoadPolicy(path)
	if err != nil { t.Fatal(err) }
	command, ok := policy.Resolve("ao", "focused")
	if !ok || command.Argv[2] != "./internal/bar" { t.Fatalf("project command = %#v, %v", command, ok) }
	command, ok = policy.Resolve("other", "focused")
	if !ok || command.Argv[2] != "./internal/foo" { t.Fatalf("global command = %#v, %v", command, ok) }
}

func TestPolicyRejectsShellAndTraversal(t *testing.T) {
	tests := []struct { name string; command Command; want string }{
		{name: "shell", command: Command{Argv: []string{"pwsh", "-Command", "evil"}}, want: "shell executable"},
		{name: "traversal", command: Command{Argv: []string{"go", "test"}, WorkingDirectory: "../outside"}, want: "must not escape"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := (Policy{Profiles: map[string]Command{"bad": test.command}}).Validate()
			if err == nil || !strings.Contains(err.Error(), test.want) { t.Fatalf("error = %v", err) }
		})
	}
}
