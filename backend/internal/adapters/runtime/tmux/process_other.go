//go:build !darwin && !linux

package tmux

import "fmt"

func platformProcessSessionID(pid int) (int, error) {
	return 0, fmt.Errorf("POSIX process sessions are unavailable for pid %d", pid)
}
