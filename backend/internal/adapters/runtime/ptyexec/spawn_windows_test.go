//go:build windows

package ptyexec

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"golang.org/x/sys/windows"
)

func TestSpawnWindowsStreamsOutput(t *testing.T) {
	p, err := Spawn(context.Background(), []string{"cmd.exe", "/D", "/Q", "/K"}, nil, 24, 80)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer p.Close()
	if _, err := p.Write([]byte("echo AO_CONPTY_OK\r\n")); err != nil {
		t.Fatalf("write PTY: %v", err)
	}

	out := readPTYUntil(t, p, "AO_CONPTY_OK", 5*time.Second)
	if !strings.Contains(out, "AO_CONPTY_OK") {
		t.Fatalf("output %q does not contain marker", out)
	}
}

func TestSpawnWindowsRejectsEmptyCommand(t *testing.T) {
	_, err := Spawn(context.Background(), nil, nil, 0, 0)
	if err == nil || !strings.Contains(err.Error(), "empty attach command") {
		t.Fatalf("expected empty attach command error, got %v", err)
	}
}

func TestSpawnWindowsRejectsCanceledContextBeforeAllocatingPTY(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Spawn(ctx, []string{"cmd.exe", "/C", "exit", "0"}, nil, 0, 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Spawn error = %v, want context.Canceled", err)
	}
}

func TestSpawnWindowsRepeatedStartFailuresReleaseConPTYHandles(t *testing.T) {
	missingCommand := filepath.Join(t.TempDir(), "missing-attach.exe")
	_, _ = Spawn(context.Background(), []string{missingCommand}, nil, 24, 80)
	before := processHandleCount(t)
	for i := 0; i < 32; i++ {
		if _, err := Spawn(context.Background(), []string{missingCommand}, nil, 24, 80); err == nil {
			t.Fatalf("iteration %d unexpectedly succeeded", i)
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if count := processHandleCount(t); count <= before+16 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("caller handle count grew across repeated attach setup failures: before=%d after=%d", before, processHandleCount(t))
}

func TestConPTYCloseIsIdempotent(t *testing.T) {
	p, err := Spawn(context.Background(), []string{"cmd.exe", "/D", "/Q", "/K"}, nil, 24, 80)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	done := make(chan struct{})
	go func() {
		_ = p.Close()
		_ = p.Close()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not return")
	}
}

func readPTYUntil(t *testing.T, p ports.Stream, marker string, timeout time.Duration) string {
	t.Helper()
	type result struct {
		out string
		err error
	}
	results := make(chan result, 1)
	go func() {
		var buf bytes.Buffer
		tmp := make([]byte, 4096)
		for {
			n, err := p.Read(tmp)
			if n > 0 {
				buf.Write(tmp[:n])
				if strings.Contains(buf.String(), marker) {
					results <- result{out: buf.String()}
					return
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					results <- result{out: buf.String()}
				} else {
					results <- result{out: buf.String(), err: err}
				}
				return
			}
		}
	}()

	select {
	case res := <-results:
		if res.err != nil {
			t.Fatalf("read PTY: %v (output %q)", res.err, res.out)
		}
		return res.out
	case <-time.After(timeout):
		_ = p.Close()
		t.Fatal("timed out reading PTY output")
		return ""
	}
}

func processHandleCount(t *testing.T) uint32 {
	t.Helper()
	var count uint32
	proc := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetProcessHandleCount")
	ok, _, callErr := proc.Call(uintptr(windows.CurrentProcess()), uintptr(unsafe.Pointer(&count)))
	if ok == 0 {
		t.Fatalf("GetProcessHandleCount: %v", callErr)
	}
	return count
}
