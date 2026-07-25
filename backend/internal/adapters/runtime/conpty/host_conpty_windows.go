//go:build windows

package conpty

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
	"time"

	gopty "github.com/aymanbagabas/go-pty"
	"golang.org/x/sys/windows"

	"github.com/aoagents/agent-orchestrator/backend/internal/process"
)

// conptyConn is the real ptyConn implementation backed by go-pty's ConPty
// (Windows ConPTY API). Only compiled on Windows.
type conptyConn struct {
	pty gopty.ConPty
	cmd *gopty.Cmd
	job *process.SessionJob

	once     sync.Once
	doneC    chan struct{}
	exitCode int
	exited   bool
	exitMu   sync.Mutex
}

// newConPTY creates a ConPTY session running shellCmd in cwd with shellArgs.
// It starts the process and returns a ptyConn ready for use.
func newConPTY(cwd, shellCmd string, shellArgs []string, job *process.SessionJob) (ptyConn, error) {
	// go-pty's New() returns a ConPty on Windows.
	p, err := gopty.New()
	if err != nil {
		return nil, fmt.Errorf("conpty: create pty: %w", err)
	}
	cp, ok := p.(gopty.ConPty)
	if !ok {
		_ = p.Close()
		return nil, fmt.Errorf("conpty: expected ConPty on windows, got %T", p)
	}
	owned := true
	defer func() {
		if owned {
			_ = cp.Close()
		}
	}()

	// Set an initial size matching node-pty defaults from pty-host.ts.
	if err := cp.Resize(220, 50); err != nil {
		return nil, fmt.Errorf("conpty: initial resize: %w", err)
	}

	cmd := cp.Command(shellCmd, shellArgs...)
	cmd.Dir = cwd
	// Inherit parent env so PATH, HOME, etc. are available.
	cmd.Env = os.Environ()
	// Prevent the shell/agent from creating an escaping descendant between
	// process creation and Job Object assignment.
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_SUSPENDED}

	if err := cmd.Start(); err != nil {
		cleanupConPTYCommand(cmd)
		return nil, fmt.Errorf("conpty: start command: %w", err)
	}
	if job == nil {
		cleanupConPTYCommand(cmd)
		return nil, errors.New("conpty: session job required")
	}
	if err := job.Assign(cmd.Process.Pid); err != nil {
		cleanupConPTYCommand(cmd)
		return nil, fmt.Errorf("conpty: assign command to session job: %w", err)
	}
	if err := process.ResumeSuspendedProcess(cmd.Process.Pid); err != nil {
		cleanupConPTYCommand(cmd)
		return nil, fmt.Errorf("conpty: resume command: %w", err)
	}

	c := &conptyConn{
		pty:   cp,
		cmd:   cmd,
		job:   job,
		doneC: make(chan struct{}),
	}

	go c.wait()
	owned = false
	return c, nil
}

func (c *conptyConn) wait() {
	_ = c.cmd.Wait()
	code := 0
	if c.cmd.ProcessState != nil {
		code = c.cmd.ProcessState.ExitCode()
	}
	c.exitMu.Lock()
	c.exitCode = code
	c.exited = true
	c.exitMu.Unlock()
	c.once.Do(func() { close(c.doneC) })
}

func (c *conptyConn) Read(b []byte) (int, error)  { return c.pty.Read(b) }
func (c *conptyConn) Write(b []byte) (int, error) { return c.pty.Write(b) }
func (c *conptyConn) Close() error {
	ptyErr := c.pty.Close()
	// Terminate the entire session tree, including grandchildren. The bounded
	// wait makes Done and daemon-side workspace cleanup observe a dead tree.
	jobCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	jobErr := c.job.TerminateAndWait(jobCtx)
	if c.cmd.Process != nil && jobErr != nil {
		_ = c.cmd.Process.Kill()
	}
	return errors.Join(ptyErr, jobErr)
}
func (c *conptyConn) Resize(cols, rows int) error { return c.pty.Resize(cols, rows) }
func (c *conptyConn) Done() <-chan struct{}       { return c.doneC }
func (c *conptyConn) PID() int {
	if c.cmd.Process == nil {
		return 0
	}
	return c.cmd.Process.Pid
}
func (c *conptyConn) ExitCode() (int, bool) {
	c.exitMu.Lock()
	defer c.exitMu.Unlock()
	return c.exitCode, c.exited
}

func cleanupConPTYCommand(cmd *gopty.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
}
