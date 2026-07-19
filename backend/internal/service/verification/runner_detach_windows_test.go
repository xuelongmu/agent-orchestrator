//go:build windows

package verification

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/processalive"
	"golang.org/x/sys/windows"
)

func configureDetachedChild(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
		HideWindow:    true,
	}
}

func TestWindowsNPMTargetUsesNodeWithoutCommandShell(t *testing.T) {
	argv, err := prepareWindowsTarget([]string{"npm", "--version"})
	if err != nil {
		t.Skipf("npm is unavailable: %v", err)
	}
	if strings.ToLower(filepath.Base(argv[0])) != "node.exe" || filepath.Base(argv[1]) != "npm-cli.js" {
		t.Fatalf("resolved argv = %#v", argv)
	}
	for _, arg := range argv {
		if strings.EqualFold(filepath.Base(arg), "cmd.exe") || strings.EqualFold(filepath.Base(arg), "cmd") {
			t.Fatalf("resolution introduced command shell: %#v", argv)
		}
	}

	ownerRead, ownerWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = ownerRead.Close()
		_ = ownerWrite.Close()
	}()
	var output bytes.Buffer
	if code := runHostedProcess([]string{"npm", "--version"}, ownerRead, &output, &output); code != 0 {
		t.Fatalf("npm execution exit=%d output=%s", code, output.String())
	}
	if strings.TrimSpace(output.String()) == "" {
		t.Fatal("npm execution returned no version")
	}
}

func TestWindowsInheritedOutputDescendantDoesNotDelayJobClose(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := testOSRunner().Run(ctx, RunSpec{
		Argv:   []string{os.Args[0], "-test.run=TestVerificationProcessHelper", "--", "inherited-output-parent", pidFile},
		Dir:    t.TempDir(),
		Env:    append(os.Environ(), "GO_WANT_VERIFY_HELPER=1"),
		Output: io.Discard,
	})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("Run() = %#v, %v", result, err)
	}
	body, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(body))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for processalive.Alive(pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if processalive.Alive(pid) {
		t.Fatalf("inherited-output descendant pid %d survived Job close", pid)
	}
}
