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

// Identity reads the real POSIX SID, then the process start identity. Both are
// compared with the anchored values immediately before every signal.
func (table osProcessTable) Identity(ctx context.Context, pid int) (processIdentity, error) {
	if pid <= 1 {
		return processIdentity{}, fmt.Errorf("unsafe pid %d", pid)
	}
	sid, err := platformProcessSessionID(pid)
	if err != nil {
		return processIdentity{}, err
	}
	probeCtx, cancel := context.WithTimeout(ctx, table.timeout)
	defer cancel()
	out, err := table.runner.Run(probeCtx, nil, "ps", "-p", strconv.Itoa(pid), "-o", "lstart=")
	if probeCtx.Err() != nil {
		return processIdentity{}, probeCtx.Err()
	}
	if err != nil {
		return processIdentity{}, fmt.Errorf("read process %d start identity: %w", pid, err)
	}
	rawStarted := strings.TrimSpace(string(out))
	if rawStarted == "" || strings.Contains(rawStarted, "\n") {
		return processIdentity{}, fmt.Errorf("read process %d start identity: invalid output", pid)
	}
	started := strings.Join(strings.Fields(rawStarted), " ")
	return processIdentity{pid: pid, sessionID: sid, started: started}, nil
}

func (table osProcessTable) Snapshot(ctx context.Context, sessionIDs map[int]struct{}) ([]processIdentity, error) {
	probeCtx, cancel := context.WithTimeout(ctx, table.timeout)
	defer cancel()
	out, err := table.runner.Run(probeCtx, nil, "ps", "-axo", "pid=,lstart=")
	if probeCtx.Err() != nil {
		return nil, probeCtx.Err()
	}
	if err != nil {
		return nil, err
	}
	processes := make([]processIdentity, 0)
	for _, line := range strings.Split(string(out), "\n") {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, parseErr := strconv.Atoi(fields[0])
		if parseErr != nil || pid <= 1 {
			continue
		}
		sid, sidErr := platformProcessSessionID(pid)
		if _, wanted := sessionIDs[sid]; sidErr == nil && wanted {
			processes = append(processes, processIdentity{
				pid: pid, sessionID: sid, started: strings.Join(fields[1:], " "),
			})
		}
	}
	return processes, nil
}
