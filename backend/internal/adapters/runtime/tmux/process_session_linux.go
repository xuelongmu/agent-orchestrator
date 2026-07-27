//go:build linux

package tmux

import "golang.org/x/sys/unix"

// platformProcessSessionID uses the POSIX getsid(2) API. It is built for Linux
// only: process_linux.go is its sole caller, and process_darwin.go identifies
// processes without a session ID (Darwin ps exposes e_sess, a kernel pointer,
// as "sess" rather than the numeric session ID).
func platformProcessSessionID(pid int) (int, error) {
	return unix.Getsid(pid)
}
