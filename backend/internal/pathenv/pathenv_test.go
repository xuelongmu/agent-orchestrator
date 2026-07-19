package pathenv

import (
	"errors"
	"path/filepath"
	"runtime"
	"testing"
)

func TestEffectivePreservesConfiguredPath(t *testing.T) {
	if got := Effective(func(string) string { return "custom-path" }); got != "custom-path" {
		t.Fatalf("Effective() = %q, want custom-path", got)
	}
}

func TestEffectiveUsesPlatformDefaultWhenPathUnset(t *testing.T) {
	want := ""
	if runtime.GOOS != "windows" {
		want = "/usr/local/bin:/usr/bin:/bin"
	}
	if got := Effective(func(string) string { return "" }); got != want {
		t.Fatalf("Effective() = %q, want %q", got, want)
	}
}

func TestAgentBinDirUsesConfiguredRunFile(t *testing.T) {
	root := t.TempDir()
	got, err := AgentBinDir(func(name string) string {
		if name == "AO_RUN_FILE" {
			return filepath.Join(root, "isolated", "running.json")
		}
		return ""
	}, func() (string, error) { return "", errors.New("must not resolve home") })
	if err != nil {
		t.Fatalf("AgentBinDir: %v", err)
	}
	want := filepath.Join(root, "isolated", "bin")
	if got != want {
		t.Fatalf("AgentBinDir = %q, want %q", got, want)
	}
}

func TestAgentBinDirDefaultsUnderAOHome(t *testing.T) {
	home := t.TempDir()
	got, err := AgentBinDir(func(string) string { return "" }, func() (string, error) { return home, nil })
	if err != nil {
		t.Fatalf("AgentBinDir: %v", err)
	}
	want := filepath.Join(home, ".ao", "bin")
	if got != want {
		t.Fatalf("AgentBinDir = %q, want %q", got, want)
	}
}
