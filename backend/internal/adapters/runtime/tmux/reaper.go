package tmux

import (
	"context"
	"fmt"
	"os"
	"syscall"
	"time"
)

const (
	reapPollingPasses = 4
	reapGraceSlices   = reapPollingPasses + 1
)

// paneSessionReaper is the process-mutation boundary used by Destroy. Tests
// inject a recorder so Destroy tests never signal host processes.
type paneSessionReaper interface {
	Anchor(ctx context.Context, panes []paneRef) []sessionAnchor
	Reap(ctx context.Context, anchors []sessionAnchor, grace time.Duration)
}

type processSessionReaper struct {
	table   processTable
	timeout time.Duration
	wait    func(context.Context, time.Duration) bool
}

type processTable interface {
	Open(ctx context.Context, pid int) (processObservation, error)
	Snapshot(ctx context.Context, sessionIDs map[int]struct{}) ([]processObservation, error)
}

// processHandle owns an exact kernel process reference. Signal must deliver
// through that reference rather than resolving a numeric PID again.
type processHandle interface {
	Alive(ctx context.Context) error
	Signal(ctx context.Context, signal os.Signal) error
	Close() error
}

type processIdentity struct {
	pid       int
	sessionID int
	started   string
}

type processObservation struct {
	identity processIdentity
	handle   processHandle
}

type processSet map[processIdentity]processHandle
type sessionSet map[int]processSet
type identitySet map[processIdentity]struct{}

// sessionAnchor owns every process handle in members until it is either
// excluded by tmux survivor verification or transferred to Reap.
type sessionAnchor struct {
	pane      paneRef
	sessionID int
	members   processSet
}

type paneRef struct {
	paneID   string
	windowID string
	pid      int
}

// Anchor opens exact handles while the tmux pane still exists, snapshots its
// POSIX session, and retains those handles across kill-session and delivery.
// Any incomplete platform or process probe disables cleanup for that pane.
func (r processSessionReaper) Anchor(ctx context.Context, panes []paneRef) []sessionAnchor {
	leaders := make(map[int]processObservation)
	owners := make(map[int]paneRef)
	leaderOrder := make([]int, 0, len(panes))
	for _, pane := range safeUniquePanes(panes) {
		leader, err := r.open(ctx, pane.pid)
		if err != nil || !safeIdentity(leader.identity) || leader.identity.pid != pane.pid || leader.identity.sessionID != pane.pid {
			closeObservation(leader)
			continue
		}
		if old, exists := leaders[pane.pid]; exists {
			closeObservation(old)
		} else {
			leaderOrder = append(leaderOrder, pane.pid)
		}
		leaders[pane.pid] = leader
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
		closeObservations(snapshot)
		closeObservationMap(leaders)
		return nil
	}
	members := observationsBySession(snapshot)
	anchors := make([]sessionAnchor, 0, len(leaders))
	for _, sid := range leaderOrder {
		leader := leaders[sid]
		set := members[sid]
		snapshotLeader, present := set[leader.identity]
		if !present || r.alive(ctx, leader.handle) != nil {
			continue
		}
		_ = snapshotLeader.Close()
		set[leader.identity] = leader.handle
		leader.handle = nil
		delete(leaders, sid)
		delete(members, sid)
		anchors = append(anchors, sessionAnchor{pane: owners[sid], sessionID: sid, members: set})
	}
	closeObservationMap(leaders)
	closeSessionSet(members)
	return anchors
}

// Reap uses four grace observations plus one final observation after the
// fourth wait. The five waits share the requested grace, so a child appearing
// in the last polling interval is TERM-ed, receives bounded grace, and is then
// escalated. Every snapshot receives its own timeout. Exact handles remain open
// from observation through all delivery attempts and are closed on return.
func (r processSessionReaper) Reap(ctx context.Context, anchors []sessionAnchor, grace time.Duration) {
	trusted := make(sessionSet, len(anchors))
	for i := range anchors {
		anchor := &anchors[i]
		if anchor.sessionID > 1 && len(anchor.members) > 0 {
			if _, duplicate := trusted[anchor.sessionID]; duplicate {
				closeProcessSet(anchor.members)
				anchor.members = nil
				continue
			}
			trusted[anchor.sessionID] = anchor.members
			anchor.members = nil
		} else {
			closeProcessSet(anchor.members)
			anchor.members = nil
		}
	}
	defer closeSessionSet(trusted)
	if len(trusted) == 0 {
		return
	}

	cleanupCtx := context.WithoutCancel(ctx)
	termed := make(identitySet)
	termOrder := make([]processIdentity, 0)
	for pass := 0; pass < reapPollingPasses; pass++ {
		if !r.observeAndTerm(cleanupCtx, trusted, termed, &termOrder) {
			return
		}
		if !r.wait(cleanupCtx, graceSlice(grace, pass)) {
			return
		}
	}
	// This observation closes the old tail hole: it runs after the fourth
	// grace wait, not before it.
	if !r.observeAndTerm(cleanupCtx, trusted, termed, &termOrder) {
		return
	}
	if !r.wait(cleanupCtx, graceSlice(grace, reapGraceSlices-1)) {
		return
	}
	for _, identity := range termOrder {
		if handle := trusted[identity.sessionID][identity]; handle != nil {
			_ = r.signal(cleanupCtx, handle, syscall.SIGKILL)
		}
	}
}

