package tmux

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type osProcessTable struct {
	runner  runner
	timeout time.Duration
}

// Open retains the strongest platform process handle from discovery through
// delivery. Callers own the returned handle and must close it.
func (table osProcessTable) Open(ctx context.Context, pid int) (processObservation, error) {
	if pid <= 1 {
		return processObservation{}, fmt.Errorf("unsafe pid %d", pid)
	}
	if err := ctx.Err(); err != nil {
		return processObservation{}, err
	}
	return platformOpenProcess(pid)
}

func (table osProcessTable) Snapshot(ctx context.Context, sessionIDs map[int]struct{}) ([]processObservation, error) {
	probeCtx, cancel := context.WithTimeout(ctx, table.timeout)
	defer cancel()
	out, err := table.runner.Run(probeCtx, nil, "ps", "-axo", "pid=")
	if probeCtx.Err() != nil {
		return nil, probeCtx.Err()
	}
	if err != nil {
		return nil, err
	}
	processes := make([]processObservation, 0)
	for _, line := range strings.Split(string(out), "\n") {
		if err := probeCtx.Err(); err != nil {
			closeObservations(processes)
			return nil, err
		}
		pid, parseErr := strconv.Atoi(strings.TrimSpace(line))
		if parseErr != nil || pid <= 1 {
			continue
		}
		observation, identityErr := table.Open(probeCtx, pid)
		if identityErr != nil {
			closeObservation(observation)
			continue
		}
		if _, wanted := sessionIDs[observation.identity.sessionID]; wanted {
			processes = append(processes, observation)
		} else {
			_ = observation.handle.Close()
		}
	}
	return processes, nil
}
