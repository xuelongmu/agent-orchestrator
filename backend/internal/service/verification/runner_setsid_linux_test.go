//go:build linux

package verification

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

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
	if !processalive.Alive(worker.Process.Pid) {
		t.Fatalf("unrelated worker pid %d was accidentally owned by verifier guardian", worker.Process.Pid)
	}
}

func assertProcessDies(t *testing.T, pid int, message string) {
	t.Helper()
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
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
