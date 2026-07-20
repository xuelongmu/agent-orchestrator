package tmux

import (
	"context"
	"fmt"
	"os"
	"syscall"
	"time"
)

const reapGraceSlices = 4

// paneSessionReaper is the process-mutation boundary used by Destroy. Tests
// inject a recorder so Destroy tests never signal host processes.
type paneSessionReaper interface {
	Anchor(ctx context.Context, panePIDs []int) []sessionAnchor
	Reap(ctx context.Context, anchors []sessionAnchor, grace time.Duration)
}

type processTable interface {
	Identity(ctx context.Context, pid int) (processIdentity, error)
	Snapshot(ctx context.Context, sessionIDs map[int]struct{}) ([]processIdentity, error)
}

type processSessionReaper struct {
	table    processTable
	signaler processSignaler
	timeout  time.Duration
	wait     func(context.Context, time.Duration) bool
}

type processSignaler interface {
	Signal(pid int, signal os.Signal) error
}

type osProcessSignaler struct{}

func (osProcessSignaler) Signal(pid int, signal os.Signal) error {
	if pid <= 1 {
		return fmt.Errorf("unsafe pid %d", pid)
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return process.Signal(signal)
}

// processIdentity combines the kernel session membership with a platform
// process-start token. Both are re-read immediately before every signal.
type processIdentity struct {
	pid       int
	sessionID int
	started   string
}

type identitySet map[processIdentity]struct{}
type sessionSet map[int]identitySet

// sessionAnchor records durable evidence gathered while the tmux pane still
// exists. A later pass only manages the SID while at least one exact member can
// carry this evidence forward, which bounds both PID and SID reuse.
type sessionAnchor struct {
	sessionID int
	members   identitySet
}

// Anchor proves that each pane PID is its POSIX session leader, snapshots that
// session before tmux tears it down, then revalidates the leader. Failed probes
// only disable the best-effort reap for that pane.
func (r processSessionReaper) Anchor(ctx context.Context, panePIDs []int) []sessionAnchor {
	leaders := make(map[int]processIdentity)
	for _, pid := range safeUniquePIDs(panePIDs) {
		identity, err := r.table.Identity(ctx, pid)
		if err != nil || identity.pid != pid || identity.sessionID != pid || identity.started == "" {
			continue
		}
		leaders[pid] = identity
	}
	if len(leaders) == 0 {
		return nil
	}

	wanted := make(map[int]struct{}, len(leaders))
	for sid := range leaders {
		wanted[sid] = struct{}{}
	}
	snapshot, err := r.table.Snapshot(ctx, wanted)
	if err != nil {
		return nil
	}
	members := make(sessionSet, len(leaders))
	for _, process := range snapshot {
		if _, ok := wanted[process.sessionID]; !ok || !safeIdentity(process) {
			continue
		}
		if members[process.sessionID] == nil {
			members[process.sessionID] = make(identitySet)
		}
		members[process.sessionID][process] = struct{}{}
	}

	anchors := make([]sessionAnchor, 0, len(leaders))
	for sid, leader := range leaders {
		if _, present := members[sid][leader]; !present {
			continue
		}
		current, err := r.table.Identity(ctx, leader.pid)
		if err != nil || current != leader {
			continue
		}
		anchors = append(anchors, sessionAnchor{sessionID: sid, members: members[sid]})
	}
	return anchors
}

// Reap terminates anchored pane descendants. Cleanup deliberately outlives
// caller cancellation once kill-session has run, but remains bounded. The grace
// budget begins after the initial post-teardown snapshot and TERM pass, then is
// split into bounded resnapshot passes so children created during grace are
// also terminated while session continuity remains proven.
func (r processSessionReaper) Reap(ctx context.Context, anchors []sessionAnchor, grace time.Duration) {
	trusted := make(sessionSet, len(anchors))
	for _, anchor := range anchors {
		if anchor.sessionID > 1 && len(anchor.members) > 0 {
			trusted[anchor.sessionID] = cloneIdentitySet(anchor.members)
		}
	}
	if len(trusted) == 0 {
		return
	}

	initialCtx, initialCancel := context.WithTimeout(context.WithoutCancel(ctx), r.timeout)
	initial, err := r.table.Snapshot(initialCtx, sessionIDs(trusted))
	if err != nil {
		initialCancel()
		return
	}
	termed := make(identitySet)
	r.termNew(initialCtx, initial, trusted, termed)
	initialCancel()
	if len(trusted) == 0 {
		return
	}

	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), grace+r.timeout)
	defer cancel()
	for pass := 0; pass < reapGraceSlices; pass++ {
		if !r.wait(cleanupCtx, graceSlice(grace, pass)) {
			return
		}
		snapshot, snapshotErr := r.table.Snapshot(cleanupCtx, sessionIDs(trusted))
		if snapshotErr != nil {
			return
		}

		// Processes already TERM-ed before this final observation are eligible
		// for KILL. A child first seen now receives TERM but never an immediate
		// KILL without any grace.
		var killable identitySet
		if pass == reapGraceSlices-1 {
			killable = cloneIdentitySet(termed)
		}
		r.termNew(cleanupCtx, snapshot, trusted, termed)
		if len(trusted) == 0 {
			return
		}
		if pass == reapGraceSlices-1 {
			for _, process := range snapshot {
				if _, ok := trusted[process.sessionID][process]; !ok {
					continue
				}
				if _, ok := killable[process]; !ok {
					continue
				}
				_ = r.signalExact(cleanupCtx, process, syscall.SIGKILL)
			}
		}
	}
}

