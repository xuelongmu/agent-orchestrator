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

// Identity uses platform kernel APIs for both session membership and the
// process generation token. It never relies on ps lstart's one-second clock.
func (table osProcessTable) Identity(ctx context.Context, pid int) (processIdentity, error) {
	if pid <= 1 {
		return processIdentity{}, fmt.Errorf("unsafe pid %d", pid)
	}
	if err := ctx.Err(); err != nil {
		return processIdentity{}, err
	}
	return platformProcessIdentity(pid)
}

func (table osProcessTable) Snapshot(ctx context.Context, sessionIDs map[int]struct{}) ([]processIdentity, error) {
	probeCtx, cancel := context.WithTimeout(ctx, table.timeout)
	defer cancel()
	out, err := table.runner.Run(probeCtx, nil, "ps", "-axo", "pid=")
	if probeCtx.Err() != nil {
		return nil, probeCtx.Err()
	}
	if err != nil {
		return nil, err
	}
	processes := make([]processIdentity, 0)
	for _, line := range strings.Split(string(out), "\n") {
		if err := probeCtx.Err(); err != nil {
			return nil, err
		}
		pid, parseErr := strconv.Atoi(strings.TrimSpace(line))
		if parseErr != nil || pid <= 1 {
			continue
		}
		identity, identityErr := table.Identity(probeCtx, pid)
		if _, wanted := sessionIDs[identity.sessionID]; identityErr == nil && wanted {
			processes = append(processes, identity)
		}
	}
	return processes, nil
}
