//go:build darwin

package tmux

import (
	"context"
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// platformProcessIdentity reads Darwin's kernel start timeval through sysctl.
// This avoids ps lstart's one-second truncation and distinguishes rapid reuse.
func platformProcessIdentity(pid int) (processIdentity, error) {
	sid, err := platformProcessSessionID(pid)
	if err != nil {
		return processIdentity{}, err
	}
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return processIdentity{}, fmt.Errorf("read process %d generation: %w", pid, err)
	}
	if int(info.Proc.P_pid) != pid {
		return processIdentity{}, fmt.Errorf("read process %d generation: pid changed", pid)
	}
	started := fmt.Sprintf("%d:%d", info.Proc.P_starttime.Sec, info.Proc.P_starttime.Usec)
	return processIdentity{pid: pid, sessionID: sid, started: started}, nil
}

// Signal registers an EVFILT_PROC exit watch before validating the kernel
// generation. Darwin has no pidfd signal API; the kqueue dead-state plus the
// microsecond kernel start token is its strongest unprivileged process handle.
func (osProcessSignaler) Signal(ctx context.Context, expected processIdentity, signal os.Signal) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	kq, err := unix.Kqueue()
	if err != nil {
		return fmt.Errorf("create process kqueue: %w", err)
	}
	defer func() { _ = unix.Close(kq) }()
	change := []unix.Kevent_t{{
		Ident: uint64(expected.pid), Filter: unix.EVFILT_PROC,
		Flags: unix.EV_ADD | unix.EV_ONESHOT, Fflags: unix.NOTE_EXIT,
	}}
	if _, err := unix.Kevent(kq, change, nil, nil); err != nil {
		return fmt.Errorf("watch process %d: %w", expected.pid, err)
	}
	current, err := platformProcessIdentity(expected.pid)
	if err != nil {
		return err
	}
	if current != expected {
		return fmt.Errorf("process identity changed")
	}
	events := make([]unix.Kevent_t, 1)
	zero := unix.Timespec{}
	if n, err := unix.Kevent(kq, nil, events, &zero); err != nil || n != 0 {
		if err != nil {
			return fmt.Errorf("check process %d dead state: %w", expected.pid, err)
		}
		return fmt.Errorf("process %d exited", expected.pid)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	sig, ok := signal.(syscall.Signal)
	if !ok {
		return fmt.Errorf("unsupported signal %T", signal)
	}
	return unix.Kill(expected.pid, sig)
}
