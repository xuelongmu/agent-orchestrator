//go:build !windows

package copilot

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func lockCopilotExclude(path string, onContention func()) (func(), error) {
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
			return unlockCopilotExclude(file), nil
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
	return unlockCopilotExclude(file), nil
}

func unlockCopilotExclude(file *os.File) func() {
	return func() {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		_ = file.Close()
	}
}
