//go:build darwin

package verification

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/processalive"
)

// These regressions deliberately codify Darwin's narrower process-group
// guarantee. They use isolated test children and prove that we neither claim
// ownership of a setsid escape nor accidentally extend ownership to workers.
func TestDarwinSetsidEscapeOwnershipLimit(t *testing.T) {
	t.Run("cancellation", func(t *testing.T) {
		pidFile := filepath.Join(t.TempDir(), "setsid-child.pid")
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, err := testOSRunner().Run(ctx, RunSpec{
				Argv:   []string{os.Args[0], "-test.run=TestVerificationProcessHelper", "--", "setsid-parent", pidFile},
				Dir:    t.TempDir(),
				Env:    append(os.Environ(), "GO_WANT_VERIFY_HELPER=1"),
				Output: io.Discard,
			})
			done <- err
		}()
		pid := waitForPIDFile(t, pidFile)
		defer killIsolatedTestProcess(pid)
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("runner did not cancel")
		}
		assertDarwinEscapeSurvives(t, pid, "cancellation")
	})

	t.Run("daemon hard exit", func(t *testing.T) {
		pidFile := filepath.Join(t.TempDir(), "setsid-child.pid")
		outer := exec.Command(os.Args[0], "-test.run=TestVerificationProcessHelper", "--", "outer-runner-setsid", pidFile)
		outer.Env = append(os.Environ(), "GO_WANT_VERIFY_HELPER=1")
		if err := outer.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() {
			_ = outer.Process.Kill()
			_ = outer.Wait()
		}()
		pid := waitForPIDFile(t, pidFile)
		defer killIsolatedTestProcess(pid)
		if err := outer.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		_ = outer.Wait()
		assertDarwinEscapeSurvives(t, pid, "daemon hard exit")
	})

	t.Run("normal parent completion", func(t *testing.T) {
		pidFile := filepath.Join(t.TempDir(), "setsid-child.pid")
		res, err := testOSRunner().Run(context.Background(), RunSpec{
			Argv:   []string{os.Args[0], "-test.run=TestVerificationProcessHelper", "--", "setsid-fast-parent", pidFile},
			Dir:    t.TempDir(),
			Env:    append(os.Environ(), "GO_WANT_VERIFY_HELPER=1"),
			Output: io.Discard,
		})
		if err != nil || res.ExitCode != 0 {
			t.Fatalf("Run() = %#v, %v", res, err)
		}
		pid := waitForPIDFile(t, pidFile)
		defer killIsolatedTestProcess(pid)
		assertDarwinEscapeSurvives(t, pid, "normal parent completion")
	})

	t.Run("unrelated worker", func(t *testing.T) {
		worker := exec.Command(os.Args[0], "-test.run=TestVerificationProcessHelper", "--", "child")
		worker.Env = append(os.Environ(), "GO_WANT_VERIFY_HELPER=1")
		if err := worker.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() {
			_ = worker.Process.Kill()
			_ = worker.Wait()
		}()

		pidFile := filepath.Join(t.TempDir(), "setsid-child.pid")
		res, err := testOSRunner().Run(context.Background(), RunSpec{
			Argv:   []string{os.Args[0], "-test.run=TestVerificationProcessHelper", "--", "setsid-fast-parent", pidFile},
			Dir:    t.TempDir(),
			Env:    append(os.Environ(), "GO_WANT_VERIFY_HELPER=1"),
			Output: io.Discard,
		})
		if err != nil || res.ExitCode != 0 {
			t.Fatalf("Run() = %#v, %v", res, err)
		}
		pid := waitForPIDFile(t, pidFile)
		defer killIsolatedTestProcess(pid)
		assertDarwinEscapeSurvives(t, pid, "normal parent completion")
		if !processalive.Alive(worker.Process.Pid) {
			t.Fatalf("unrelated worker pid %d was accidentally owned by verifier guardian", worker.Process.Pid)
		}
	})
}

func assertDarwinEscapeSurvives(t *testing.T, pid int, event string) {
	t.Helper()
	if !processalive.Alive(pid) {
		t.Fatalf("setsid test child unexpectedly died after %s; update the documented Darwin guarantee", event)
	}
}

func killIsolatedTestProcess(pid int) {
	_ = syscall.Kill(pid, syscall.SIGKILL)
}
