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
	poll := []unix.PollFd{{Fd: int32(w.fd), Events: unix.POLLIN}}
	n, err := unix.Poll(poll, int(timeout.Milliseconds()))
	if err != nil {
		return err
	}
	if n == 0 || poll[0].Revents&unix.POLLIN == 0 {
		return fmt.Errorf("process identity did not exit within %s", timeout)
	}
	return nil
}

func (w *processExitWatcher) Close() error { return unix.Close(w.fd) }
