//go:build windows

package gitexclude

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// Acquire locks path until the returned function is called.
func Acquire(path string, onContention func()) (func(), error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // repository-common lock file
	if err != nil {
		return nil, err
	}
	var overlapped windows.Overlapped
	if onContention != nil {
		err = windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped)
		if err == nil {
			return unlock(file, &overlapped), nil
		}
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			_ = file.Close()
			return nil, err
		}
		onContention()
		overlapped = windows.Overlapped{}
	}
	if err := windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &overlapped); err != nil {
		_ = file.Close()
		return nil, err
	}
	return unlock(file, &overlapped), nil
}

func unlock(file *os.File, overlapped *windows.Overlapped) func() {
	return func() {
		_ = windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, overlapped)
		_ = file.Close()
	}
}
