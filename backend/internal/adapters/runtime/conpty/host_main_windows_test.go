//go:build windows

package conpty

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/runtime/conpty/ptyregistry"
)

type failingReadyWriter struct{}

func (failingReadyWriter) Write([]byte) (int, error) {
	return 0, errors.New("startup pipe closed")
}

func TestRunHostReadyWriteFailureReleasesSetupResources(t *testing.T) {
	isolateRegistry(t)
	t.Setenv(hostGenerationEnv, "ready-write-failure")
	command := windowsCommand(t)
	_, _ = newConPTY(t.TempDir(), filepath.Join(t.TempDir(), "missing-warmup.exe"), nil)
	before := currentProcessHandleCount(t)

	if code := RunHost([]string{"ready-write-failure", t.TempDir(), command, "/D", "/Q", "/K"}, failingReadyWriter{}); code != 1 {
		t.Fatalf("RunHost code = %d, want 1", code)
	}
	entries, err := ptyregistry.LookupAll("ready-write-failure")
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed READY publication remained registered: entries=%v err=%v", entries, err)
	}
	waitForHandleCount(t, before+maxHandleNoise)
}

func TestRunHostRegistryFailureReleasesSetupResources(t *testing.T) {
	home := t.TempDir()
	blockedDataDir := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blockedDataDir, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv(dataDirEnv, blockedDataDir)
	t.Setenv("AO_RUN_FILE", filepath.Join(blockedDataDir, "running.json"))
	t.Setenv(hostGenerationEnv, "registry-failure")
	command := windowsCommand(t)
	_, _ = newConPTY(t.TempDir(), filepath.Join(t.TempDir(), "missing-warmup.exe"), nil)
	before := currentProcessHandleCount(t)

	if code := RunHost([]string{"registry-failure", t.TempDir(), command, "/D", "/Q", "/K"}, os.Stdout); code != 1 {
		t.Fatalf("RunHost code = %d, want 1", code)
	}
	waitForHandleCount(t, before+maxHandleNoise)
}

func windowsCommand(t *testing.T) string {
	t.Helper()
	command, err := exec.LookPath("cmd.exe")
	if err != nil {
		t.Fatal(err)
	}
	command, err = filepath.Abs(command)
	if err != nil {
		t.Fatal(err)
	}
	return command
}
