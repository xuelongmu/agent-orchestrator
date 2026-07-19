//go:build !windows

package verification

import "os/exec"

func configureDetachedChild(_ *exec.Cmd) {}
