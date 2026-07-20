//go:build linux

package tmux

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// platformProcessIdentity combines the kernel SID with /proc's start-time
// tick. The latter is the kernel process generation, not ps's second-resolution
// rendered timestamp.
func platformProcessIdentity(pid int) (processIdentity, error) {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return processIdentity{}, fmt.Errorf("open pidfd for %d: %w", pid, err)
	}
	defer func() { _ = unix.Close(fd) }()
	return linuxIdentityWithPIDFD(pid, fd)
}

func linuxIdentityWithPIDFD(pid, fd int) (processIdentity, error) {
	sid, err := platformProcessSessionID(pid)
	if err != nil {
		return processIdentity{}, err
	}
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)) // #nosec G304 -- pid is an integer from ps/kernel APIs.
	if err != nil {
		return processIdentity{}, fmt.Errorf("read process %d generation: %w", pid, err)
	}
	closeParen := strings.LastIndexByte(string(raw), ')')
	if closeParen < 0 {
		return processIdentity{}, fmt.Errorf("read process %d generation: invalid stat", pid)
	}
	fields := strings.Fields(string(raw[closeParen+1:]))
	// fields begins at proc(5) field 3 (state); starttime is field 22.
	if len(fields) <= 19 {
		return processIdentity{}, fmt.Errorf("read process %d generation: short stat", pid)
	}
	if _, err := strconv.ParseUint(fields[19], 10, 64); err != nil {
		return processIdentity{}, fmt.Errorf("read process %d generation: %w", pid, err)
	}
	// A pidfd zero-signal after both numeric-PID reads proves they referred to
	// the process retained by fd. If it exited or the number was reused, fail.
	if err := unix.PidfdSendSignal(fd, 0, nil, 0); err != nil {
		return processIdentity{}, fmt.Errorf("validate pidfd for %d: %w", pid, err)
	}
	return processIdentity{pid: pid, sessionID: sid, started: fields[19]}, nil
}

// Signal binds delivery to a pidfd, making PID reuse between validation and
// delivery harmless: the kernel signals only the retained process generation.
func (osProcessSignaler) Signal(ctx context.Context, expected processIdentity, signal os.Signal) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	fd, err := unix.PidfdOpen(expected.pid, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	current, err := linuxIdentityWithPIDFD(expected.pid, fd)
	if err != nil {
		return err
	}
	if current != expected {
		return fmt.Errorf("process identity changed")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	sig, ok := signal.(syscall.Signal)
	if !ok {
		return fmt.Errorf("unsupported signal %T", signal)
	}
	return unix.PidfdSendSignal(fd, sig, nil, 0)
}
