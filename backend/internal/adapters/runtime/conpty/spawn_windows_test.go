//go:build windows

package conpty

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	spawnHelperModeEnv = "AO_CONPTY_SPAWN_TEST_HELPER"
	spawnHelperPIDEnv  = "AO_CONPTY_SPAWN_TEST_PID_FILE"
	// The Go runtime may create a small burst of Windows thread/event handles
	// while these subprocess tests run. Thirty-two leaked per-call handles would
	// still exceed this bounded allowance deterministically.
	maxHandleNoise = 16
)

func TestMain(m *testing.M) {
	if mode := os.Getenv(spawnHelperModeEnv); mode != "" {
		runSpawnHelper(mode)
		return
	}
	os.Exit(m.Run())
}

func runSpawnHelper(mode string) {
	if pidFile := os.Getenv(spawnHelperPIDEnv); pidFile != "" {
		_ = os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0o600)
	}
	// Give the parent test time to retain an exact process handle before this
	// helper exits or reports READY. Tests never act on a reusable PID alone.
	time.Sleep(100 * time.Millisecond)
	switch mode {
	case "exit-with-diagnostic":
		_, _ = fmt.Fprintln(os.Stderr, "deterministic startup failure")
		os.Exit(23)
	case "wrong-ready-pid":
		_, _ = fmt.Fprintln(os.Stdout, "READY:1 43210")
	case "ready":
		_, _ = fmt.Fprintf(os.Stdout, "READY:%d 43210\n", os.Getpid())
	case "hang":
	default:
		os.Exit(24)
	}
	for {
		time.Sleep(time.Minute)
	}
}

func TestDefaultSpawnHostFailureBoundariesReleaseOwnedResources(t *testing.T) {
	t.Run("already canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, _, err := defaultSpawnHost(ctx, "already-canceled", t.TempDir(), []string{"cmd.exe"}, nil)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("defaultSpawnHost error = %v, want context.Canceled", err)
		}
	})

	t.Run("start failure", func(t *testing.T) {
		missingCWD := filepath.Join(t.TempDir(), "missing")
		_, _, err := defaultSpawnHost(context.Background(), "start-failure", missingCWD, []string{"cmd.exe"}, nil)
		if err == nil || !strings.Contains(err.Error(), "start") {
			t.Fatalf("defaultSpawnHost error = %v, want start failure", err)
		}
	})

	t.Run("child exits before ready", func(t *testing.T) {
		pidFile := filepath.Join(t.TempDir(), "pid")
		result := spawnHostAsync(context.Background(), "early-exit", t.TempDir(), helperEnv("exit-with-diagnostic", pidFile))
		helper := captureHelper(t, pidFile)
		err := (<-result).err
		if err == nil || !strings.Contains(err.Error(), "deterministic startup failure") {
			t.Fatalf("defaultSpawnHost error = %v, want captured diagnostic", err)
		}
		assertHelperExited(t, helper)
	})

	t.Run("ready identity mismatch", func(t *testing.T) {
		pidFile := filepath.Join(t.TempDir(), "pid")
		result := spawnHostAsync(context.Background(), "wrong-pid", t.TempDir(), helperEnv("wrong-ready-pid", pidFile))
		helper := captureHelper(t, pidFile)
		err := (<-result).err
		if err == nil || !strings.Contains(err.Error(), "does not match") {
			t.Fatalf("defaultSpawnHost error = %v, want READY pid mismatch", err)
		}
		assertHelperExited(t, helper)
	})

	t.Run("startup timeout", func(t *testing.T) {
		withSpawnReadyTimeout(t, 2*time.Second)
		pidFile := filepath.Join(t.TempDir(), "pid")
		result := spawnHostAsync(context.Background(), "timeout", t.TempDir(), helperEnv("hang", pidFile))
		helper := captureHelper(t, pidFile)
		err := (<-result).err
		if err == nil || !strings.Contains(err.Error(), "startup timeout") {
			t.Fatalf("defaultSpawnHost error = %v, want timeout", err)
		}
		assertHelperExited(t, helper)
	})

	t.Run("context cancellation", func(t *testing.T) {
		pidFile := filepath.Join(t.TempDir(), "pid")
		cwd := t.TempDir()
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() {
			_, _, err := defaultSpawnHost(ctx, "cancel", cwd, []string{"cmd.exe"}, helperEnv("hang", pidFile))
			result <- err
		}()
		helper := captureHelper(t, pidFile)
		cancel()
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatalf("defaultSpawnHost error = %v, want context.Canceled", err)
		}
		assertHelperExited(t, helper)
	})
}