// termNew advances continuity one snapshot at a time. A previously trusted
// exact identity must still exist for a session to stay active. Newly observed
// members are only trusted after that witness and the target have both been
// revalidated immediately before TERM.
func (r processSessionReaper) termNew(
	ctx context.Context,
	snapshot []processIdentity,
	trusted sessionSet,
	termed identitySet,
) {
	bySession := groupProcesses(snapshot)
	for sid, prior := range trusted {
		witness, ok := stableWitness(bySession[sid], prior)
		if !ok || !r.matches(ctx, witness) {
			delete(trusted, sid)
			continue
		}
		for _, process := range bySession[sid] {
			if _, done := termed[process]; done {
				continue
			}
			if _, known := prior[process]; !known && !r.matches(ctx, witness) {
				delete(trusted, sid)
				break
			}
			if r.signalExact(ctx, process, syscall.SIGTERM) != nil {
				continue
			}
			prior[process] = struct{}{}
			termed[process] = struct{}{}
		}
	}
}

func (r processSessionReaper) matches(ctx context.Context, expected processIdentity) bool {
	if !safeIdentity(expected) || ctx.Err() != nil {
		return false
	}
	current, err := r.table.Identity(ctx, expected.pid)
	return err == nil && current == expected
}

func (r processSessionReaper) signalExact(ctx context.Context, expected processIdentity, signal os.Signal) error {
	if !r.matches(ctx, expected) {
		return fmt.Errorf("process identity changed")
	}
	return r.signaler.Signal(expected.pid, signal)
}

func stableWitness(current []processIdentity, trusted identitySet) (processIdentity, bool) {
	for _, process := range current {
		if _, ok := trusted[process]; ok {
			return process, true
		}
	}
	return processIdentity{}, false
}

func groupProcesses(processes []processIdentity) map[int][]processIdentity {
	grouped := make(map[int][]processIdentity)
	for _, process := range processes {
		if safeIdentity(process) {
			grouped[process.sessionID] = append(grouped[process.sessionID], process)
		}
	}
	return grouped
}

func safeIdentity(process processIdentity) bool {
	return process.pid > 1 && process.sessionID > 1 && process.started != ""
}

func safeUniquePIDs(pids []int) []int {
	seen := make(map[int]struct{}, len(pids))
	result := make([]int, 0, len(pids))
	for _, pid := range pids {
		if pid <= 1 {
			continue
		}
		if _, ok := seen[pid]; ok {
			continue
		}
		seen[pid] = struct{}{}
		result = append(result, pid)
	}
	return result
}

func sessionIDs(values sessionSet) map[int]struct{} {
	set := make(map[int]struct{}, len(values))
	for value := range values {
		set[value] = struct{}{}
	}
	return set
}

func cloneIdentitySet(source identitySet) identitySet {
	clone := make(identitySet, len(source))
	for identity := range source {
		clone[identity] = struct{}{}
	}
	return clone
}

func graceSlice(grace time.Duration, pass int) time.Duration {
	if grace <= 0 {
		return 0
	}
	base := grace / reapGraceSlices
	if pass == reapGraceSlices-1 {
		return base + grace%reapGraceSlices
	}
	return base
}

func waitContext(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
