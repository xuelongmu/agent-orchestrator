//go:build windows

package process

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

const (
	sessionJobHelperMode = "AO_SESSION_JOB_TEST_HELPER"
	sessionJobHelperGate = "AO_SESSION_JOB_TEST_GATE"
	sessionJobHelperPID  = "AO_SESSION_JOB_TEST_CHILD_PID"
)

func TestMain(m *testing.M) {
	switch os.Getenv(sessionJobHelperMode) {
	case "parent":
		runSessionJobParentHelper()
		return
	case "child":
		for {
			time.Sleep(time.Minute)
		}
	}
	os.Exit(m.Run())
}

func runSessionJobParentHelper() {
	gate := os.Getenv(sessionJobHelperGate)
	for {
		if _, err := os.Stat(gate); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	child := exec.Command(os.Args[0], "-test.run=^$")
	child.Env = append(os.Environ(), sessionJobHelperMode+"=child")
	if err := child.Start(); err != nil {
		os.Exit(2)
	}
	if err := os.WriteFile(os.Getenv(sessionJobHelperPID), []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
		os.Exit(3)
	}
	for {
		time.Sleep(time.Minute)
	}
}

func TestSessionJobTerminatesAssignedProcessTree(t *testing.T) {
	dataDir := t.TempDir()
	job, err := CreateSessionJob(dataDir, "tree-test", "generation-a")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = job.Close() })

	gate := filepath.Join(t.TempDir(), "go")
	childPIDFile := filepath.Join(t.TempDir(), "child.pid")
	parent := exec.Command(os.Args[0], "-test.run=^$")
	parent.Env = append(os.Environ(),
		sessionJobHelperMode+"=parent",
		sessionJobHelperGate+"="+gate,
		sessionJobHelperPID+"="+childPIDFile,
	)
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	if err := job.Assign(parent.Process.Pid); err != nil {
		_ = parent.Process.Kill()
		_ = parent.Wait()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if parent.Process != nil {
			_ = parent.Process.Kill()
			_, _ = parent.Process.Wait()
		}
	})
	if err := os.WriteFile(gate, []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}

	childPID := waitForChildPID(t, childPIDFile)
	parentHandle := openSynchronizeHandle(t, parent.Process.Pid)
	defer func() { _ = windows.CloseHandle(parentHandle) }()
	childHandle := openSynchronizeHandle(t, childPID)
	defer func() { _ = windows.CloseHandle(childHandle) }()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := job.TerminateAndWait(ctx); err != nil {
		t.Fatal(err)
	}
	assertProcessHandleExited(t, parentHandle)
	assertProcessHandleExited(t, childHandle)
}

func TestSessionJobIdentityIncludesRuntimeGeneration(t *testing.T) {
	dataDir := t.TempDir()
	first, err := sessionJobName(dataDir, "same-session", "generation-a")
	if err != nil {
		t.Fatal(err)
	}
	second, err := sessionJobName(dataDir, "same-session", "generation-b")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("job names collide across runtime generations: %q", first)
	}
	if _, err := sessionJobName(dataDir, "same-session", ""); err == nil {
		t.Fatal("empty runtime generation should be rejected")
	}

	job, err := CreateSessionJob(dataDir, "same-session", "generation-a")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = job.Close() })
	if _, err := OpenSessionJob(dataDir, "same-session", "generation-b"); !errors.Is(err, ErrSessionJobNotFound) {
		t.Fatalf("open replacement generation = %v, want ErrSessionJobNotFound", err)
	}
}

func waitForChildPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(string(data))
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			return pid
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("child pid file %q was not created", path)
	return 0
}

func openSynchronizeHandle(t *testing.T, pid int) windows.Handle {
	t.Helper()
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		t.Fatalf("open process %d: %v", pid, err)
	}
	return handle
}

func assertProcessHandleExited(t *testing.T, handle windows.Handle) {
	t.Helper()
	result, err := windows.WaitForSingleObject(handle, 3_000)
	if err != nil {
		t.Fatal(err)
	}
	if result != uint32(windows.WAIT_OBJECT_0) {
		t.Fatalf("process wait result = %d, want WAIT_OBJECT_0", result)
	}
}
