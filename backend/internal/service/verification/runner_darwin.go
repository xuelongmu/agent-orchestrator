//go:build darwin

package verification

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

func verificationSysProcAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }

// Darwin has no public, unprivileged equivalent of a Linux child subreaper or
// Windows Job Object. In particular, XNU rejects EVFILT_PROC NOTE_TRACK, so a
// guardian cannot race-freely retain ownership after a descendant reparents.
// Keep the process-group guarantee and make the stronger limitation explicit
// in docs/verification.md and the Darwin regression tests.
type darwinVerificationDescendantOwner struct{}

func newVerificationDescendantOwner() (*darwinVerificationDescendantOwner, error) {
	return &darwinVerificationDescendantOwner{}, nil
}

func (*darwinVerificationDescendantOwner) Close() error { return nil }

// waitVerificationProcess observes target exit without reaping it. The target
// PID is also its PGID, so retaining the zombie leader retains the kernel
// identity needed to signal ordinary in-group descendants without risking PGID
// reuse. Both natural completion and owner cancellation kill the group before
// cmd.Wait releases that identity.
func waitVerificationProcess(cmd *exec.Cmd, owner io.Reader) (error, error) {
	kq, err := unix.Kqueue()
	if err != nil {
		killVerificationProcessGroup(cmd.Process.Pid)
		return cmd.Wait(), fmt.Errorf("create target exit kqueue: %w", err)
	}
	defer func() { _ = unix.Close(kq) }()

	change := []unix.Kevent_t{{
		Ident:  uint64(cmd.Process.Pid),
		Filter: unix.EVFILT_PROC,
		Flags:  unix.EV_ADD | unix.EV_ONESHOT,
		Fflags: unix.NOTE_EXIT,
	}}
	if _, err = unix.Kevent(kq, change, nil, nil); err != nil {
		killVerificationProcessGroup(cmd.Process.Pid)
		return cmd.Wait(), fmt.Errorf("watch target exit: %w", err)
	}

	exited := make(chan error, 1)
	go func() {
		events := make([]unix.Kevent_t, 1)
		for {
			n, eventErr := unix.Kevent(kq, nil, events, nil)
			if errors.Is(eventErr, unix.EINTR) {
				continue
			}
			if eventErr == nil && n == 0 {
				continue
			}
			if eventErr == nil && events[0].Flags&unix.EV_ERROR != 0 {
				eventErr = syscall.Errno(events[0].Data)
			}
			exited <- eventErr
			return
		}
	}()

	var observeErr error
	select {
	case observeErr = <-exited:
		killVerificationProcessGroup(cmd.Process.Pid)
	case <-ownershipCanceled(owner):
		killVerificationProcessGroup(cmd.Process.Pid)
		observeErr = <-exited
	}
	waitErr := cmd.Wait()
	if observeErr != nil {
		return waitErr, fmt.Errorf("observe target exit: %w", observeErr)
	}
	return waitErr, nil
}

func (*darwinVerificationDescendantOwner) Terminate(targetPID int) error {
	// Same-PGID cleanup happens before target reap in waitVerificationProcess.
	// Detached descendants are intentionally outside the supported guarantee.
	return nil
}
