//go:build windows

package ptyregistry

import (
	"errors"
	"math"

	"golang.org/x/sys/windows"
)

// defaultPidAlive probes PID liveness via OpenProcess. SUCCESS means alive
// (CloseHandle and return true). ERROR_ACCESS_DENIED mirrors EPERM: the
// process exists but cannot be queried, so treat as alive.
func defaultPidAlive(pid int) bool {
	if pid <= 0 || uint64(pid) > math.MaxUint32 {
		return false
	}
	pid32 := uint32(pid) // #nosec G115 -- range checked above.
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid32)
	if err == nil {
		_ = windows.CloseHandle(h)
		return true
	}
	return errors.Is(err, windows.ERROR_ACCESS_DENIED)
}
