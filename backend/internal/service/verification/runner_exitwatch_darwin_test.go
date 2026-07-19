//go:build darwin

package verification

import (
	"fmt"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type processExitWatcher struct {
	kq int
}

func newProcessExitWatcher(pid int) (*processExitWatcher, error) {
	kq, err := unix.Kqueue()
	if err != nil {
		return nil, err
	}
	change := []unix.Kevent_t{{
		Ident:  uint64(pid),
		Filter: unix.EVFILT_PROC,
		Flags:  unix.EV_ADD | unix.EV_ONESHOT,
		Fflags: unix.NOTE_EXIT,
	}}
	if _, err = unix.Kevent(kq, change, nil, nil); err != nil {
		_ = unix.Close(kq)
		return nil, err
	}
	return &processExitWatcher{kq: kq}, nil
}

func (w *processExitWatcher) Wait(timeout time.Duration) error {
	events := make([]unix.Kevent_t, 1)
	timespec := unix.NsecToTimespec(timeout.Nanoseconds())
	n, err := unix.Kevent(w.kq, nil, events, &timespec)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("process identity did not exit within %s", timeout)
	}
	if events[0].Flags&unix.EV_ERROR != 0 {
		return syscall.Errno(events[0].Data)
	}
	return nil
}

func (w *processExitWatcher) Close() error { return unix.Close(w.kq) }
