//go:build darwin

package launchagenthandoff

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ao-handoff-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "env.sock")
}

func lockPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "environment.lock")
}

func TestRunDeliversEnvironmentOverProtectedSocket(t *testing.T) {
	socketPath := shortSocketPath(t)
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	result := make(chan error, 1)
	go func() {
		result <- Run(context.Background(), inputReader, outputWriter, lockPath(t))
		_ = outputWriter.Close()
	}()
	if _, err := io.WriteString(
		inputWriter,
		`{"socket_path":"`+socketPath+`","environment":{"GH_TOKEN":"secret","OPENAI_API_KEY":"api"}}`+"\n",
	); err != nil {
		t.Fatal(err)
	}

	output := bufio.NewReader(outputReader)
	if line, err := output.ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("readiness = %q, %v", line, err)
	}
	info, err := os.Stat(socketPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %o, want 600", info.Mode().Perm())
	}
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	for _, expected := range []string{"GH_TOKEN=secret\x00", "OPENAI_API_KEY=api\x00", "AO_HANDOFF_COMPLETE=1\x00"} {
		if !strings.Contains(string(payload), expected) {
			t.Fatalf("payload %q does not contain %q", payload, expected)
		}
	}
	if line, err := output.ReadString('\n'); err != nil || line != "delivered\n" {
		t.Fatalf("delivery = %q, %v", line, err)
	}
	_, _ = io.WriteString(inputWriter, "release\n")
	_ = inputWriter.Close()
	if err := <-result; err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if _, err := os.Stat(socketPath); !os.IsNotExist(err) {
		t.Fatalf("socket remained after release: %v", err)
	}
}

func TestRunRemovesSocketWhenElectronPipeCloses(t *testing.T) {
	socketPath := shortSocketPath(t)
	input := strings.NewReader(`{"socket_path":"` + socketPath + `","environment":{"GH_TOKEN":"secret"}}` + "\n")
	var output strings.Builder
	if err := Run(context.Background(), input, &output, lockPath(t)); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if output.String() != "ready\n" {
		t.Fatalf("output = %q, want ready", output.String())
	}
	if _, err := os.Stat(socketPath); !os.IsNotExist(err) {
		t.Fatalf("socket remained after parent pipe closed: %v", err)
	}
}

func TestRunLockOnlyDoesNotCreateSocket(t *testing.T) {
	var output strings.Builder
	if err := Run(context.Background(), strings.NewReader("{}\n"), &output, lockPath(t)); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if output.String() != "ready\n" {
		t.Fatalf("output = %q, want ready", output.String())
	}
}

func TestPrepareEnvironmentSocketRefusesRegularFile(t *testing.T) {
	socketPath := shortSocketPath(t)
	if err := os.WriteFile(socketPath, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := prepareEnvironmentSocket(socketPath, map[string]string{}); err == nil {
		t.Fatal("prepareEnvironmentSocket() succeeded for a regular file")
	}
	content, err := os.ReadFile(socketPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "keep" {
		t.Fatalf("regular file content = %q", content)
	}
}

func TestAcquireLockExcludesAConcurrentHandoffUntilReleased(t *testing.T) {
	path := lockPath(t)
	release, err := acquireLock(context.Background(), path, time.Second)
	if err != nil {
		t.Fatalf("first acquireLock() error = %v", err)
	}
	if _, err := acquireLock(context.Background(), path, 100*time.Millisecond); err == nil {
		t.Fatal("second acquireLock() succeeded while the lock was held")
	} else if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("second acquireLock() error = %v, want a timeout", err)
	}

	release()

	second, err := acquireLock(context.Background(), path, time.Second)
	if err != nil {
		t.Fatalf("acquireLock() after release error = %v", err)
	}
	second()
}

func TestAcquireLockCreatesAProtectedLockFileAndDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "launchd", "environment.lock")
	release, err := acquireLock(context.Background(), path, time.Second)
	if err != nil {
		t.Fatalf("acquireLock() error = %v", err)
	}
	defer release()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("lock mode = %o, want 600", info.Mode().Perm())
	}
}

func TestAcquireLockRequiresAPath(t *testing.T) {
	if _, err := acquireLock(context.Background(), "", time.Second); err == nil {
		t.Fatal("acquireLock() succeeded without a path")
	}
}

func TestAcquireLockStopsWaitingWhenTheContextIsCancelled(t *testing.T) {
	path := lockPath(t)
	release, err := acquireLock(context.Background(), path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := acquireLock(ctx, path, time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatalf("acquireLock() error = %v, want context.Canceled", err)
	}
}

func TestRunHoldsTheLockForItsDurationAndReleasesIt(t *testing.T) {
	path := lockPath(t)
	socketPath := shortSocketPath(t)
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	result := make(chan error, 1)
	go func() {
		result <- Run(context.Background(), inputReader, outputWriter, path)
		_ = outputWriter.Close()
	}()
	if _, err := io.WriteString(
		inputWriter,
		`{"socket_path":"`+socketPath+`","environment":{"GH_TOKEN":"secret"}}`+"\n",
	); err != nil {
		t.Fatal(err)
	}
	output := bufio.NewReader(outputReader)
	if line, err := output.ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("readiness = %q, %v", line, err)
	}

	if _, err := acquireLock(context.Background(), path, 100*time.Millisecond); err == nil {
		t.Fatal("acquireLock() succeeded while Run held the lock")
	}

	_, _ = io.WriteString(inputWriter, "release\n")
	_ = inputWriter.Close()
	if err := <-result; err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	release, err := acquireLock(context.Background(), path, time.Second)
	if err != nil {
		t.Fatalf("acquireLock() after Run returned error = %v", err)
	}
	release()
}
