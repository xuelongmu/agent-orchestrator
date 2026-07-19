package verification

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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
	case "child":
		for {
			time.Sleep(time.Second)
		}
	default:
		_, _ = fmt.Fprintln(os.Stderr, "bad helper mode")
		os.Exit(93)
	}
}
