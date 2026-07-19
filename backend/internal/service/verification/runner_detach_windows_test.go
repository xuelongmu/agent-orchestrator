//go:build windows

package verification

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

func configureDetachedChild(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
		HideWindow:    true,
	}
}
