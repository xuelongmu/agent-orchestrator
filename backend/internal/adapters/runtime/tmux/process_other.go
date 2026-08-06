//go:build !darwin && !linux

package tmux

import "fmt"

func platformOpenProcess(pid int) (processObservation, error) {
	return processObservation{}, fmt.Errorf("exact process signal handles are unavailable for pid %d", pid)
}
