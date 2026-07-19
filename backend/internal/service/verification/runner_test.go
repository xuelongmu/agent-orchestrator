package verification

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/processalive"
)

func TestOSRunnerPreservesArgumentBoundaries(t *testing.T) {
	var output bytes.Buffer
	want := []string{"argument with spaces", `quote"kept`, "shell;&text"}
	argv := append([]string{os.Args[0], "-test.run=TestVerificationProcessHelper", "--", "argv"}, want...)
	res, err := testOSRunner().Run(context.Background(), RunSpec{Argv: argv, Dir: t.TempDir(), Env: append(os.Environ(), "GO_WANT_VERIFY_HELPER=1"), Output: &output})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("Run() = %#v, %v; output=%s", res, err, output.String())
	}
	if got := strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n"); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("arguments = %#v, want %#v", got, want)
	}
}

func TestOSRunnerDoesNotLeakGuardianMarkerToTarget(t *testing.T) {
	res, err := testOSRunner().Run(context.Background(), RunSpec{
		Argv:   []string{os.Args[0], "-test.run=TestVerificationProcessHelper", "--", "marker-absent"},
		Dir:    t.TempDir(),
		Env:    append(os.Environ(), "GO_WANT_VERIFY_HELPER=1"),
		Output: io.Discard,
	})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("Run() = %#v, %v", res, err)
	}
}

func TestOSRunnerCancellationKillsDescendant(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := testOSRunner().Run(ctx, RunSpec{Argv: []string{os.Args[0], "-test.run=TestVerificationProcessHelper", "--", "parent", pidFile}, Dir: t.TempDir(), Env: append(os.Environ(), "GO_WANT_VERIFY_HELPER=1"), Output: io.Discard})
		done <- err
	}()
	var pid int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(pidFile)
		if err == nil {
			pid, _ = strconv.Atoi(string(b))
			if pid > 0 {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if pid == 0 {
		cancel()
		t.Fatal("helper child did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runner did not cancel")
	}
	deadline = time.Now().Add(3 * time.Second)
	for processalive.Alive(pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if processalive.Alive(pid) {
		t.Fatalf("descendant pid %d survived cancellation", pid)
	}
}

func TestOSRunnerFastDetachedParentCannotEscapeContainment(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("Darwin cannot provide race-free post-reap process-group ownership")
	}
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	res, err := testOSRunner().Run(context.Background(), RunSpec{
		Argv:   []string{os.Args[0], "-test.run=TestVerificationProcessHelper", "--", "fast-parent", pidFile},
		Dir:    t.TempDir(),
		Env:    append(os.Environ(), "GO_WANT_VERIFY_HELPER=1"),
		Output: io.Discard,
	})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("Run() = %#v, %v", res, err)
	}
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(b))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for processalive.Alive(pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if processalive.Alive(pid) {
		t.Fatalf("detached descendant pid %d survived fast parent exit", pid)
	}
}

func testOSRunner() OSRunner {
	return OSRunner{HostArgv: []string{os.Args[0], "-test.run=TestVerificationGuardianHelper"}}
}

func TestVerificationGuardianHelper(t *testing.T) {
	if code, handled := RunHostFromEnvironment(); handled {
		os.Exit(code)
	}
}

