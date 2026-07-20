//go:build darwin || linux

package tmux

import "golang.org/x/sys/unix"

// platformProcessSessionID uses the POSIX getsid(2) API. In particular, Darwin
// ps exposes e_sess (a kernel pointer) as "sess", not the numeric session ID.
func platformProcessSessionID(pid int) (int, error) {
	return unix.Getsid(pid)
}
