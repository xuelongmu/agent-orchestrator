//go:build linux

package verification

import (
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

type processExitWatcher struct {
	fd int
}

func newProcessExitWatcher(pid int) (*processExitWatcher, error) {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return nil, err
	}
	return &processExitWatcher{fd: fd}, nil
}

func (w *processExitWatcher) Wait(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining < 0 {
			remaining = 0
		}
		poll := []unix.PollFd{{Fd: int32(w.fd), Events: unix.POLLIN}}
		n, err := unix.Poll(poll, int(remaining.Milliseconds()))
		if err == unix.EINTR {
			// Go runtime signals (preemption, GC) interrupt the syscall; retry
			// with the remaining time until the deadline.
			continue
		}
		if err != nil {
			return err
		}
		if n == 0 || poll[0].Revents&unix.POLLIN == 0 {
			return fmt.Errorf("process identity did not exit within %s", timeout)
		}
		return nil
	}
}

func (w *processExitWatcher) Close() error { return unix.Close(w.fd) }
