//go:build !windows

package verification

import (
	"os/exec"
	"syscall"
)

func configureDetachedChild(_ *exec.Cmd) {}

func detachCurrentProcessSession() error {
	_, err := syscall.Setsid()
	return err
}
