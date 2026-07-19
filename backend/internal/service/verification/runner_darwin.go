//go:build darwin

package verification

import "syscall"

func verificationSysProcAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }
