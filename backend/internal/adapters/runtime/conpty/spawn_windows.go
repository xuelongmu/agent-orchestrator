//go:build windows

// spawn_windows.go - real detached pty-host spawner for Windows using
// CREATE_NEW_PROCESS_GROUP + DETACHED_PROCESS so the host survives daemon exit.
package conpty

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/windows"
)

// readyRE matches the "READY:<pid> <port>" line printed by RunHost.
var readyRE = regexp.MustCompile(`READY:(\d+) (\d+)`)

var spawnReadyTimeout = 10 * time.Second

// maxCapturedStderr bounds how much pty-host stderr we retain for diagnostics.
const maxCapturedStderr = 8192

// boundedBuffer is a thread-safe io.Writer that retains up to max bytes of what
// is written and discards the rest. It always consumes its input (never blocks
// or errors), so it is a safe stderr sink for the detached pty-host — matching
// the previous io.Discard behavior while keeping a capped copy so a startup
// failure (e.g. newConPTY) can be reported instead of only "exited without
// printing READY".
type boundedBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := b.max - len(b.buf); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		b.buf = append(b.buf, p[:room]...)
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

// defaultSpawnHost resolves the current executable, builds the pty-host argv,
// and spawns it detached on Windows. It reads stdout for "READY:<pid> <port>"
// with a 10s timeout, then unrefs (detaches) the child. Returns the loopback
// address and the pty-host OS PID.
func defaultSpawnHost(ctx context.Context, sessionID, cwd string, argv []string, env map[string]string) (string, int, error) {
	if err := ctx.Err(); err != nil {
		return "", 0, err
	}
	exe, err := os.Executable()
	if err != nil {
		return "", 0, fmt.Errorf("conpty spawn: resolve executable: %w", err)
	}

	// Translate a leading `env NAME=VALUE ...` prefix into real child env vars.
	// Windows has no `env` binary and the pty-host execs argv[0] directly, so an
	// adapter that emits `env KEY=value <bin>` (e.g. opencode, to set
	// OPENCODE_CONFIG) would otherwise fail with "env: executable file not
	// found". The assignments are added to the pty-host environment below, which
	// the ConPTY child inherits (host_conpty_windows.go passes os.Environ()).
	envAssignments, argv := stripEnvAssignments(argv)

	// Build: <exe> pty-host <sessionID> <cwd> <shellCmd> <shellArgs...>
	args := append([]string{"pty-host", sessionID, cwd}, argv...)

	// Merge using Windows' case-insensitive semantics. Runtime-owned host
	// controls win over ambient, project, and stripped argv assignments.
	merged := buildHostEnvironment(os.Environ(), env, envAssignments)

	// Cancellation is owned by the select/cleanup path below. CommandContext
	// would add an exec.Cmd watcher that normally exits from Wait, but successful
	// detached hosts are deliberately released and never waited by this parent.
	cmd := exec.Command(exe, args...)
	cmd.Dir = cwd
	cmd.Env = merged

	// Windows process-creation flags: detached + hidden console.
	// ponytail: DETACHED_PROCESS puts the child in its own console; without it
	// the child is killed when the parent's console closes. CREATE_NEW_PROCESS_GROUP
	// insulates it from Ctrl+C sent to the parent. windowsHide suppresses the flash.
	cmd.SysProcAttr = &windows.SysProcAttr{
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
		HideWindow:    true,
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", 0, fmt.Errorf("conpty spawn: stdout pipe: %w", err)
	}
	// Capture a bounded copy of the pty-host's stderr. It writes its startup
	// diagnostics there (listen/newConPTY failures) before exiting without
	// printing READY; retaining them lets us report the real cause below. Use an
	// explicit pipe rather than assigning boundedBuffer directly to cmd.Stderr:
	// exec.Cmd otherwise owns a hidden copy goroutine and pipe until Wait, but a
	// successful detached host is released rather than waited and may run for
	// days.
	stderr := &boundedBuffer{max: maxCapturedStderr}
	stderrReader, stderrWriter, err := os.Pipe()
	if err != nil {
		_ = stdout.Close()
		return "", 0, fmt.Errorf("conpty spawn: stderr pipe: %w", err)
	}
	cmd.Stderr = stderrWriter

	if err := cmd.Start(); err != nil {
		_ = stderrWriter.Close()
		cleanupFailedHostLaunch(cmd, stdout)
		_ = stderrReader.Close()
		return "", 0, fmt.Errorf("conpty spawn: start: %w", err)
	}
	// The child has its duplicated write handle now; the parent owns only the
	// read side and closes it as soon as startup succeeds or fails.
	_ = stderrWriter.Close()
	stderrDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(stderr, stderrReader)
		close(stderrDone)
	}()
	// Until READY is accepted and the process reference is deliberately
	// released, this launcher owns the child handle and both startup pipe read
	// ends. Keep one cleanup path for every timeout, cancellation, scanner, and
	// future setup error so detached launch failures cannot leak those resources.
	launchOwned := true
	defer func() {
		if launchOwned {
			cleanupFailedHostLaunch(cmd, stdout)
			_ = stderrReader.Close()
			<-stderrDone
		}
	}()

	// Read READY line with a timeout.
	readyC := make(chan struct {
		addr string
		pid  int
		err  error
	}, 1)

	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			m := readyRE.FindStringSubmatch(line)
			if m != nil {
				pid, _ := strconv.Atoi(m[1])
				port, _ := strconv.Atoi(m[2])
				readyC <- struct {
					addr string
					pid  int
					err  error
				}{"127.0.0.1:" + strconv.Itoa(port), pid, nil}
				return
			}
		}
		<-stderrDone
		msg := "conpty spawn: pty-host exited without printing READY"
		if diag := strings.TrimSpace(stderr.String()); diag != "" {
			msg += ": " + diag
		}
		readyC <- struct {
			addr string
			pid  int
			err  error
		}{"", 0, fmt.Errorf("%s", msg)}
	}()

	timer := time.NewTimer(spawnReadyTimeout)
	defer timer.Stop()

	select {
	case r := <-readyC:
		if r.err != nil {
			return "", 0, r.err
		}
		if r.pid != cmd.Process.Pid {
			return "", 0, fmt.Errorf("conpty spawn: READY host pid %d does not match launched process %d", r.pid, cmd.Process.Pid)
		}
		// Unref: detach stdout so the child is not blocked, then release reference
		// so our process can exit while the child keeps running.
		if err := stdout.Close(); err != nil {
			return "", 0, fmt.Errorf("conpty spawn: close startup pipe: %w", err)
		}
		_ = stderrReader.Close()
		<-stderrDone
		pid := cmd.Process.Pid
		// Release consumes the os.Process reference even if the underlying
		// CloseHandle reports an error. The host is already durably published and
		// controllable at this point, so do not turn that into an unowned launch.
		_ = cmd.Process.Release()
		launchOwned = false
		return r.addr, pid, nil
	case <-timer.C:
		return "", 0, fmt.Errorf("conpty spawn: pty-host startup timeout (%s)", spawnReadyTimeout)
	case <-ctx.Done():
		return "", 0, ctx.Err()
	}
}

func cleanupFailedHostLaunch(cmd *exec.Cmd, stdout io.Closer) {
	_ = stdout.Close()
	if cmd.Process == nil {
		return
	}
	if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		// Waiting after an unsuccessful kill could block forever. Release still
		// closes our process handle; the startup pipe is already closed above.
		_ = cmd.Process.Release()
		return
	}
	// Wait releases exec.Cmd's process handle and any remaining parent-owned
	// pipes. It is required even after Kill or an already-observed child exit.
	_ = cmd.Wait()
}
