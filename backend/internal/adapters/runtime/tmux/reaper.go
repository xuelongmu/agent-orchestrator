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
	Anchor(ctx context.Context, panes []paneRef) []sessionAnchor
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
	Signal(ctx context.Context, expected processIdentity, signal os.Signal) error
}

type osProcessSignaler struct{}

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
	pane      paneRef
	sessionID int
	members   identitySet
}

type paneRef struct {
	paneID   string
	windowID string
	pid      int
}

// Anchor proves that each pane PID is its POSIX session leader, snapshots that
// session before tmux tears it down, then revalidates the leader. Failed probes
// only disable the best-effort reap for that pane.
func (r processSessionReaper) Anchor(ctx context.Context, panes []paneRef) []sessionAnchor {
	leaders := make(map[int]processIdentity)
	owners := make(map[int]paneRef)
	leaderOrder := make([]int, 0, len(panes))
	for _, pane := range safeUniquePanes(panes) {
		identity, err := r.identity(ctx, pane.pid)
		if err != nil || identity.pid != pane.pid || identity.sessionID != pane.pid || identity.started == "" {
			continue
		}
		if _, exists := leaders[pane.pid]; !exists {
			leaderOrder = append(leaderOrder, pane.pid)
		}
		leaders[pane.pid] = identity
		owners[pane.pid] = pane
	}
	if len(leaders) == 0 {
		return nil
	}

	wanted := make(map[int]struct{}, len(leaders))
	for sid := range leaders {
		wanted[sid] = struct{}{}
	}
	snapshot, err := r.snapshot(ctx, wanted)
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
	for _, sid := range leaderOrder {
		leader := leaders[sid]
		if _, present := members[sid][leader]; !present {
			continue
		}
		current, err := r.identity(ctx, leader.pid)
		if err != nil || current != leader {
			continue
		}
		anchors = append(anchors, sessionAnchor{pane: owners[sid], sessionID: sid, members: members[sid]})
	}
	return anchors
}

// Reap terminates anchored pane descendants. Cleanup deliberately outlives
// caller cancellation once kill-session has run, but remains bounded. The grace
// budget is split across four observations. Each snapshot and identity probe
// receives its own timeout; the grace interval never consumes their budgets.
// A child first seen in the final observation still gets one bounded slice
// before the exact-handle KILL pass.
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

	cleanupCtx := context.WithoutCancel(ctx)
	termed := make(identitySet)
	termOrder := make([]processIdentity, 0)
	for pass := 0; pass < reapGraceSlices; pass++ {
		snapshot, snapshotErr := r.snapshot(cleanupCtx, sessionIDs(trusted))
		if snapshotErr != nil {
			return
		}
		termOrder = r.termNew(cleanupCtx, snapshot, trusted, termed, termOrder)
		if len(trusted) == 0 {
			return
		}
		if !r.wait(cleanupCtx, graceSlice(grace, pass)) {
			return
		}
	}
	for _, process := range termOrder {
		if _, ok := trusted[process.sessionID][process]; ok {
			_ = r.signalExact(cleanupCtx, process, syscall.SIGKILL)
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
	termOrder []processIdentity,
) []processIdentity {
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
			termOrder = append(termOrder, process)
		}
	}
	return termOrder
}

func (r processSessionReaper) matches(ctx context.Context, expected processIdentity) bool {
	if !safeIdentity(expected) {
		return false
	}
	current, err := r.identity(ctx, expected.pid)
	return err == nil && current == expected
}

func (r processSessionReaper) signalExact(ctx context.Context, expected processIdentity, signal os.Signal) error {
	if !safeIdentity(expected) {
		return fmt.Errorf("unsafe process identity")
	}
	if !r.matches(ctx, expected) {
		return fmt.Errorf("process identity changed")
	}
	probeCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	return r.signaler.Signal(probeCtx, expected, signal)
}

func (r processSessionReaper) identity(ctx context.Context, pid int) (processIdentity, error) {
	probeCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	return r.table.Identity(probeCtx, pid)
}

func (r processSessionReaper) snapshot(ctx context.Context, wanted map[int]struct{}) ([]processIdentity, error) {
	probeCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	return r.table.Snapshot(probeCtx, wanted)
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

func safeUniquePanes(panes []paneRef) []paneRef {
	seen := make(map[paneRef]struct{}, len(panes))
	result := make([]paneRef, 0, len(panes))
	for _, pane := range panes {
		if pane.pid <= 1 || pane.paneID == "" || pane.windowID == "" {
			continue
		}
		if _, ok := seen[pane]; ok {
			continue
		}
		seen[pane] = struct{}{}
		result = append(result, pane)
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
