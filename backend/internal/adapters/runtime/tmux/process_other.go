//go:build !darwin && !linux

package tmux

import (
	"context"
	"fmt"
	"os"
)

func platformProcessSessionID(pid int) (int, error) {
	return 0, fmt.Errorf("POSIX process sessions are unavailable for pid %d", pid)
}

func platformProcessIdentity(pid int) (processIdentity, error) {
	return processIdentity{}, fmt.Errorf("process identity handles are unavailable for pid %d", pid)
}

func (osProcessSignaler) Signal(_ context.Context, expected processIdentity, _ os.Signal) error {
	return fmt.Errorf("exact process signaling is unavailable for pid %d", expected.pid)
}
