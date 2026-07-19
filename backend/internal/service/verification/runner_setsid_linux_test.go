//go:build linux

package verification

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestLinuxCompletionSignalsGroupBeforeReap(t *testing.T) {
	for _, tc := range []struct {
		name       string
		canceled   bool
		exited     bool
		exitOnKill bool
	}{
		{name: "natural exit", exited: true},
		{name: "cancellation", canceled: true, exitOnKill: true},
		{name: "cancellation and exit both ready", canceled: true, exited: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for range 100 {
				canceled := make(chan struct{}, 1)
				exited := make(chan error, 1)
				if tc.canceled {
					canceled <- struct{}{}
				}
				if tc.exited {
					exited <- nil
				}
				var events []string
				waitErr, observeErr := completeLinuxVerificationProcess(
					123,
					canceled,
					exited,
					func(pid int) {
						if pid != 123 {
							t.Fatalf("signal pid = %d, want 123", pid)
						}
						events = append(events, "signal")
						if tc.exitOnKill {
							exited <- nil
						}
					},
					func() error {
						events = append(events, "reap")
						return nil
					},
				)
				if waitErr != nil || observeErr != nil {
					t.Fatalf("completeLinuxVerificationProcess() = %v, %v", waitErr, observeErr)
				}
				if got := strings.Join(events, ","); got != "signal,reap" {
					t.Fatalf("completion order = %q, want signal,reap", got)
				}
			}
		})
	}
}

func TestLinuxGuardianCleansSetsidDescendantOnCancellation(t *testing.T) {
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
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runner did not cancel")
	}
	assertProcessDies(t, pid, "setsid descendant survived cancellation")
}

func TestLinuxGuardianCleansSetsidDescendantOnDaemonHardExit(t *testing.T) {
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
	if err := outer.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = outer.Wait()
	assertProcessDies(t, pid, "setsid descendant survived daemon hard exit")
}

func TestLinuxGuardianCleansSetsidDescendantOnNormalParentCompletion(t *testing.T) {
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
	assertProcessDies(t, pid, "setsid descendant survived normal parent completion")
}

func TestLinuxGuardianDoesNotOwnUnrelatedWorker(t *testing.T) {
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
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runner did not cancel")
	}
	assertProcessDies(t, pid, "setsid descendant survived cancellation")
	workerFD, workerErr := unix.PidfdOpen(worker.Process.Pid, 0)
	if workerErr != nil {
		t.Fatalf("unrelated worker pid %d was accidentally owned by verifier guardian", worker.Process.Pid)
	}
	_ = unix.Close(workerFD)
}

func assertProcessDies(t *testing.T, pid int, message string) {
	t.Helper()
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		if errors.Is(err, unix.ESRCH) {
			return
		}
		t.Fatalf("open pidfd %d: %v", pid, err)
	}
	defer func() { _ = unix.Close(fd) }()
	poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	if _, err := unix.Poll(poll, 5000); err != nil {
		t.Fatalf("poll pidfd %d: %v", pid, err)
	}
	if poll[0].Revents&unix.POLLIN == 0 {
		t.Fatalf("%s: pid %d", message, pid)
	}
}