func (r processSessionReaper) observeAndTerm(
	ctx context.Context,
	trusted sessionSet,
	termed identitySet,
	termOrder *[]processIdentity,
) bool {
	snapshot, err := r.snapshot(ctx, sessionIDs(trusted))
	if err != nil {
		closeObservations(snapshot)
		return false
	}
	defer closeObservations(snapshot)
	current := groupObservations(snapshot)
	for sid, prior := range trusted {
		witness, ok := stableWitness(current[sid], prior)
		if !ok || r.alive(ctx, prior[witness]) != nil {
			closeProcessSet(prior)
			delete(trusted, sid)
			continue
		}
		for _, observation := range current[sid] {
			identity := observation.identity
			if _, done := termed[identity]; done {
				continue
			}
			handle, known := prior[identity]
			if !known {
				if r.alive(ctx, prior[witness]) != nil {
					closeProcessSet(prior)
					delete(trusted, sid)
					break
				}
				handle = observation.handle
			}
			if r.signal(ctx, handle, syscall.SIGTERM) != nil {
				continue
			}
			if !known {
				prior[identity] = handle
				observation.handle = nil
			}
			termed[identity] = struct{}{}
			*termOrder = append(*termOrder, identity)
		}
	}
	return len(trusted) > 0
}

func (r processSessionReaper) open(ctx context.Context, pid int) (processObservation, error) {
	probeCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	return r.table.Open(probeCtx, pid)
}

func (r processSessionReaper) snapshot(ctx context.Context, wanted map[int]struct{}) ([]processObservation, error) {
	probeCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	return r.table.Snapshot(probeCtx, wanted)
}

func (r processSessionReaper) alive(ctx context.Context, handle processHandle) error {
	if handle == nil {
		return fmt.Errorf("missing process handle")
	}
	probeCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	return handle.Alive(probeCtx)
}

func (r processSessionReaper) signal(ctx context.Context, handle processHandle, signal os.Signal) error {
	if handle == nil {
		return fmt.Errorf("missing process handle")
	}
	probeCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	return handle.Signal(probeCtx, signal)
}

func stableWitness(current []*processObservation, trusted processSet) (processIdentity, bool) {
	for _, observation := range current {
		if _, ok := trusted[observation.identity]; ok {
			return observation.identity, true
		}
	}
	return processIdentity{}, false
}

func groupObservations(processes []processObservation) map[int][]*processObservation {
	grouped := make(map[int][]*processObservation)
	for i := range processes {
		observation := &processes[i]
		if safeIdentity(observation.identity) && observation.handle != nil {
			grouped[observation.identity.sessionID] = append(grouped[observation.identity.sessionID], observation)
		}
	}
	return grouped
}

func observationsBySession(processes []processObservation) sessionSet {
	sets := make(sessionSet)
	for i := range processes {
		observation := &processes[i]
		identity := observation.identity
		if !safeIdentity(identity) || observation.handle == nil {
			continue
		}
		if sets[identity.sessionID] == nil {
			sets[identity.sessionID] = make(processSet)
		}
		if _, duplicate := sets[identity.sessionID][identity]; duplicate {
			continue
		}
		sets[identity.sessionID][identity] = observation.handle
		observation.handle = nil
	}
	closeObservations(processes)
	return sets
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

func closeAnchors(anchors []sessionAnchor) {
	for i := range anchors {
		closeProcessSet(anchors[i].members)
		anchors[i].members = nil
	}
}

func closeSessionSet(sessions sessionSet) {
	for sid, set := range sessions {
		closeProcessSet(set)
		delete(sessions, sid)
	}
}

func closeProcessSet(set processSet) {
	for identity, handle := range set {
		if handle != nil {
			_ = handle.Close()
		}
		delete(set, identity)
	}
}

func closeObservation(observation processObservation) {
	if observation.handle != nil {
		_ = observation.handle.Close()
	}
}

func closeObservations(observations []processObservation) {
	for i := range observations {
		if observations[i].handle != nil {
			_ = observations[i].handle.Close()
			observations[i].handle = nil
		}
	}
}

func closeObservationMap(observations map[int]processObservation) {
	for pid, observation := range observations {
		closeObservation(observation)
		delete(observations, pid)
	}
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