func TestDefaultSpawnHostTransfersOwnershipOnSuccess(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	resultC := spawnHostAsync(context.Background(), "success", t.TempDir(), helperEnv("ready", pidFile))
	helper := captureHelper(t, pidFile)
	result := <-resultC
	if result.err != nil {
		t.Fatal(result.err)
	}
	if result.addr != "127.0.0.1:43210" || result.pid != helper.pid {
		t.Fatalf("defaultSpawnHost = %q, %d", result.addr, result.pid)
	}
	if !helperAlive(helper) {
		t.Fatal("successful launch was closed instead of transferring ownership")
	}
}

func TestDefaultSpawnHostRepeatedStartFailuresDoNotLeakHandles(t *testing.T) {
	missingCWD := filepath.Join(t.TempDir(), "missing")
	// Warm lazy os/exec and Windows DLL state before measuring repeated calls.
	_, _, _ = defaultSpawnHost(context.Background(), "warmup", missingCWD, []string{"cmd.exe"}, nil)
	before := currentProcessHandleCount(t)
	for i := 0; i < 32; i++ {
		if _, _, err := defaultSpawnHost(context.Background(), fmt.Sprintf("repeat-%d", i), missingCWD, []string{"cmd.exe"}, nil); err == nil {
			t.Fatalf("iteration %d unexpectedly succeeded", i)
		}
	}
	after := currentProcessHandleCount(t)
	if after > before+maxHandleNoise {
		t.Fatalf("caller handle count grew across repeated setup failures: before=%d after=%d", before, after)
	}
}

func TestNewConPTYRepeatedStartFailuresDoNotLeakHandles(t *testing.T) {
	before := currentProcessHandleCount(t)
	missingCommand := filepath.Join(t.TempDir(), "missing-agent.exe")
	for i := 0; i < 32; i++ {
		if _, err := newConPTY(t.TempDir(), missingCommand, nil); err == nil {
			t.Fatalf("iteration %d unexpectedly succeeded", i)
		}
	}
	waitForHandleCount(t, before+maxHandleNoise)
}

func TestCleanupFailedHostLaunchClosesPipeAndWaitsProcess(t *testing.T) {
	cmd := exec.Command("cmd.exe", "/D", "/Q", "/K")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	cleanupFailedHostLaunch(cmd, stdout)
	if cmd.ProcessState == nil {
		t.Fatal("failed launch process was not waited")
	}
	if _, err := stdout.Read(make([]byte, 1)); err == nil {
		t.Fatal("failed launch stdout pipe remained open")
	}
}

func helperEnv(mode, pidFile string) map[string]string {
	return map[string]string{spawnHelperModeEnv: mode, spawnHelperPIDEnv: pidFile}
}

type spawnHostResult struct {
	addr string
	pid  int
	err  error
}

func spawnHostAsync(ctx context.Context, sessionID, cwd string, env map[string]string) <-chan spawnHostResult {
	result := make(chan spawnHostResult, 1)
	go func() {
		addr, pid, err := defaultSpawnHost(ctx, sessionID, cwd, []string{"cmd.exe"}, env)
		result <- spawnHostResult{addr: addr, pid: pid, err: err}
	}()
	return result
}

func withSpawnReadyTimeout(t *testing.T, timeout time.Duration) {
	t.Helper()
	original := spawnReadyTimeout
	spawnReadyTimeout = timeout
	t.Cleanup(func() { spawnReadyTimeout = original })
}

type retainedHelper struct {
	pid    int
	handle windows.Handle
}

func captureHelper(t *testing.T, path string) retainedHelper {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(string(data))
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			handle, openErr := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_TERMINATE, false, uint32(pid))
			if openErr != nil {
				t.Fatalf("retain helper process %d: %v", pid, openErr)
			}
			helper := retainedHelper{pid: pid, handle: handle}
			t.Cleanup(func() {
				if helperAlive(helper) {
					_ = windows.TerminateProcess(helper.handle, 1)
				}
				_ = windows.CloseHandle(helper.handle)
			})
			return helper
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("helper pid file %q was not created", path)
	return retainedHelper{}
}

func helperAlive(helper retainedHelper) bool {
	result, err := windows.WaitForSingleObject(helper.handle, 0)
	return err == nil && result == uint32(windows.WAIT_TIMEOUT)
}

func assertHelperExited(t *testing.T, helper retainedHelper) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !helperAlive(helper) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("failed launch helper pid %d remained alive", helper.pid)
}

func currentProcessHandleCount(t *testing.T) uint32 {
	t.Helper()
	var count uint32
	proc := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetProcessHandleCount")
	ok, _, callErr := proc.Call(uintptr(windows.CurrentProcess()), uintptr(unsafe.Pointer(&count)))
	if ok == 0 {
		t.Fatalf("GetProcessHandleCount: %v", callErr)
	}
	return count
}

func waitForHandleCount(t *testing.T, maximum uint32) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if count := currentProcessHandleCount(t); count <= maximum {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("caller handle count did not return to at most %d; got %d", maximum, currentProcessHandleCount(t))
}
