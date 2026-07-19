//go:build darwin || linux

package verification

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestUnixGuardianCleansDescendantsAfterRunnerHardExit(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	outer := exec.Command(os.Args[0], "-test.run=TestVerificationProcessHelper", "--", "outer-runner", pidFile)
	outer.Env = append(os.Environ(), "GO_WANT_VERIFY_HELPER=1")
	if err := outer.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = outer.Process.Kill()
		_ = outer.Wait()
	}()

	pid := waitForPIDFile(t, pidFile)
	exit, err := newProcessExitWatcher(pid)
	if err != nil {
		t.Fatalf("watch descendant pid %d: %v", pid, err)
	}
	defer func() { _ = exit.Close() }()
	if err := outer.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = outer.Wait()
	if err := exit.Wait(5 * time.Second); err != nil {
		t.Fatalf("descendant pid %d survived runner hard exit: %v", pid, err)
	}
}

func waitForPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		body, err := os.ReadFile(path)
		if err == nil {
			pid, convErr := strconv.Atoi(string(body))
			if convErr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("helper child did not start")
	return 0
}
