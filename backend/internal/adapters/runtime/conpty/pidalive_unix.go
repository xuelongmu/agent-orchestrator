//go:build !windows

package conpty

import (
	"errors"
	"os"
	"syscall"
)

type unixProcess struct {
	process *os.Process
	pid     int
}

func (p *unixProcess) Alive() (bool, error) {
	err := syscall.Kill(p.pid, 0)
	if err == nil || errors.Is(err, syscall.EPERM) {
		return true, nil
	}
	if errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	return false, err
}

func (p *unixProcess) Kill() error  { return p.process.Kill() }
func (p *unixProcess) Close() error { return p.process.Release() }

// defaultOSProcessFinder is a buildability fallback; ConPTY production runs
// on Windows, where the implementation retains a native process object.
func defaultOSProcessFinder(pid int) (processKiller, error) {
	process, err := os.FindProcess(pid)
	if err != nil {
		return nil, err
	}
	return &unixProcess{process: process, pid: pid}, nil
}

func isProcessNotFound(err error) bool {
	return errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH)
}