func TestVerificationProcessHelper(t *testing.T) {
	if os.Getenv("GO_WANT_VERIFY_HELPER") != "1" {
		return
	}
	idx := -1
	for i, arg := range os.Args {
		if arg == "--" {
			idx = i
			break
		}
	}
	if idx < 0 || idx+1 >= len(os.Args) {
		os.Exit(90)
	}
	switch os.Args[idx+1] {
	case "argv":
		for _, arg := range os.Args[idx+2:] {
			_, _ = fmt.Fprintln(os.Stdout, arg)
		}
		os.Exit(0)
	case "parent":
		cmd := exec.Command(os.Args[0], "-test.run=TestVerificationProcessHelper", "--", "child")
		cmd.Env = os.Environ()
		if err := cmd.Start(); err != nil {
			os.Exit(91)
		}
		if err := os.WriteFile(os.Args[idx+2], []byte(strconv.Itoa(cmd.Process.Pid)), 0o600); err != nil {
			os.Exit(92)
		}
		_ = cmd.Wait()
		os.Exit(0)
	case "fast-parent":
		cmd := exec.Command(os.Args[0], "-test.run=TestVerificationProcessHelper", "--", "child")
		cmd.Env = os.Environ()
		configureDetachedChild(cmd)
		if err := cmd.Start(); err != nil {
			os.Exit(91)
		}
		if err := os.WriteFile(os.Args[idx+2], []byte(strconv.Itoa(cmd.Process.Pid)), 0o600); err != nil {
			os.Exit(92)
		}
		os.Exit(0)
	case "setsid-parent", "setsid-fast-parent":
		cmd := exec.Command(os.Args[0], "-test.run=TestVerificationProcessHelper", "--", "setsid-child", os.Args[idx+2])
		cmd.Env = os.Environ()
		if err := cmd.Start(); err != nil {
			os.Exit(91)
		}
		if os.Args[idx+1] == "setsid-fast-parent" {
			if !waitForHelperFile(os.Args[idx+2], 5*time.Second) {
				_ = cmd.Process.Kill()
				os.Exit(95)
			}
			os.Exit(0)
		}
		_ = cmd.Wait()
		os.Exit(0)
	case "setsid-child":
		if err := detachCurrentProcessSession(); err != nil {
			os.Exit(96)
		}
		if err := os.WriteFile(os.Args[idx+2], []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
			os.Exit(92)
		}
		for {
			time.Sleep(time.Second)
		}
	case "inherited-output-parent":
		cmd := exec.Command(os.Args[0], "-test.run=TestVerificationProcessHelper", "--", "child")
		cmd.Env = os.Environ()
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		configureDetachedChild(cmd)
		if err := cmd.Start(); err != nil {
			os.Exit(91)
		}
		if err := os.WriteFile(os.Args[idx+2], []byte(strconv.Itoa(cmd.Process.Pid)), 0o600); err != nil {
			os.Exit(92)
		}
		os.Exit(0)
	case "inherited-output-gated-parent":
		cmd := exec.Command(os.Args[0], "-test.run=TestVerificationProcessHelper", "--", "child")
		cmd.Env = os.Environ()
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Start(); err != nil {
			os.Exit(91)
		}
		if err := os.WriteFile(os.Args[idx+2], []byte(strconv.Itoa(cmd.Process.Pid)), 0o600); err != nil {
			os.Exit(92)
		}
		if !waitForHelperFile(os.Args[idx+3], 5*time.Second) {
			_ = cmd.Process.Kill()
			os.Exit(95)
		}
		os.Exit(0)
	case "outer-runner":
		_, _ = testOSRunner().Run(context.Background(), RunSpec{
			Argv:   []string{os.Args[0], "-test.run=TestVerificationProcessHelper", "--", "parent", os.Args[idx+2]},
			Dir:    os.TempDir(),
			Env:    os.Environ(),
			Output: io.Discard,
		})
		os.Exit(0)
	case "outer-runner-setsid":
		_, _ = testOSRunner().Run(context.Background(), RunSpec{
			Argv:   []string{os.Args[0], "-test.run=TestVerificationProcessHelper", "--", "setsid-parent", os.Args[idx+2]},
			Dir:    os.TempDir(),
			Env:    os.Environ(),
			Output: io.Discard,
		})
		os.Exit(0)
	case "marker-absent":
		if _, ok := os.LookupEnv(verifyHostEnv); ok {
			os.Exit(94)
		}
		os.Exit(0)
	case "chatty":
		payload := strings.Repeat("output", 16*1024)
		for {
			_, _ = io.WriteString(os.Stdout, payload)
		}
	case "child":
		for {
			time.Sleep(time.Second)
		}
	default:
		_, _ = fmt.Fprintln(os.Stderr, "bad helper mode")
		os.Exit(93)
	}
}

func waitForHelperFile(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}
