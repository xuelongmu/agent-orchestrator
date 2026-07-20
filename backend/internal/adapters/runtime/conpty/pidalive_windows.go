//go:build windows

package conpty

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

type windowsProcess struct {
	handle windows.Handle
}

func (p *windowsProcess) Alive() (bool, error) {
	result, err := windows.WaitForSingleObject(p.handle, 0)
	if err != nil {
		return false, err
	}
	return result == uint32(windows.WAIT_TIMEOUT), nil
}

func (p *windowsProcess) Kill() error {
	return windows.TerminateProcess(p.handle, 1)
}

func (p *windowsProcess) Close() error {
	return windows.CloseHandle(p.handle)
}

// defaultOSProcessFinder opens and retains the exact Windows process object.
func defaultOSProcessFinder(pid int) (processKiller, error) {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return nil, fmt.Errorf("OpenProcess(%d): %w", pid, err)
	}
	return &windowsProcess{handle: h}, nil
}

func isProcessNotFound(err error) bool {
	return errors.Is(err, os.ErrProcessDone) || errors.Is(err, windows.ERROR_INVALID_PARAMETER) || errors.Is(err, windows.ERROR_NOT_FOUND)
}
