//go:build darwin

package launchagenthandoff

import (
	"bufio"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestRunDeliversEnvironmentOverProtectedSocket(t *testing.T) {
	socketPath := shortSocketPath(t)
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	result := make(chan error, 1)
	go func() {
		result <- Run(context.Background(), inputReader, outputWriter)
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
	if err := Run(context.Background(), input, &output); err != nil {
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
	if err := Run(context.Background(), strings.NewReader("{}\n"), &output); err != nil {
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
