//go:build windows

package verification

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWindowsPolicyRejectsBatchBackedExecutables(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "custom-tool.cmd"), []byte("@exit /b 0\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, executable := range []string{"custom-tool", "pnpm.cmd", "tool.bat"} {
		err := validateCommand(Command{Argv: []string{executable}})
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "batch") {
			t.Fatalf("validateCommand(%q) error = %v", executable, err)
		}
	}
}

func TestWindowsPolicyAllowsInternallyMappedNPM(t *testing.T) {
	if err := validateCommand(Command{Argv: []string{"npm", "--version"}}); err != nil {
		t.Fatalf("npm policy rejected: %v", err)
	}
}
