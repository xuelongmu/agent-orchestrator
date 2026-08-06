//go:build windows

// Package processalive probes whether an operating-system process id still
// maps to a live process.
package processalive

import (
	"errors"
	"math"

	"golang.org/x/sys/windows"
)

// Alive reports whether pid exists. Access denied counts as alive: the process
// exists even if the current user cannot wait on it.
func Alive(pid int) bool {
	if pid <= 0 || uint64(pid) > math.MaxUint32 {
		return false
	}
	pid32 := uint32(pid) // #nosec G115 -- range checked above.
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, pid32)
	if err != nil {
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer func() { _ = windows.CloseHandle(handle) }()

	status, err := windows.WaitForSingleObject(handle, 0)
	if err != nil {
		return false
	}
	return status == uint32(windows.WAIT_TIMEOUT)
}
