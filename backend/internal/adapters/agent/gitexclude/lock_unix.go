//go:build !windows

package gitexclude

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// Acquire locks path until the returned function is called.
func Acquire(path string, onContention func()) (func(), error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // repository-common lock file
	if err != nil {
		return nil, err
	}
	if onContention != nil {
		for {
			err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
			if !errors.Is(err, unix.EINTR) {
				break
			}
		}
		if err == nil {
			return unlock(file), nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			_ = file.Close()
			return nil, err
		}
		onContention()
	}
	for {
		err = unix.Flock(int(file.Fd()), unix.LOCK_EX)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return unlock(file), nil
}

func unlock(file *os.File) func() {
	return func() {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		_ = file.Close()
	}
}
