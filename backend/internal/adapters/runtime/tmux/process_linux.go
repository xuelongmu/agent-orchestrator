//go:build linux

package tmux

import (
	"context"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

type linuxProcessHandle struct {
	fd     int
	closed bool
}

// platformOpenProcess opens the pidfd before reading any numeric-PID metadata
// and returns that exact fd to the caller. The fd remains the delivery target;
// /proc starttime is descriptive continuity metadata, never a signal authority.
func platformOpenProcess(pid int) (processObservation, error) {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return processObservation{}, fmt.Errorf("open pidfd for %d: %w", pid, err)
	}
	identity, err := linuxIdentityWithPIDFD(pid, fd)
	if err != nil {
		_ = unix.Close(fd)
		return processObservation{}, err
	}
	return processObservation{identity: identity, handle: &linuxProcessHandle{fd: fd}}, nil
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

func (h *linuxProcessHandle) Alive(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if h == nil || h.closed || h.fd < 0 {
		return fmt.Errorf("process handle is closed")
	}
	return unix.PidfdSendSignal(h.fd, 0, nil, 0)
}

// Exited observes the retained pidfd itself. Unlike a numeric PID lookup, a
// readable pidfd cannot be redirected to a replacement process after reuse.
func (h *linuxProcessHandle) Exited(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if h == nil || h.closed || h.fd < 0 {
		return false, fmt.Errorf("process handle is closed")
	}
	if h.fd > math.MaxInt32 {
		return false, fmt.Errorf("process handle fd %d exceeds poll range", h.fd)
	}
	// #nosec G115 -- h.fd is nonnegative and bounded by MaxInt32 above.
	pollFD := int32(h.fd)
	fds := []unix.PollFd{{Fd: pollFD, Events: unix.POLLIN}}
	ready, err := unix.Poll(fds, 0)
	if err != nil {
		return false, err
	}
	if ready == 0 {
		return false, nil
	}
	if fds[0].Revents&(unix.POLLIN|unix.POLLHUP) != 0 {
		return true, nil
	}
	if fds[0].Revents&(unix.POLLERR|unix.POLLNVAL) != 0 {
		return false, fmt.Errorf("poll pidfd: revents %#x", fds[0].Revents)
	}
	return false, nil
}

// Signal delivers through the retained pidfd, so numeric PID reuse after
// discovery cannot redirect the signal to another process.
func (h *linuxProcessHandle) Signal(ctx context.Context, signal os.Signal) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if h == nil || h.closed || h.fd < 0 {
		return fmt.Errorf("process handle is closed")
	}
	sig, ok := signal.(syscall.Signal)
	if !ok {
		return fmt.Errorf("unsupported signal %T", signal)
	}
	return unix.PidfdSendSignal(h.fd, sig, nil, 0)
}

func (h *linuxProcessHandle) Close() error {
	if h == nil || h.closed {
		return nil
	}
	h.closed = true
	fd := h.fd
	h.fd = -1
	return unix.Close(fd)
}
