// Package lifecycle implements the synchronous reducer that writes durable
// session lifecycle facts. It deliberately keeps the session model small:
// activity_state plus an is_terminated bit are the only persisted status-like
// facts on the session row.
package lifecycle

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/designcontract"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/sessionguard"
)

type sessionStore interface {
	GetSession(ctx context.Context, id domain.SessionID) (domain.SessionRecord, bool, error)
	UpdateSession(ctx context.Context, rec domain.SessionRecord) error
	UpdateSessionLifecycle(ctx context.Context, before, after domain.SessionRecord) error
	MarkReservedDependencySpawned(ctx context.Context, id domain.SessionID, token string, metadata domain.SessionMetadata, updatedAt time.Time) (bool, error)
	PrepareReservedDependencyWorkspace(ctx context.Context, id domain.SessionID, token string, metadata domain.SessionMetadata, worktrees []domain.SessionWorktreeRecord, updatedAt time.Time) (bool, error)
	MarkReservedDependencyLaunchSucceeded(ctx context.Context, id domain.SessionID, token string, succeededAt time.Time) (bool, error)
	ResetReservedDependencyLaunch(ctx context.Context, id domain.SessionID, token string, preserveWorktrees bool, updatedAt time.Time) (bool, error)
	// ListPRsBySession returns every PR row tracked for the session. The
	// reducer reads it to apply the multi-PR completion rule (terminate only
	// when no open PR remains and at least one merged) and to suppress
	// merge-conflict nudges on PRs stacked behind an open parent.
	ListPRsBySession(ctx context.Context, id domain.SessionID) ([]domain.PullRequest, error)
	// GetPRLastNudgeSignature / UpdatePRLastNudgeSignature persist the
	// reaction-dedup map so nudges survive a daemon restart.
	GetPRLastNudgeSignature(ctx context.Context, prURL string) (string, error)
	UpdatePRLastNudgeSignature(ctx context.Context, prURL, payload string) error
	GetPRDesignContract(ctx context.Context, prURL string) (string, bool, error)
}

type designContractDeliveryStore interface {
	GetPendingPRDesignContractDelivery(ctx context.Context, sessionID domain.SessionID, prURL string) (designcontract.PendingDelivery, bool, error)
	CompletePRDesignContractDelivery(ctx context.Context, sessionID domain.SessionID, prURL, deliveryToken string, contractRevision int64) (bool, error)
}

// reactionReservationStore is the atomic pre-send boundary implemented by the
// SQLite store. Keeping it optional preserves the reducer's existing in-memory
// fallback for tests and non-SQLite embeddings.
type reactionReservationStore interface {
	ReservePRReaction(ctx context.Context, prURL, key, signature string, maxAttempts int, ownerToken string, fences []ports.PRReactionFence, now, leaseExpiresAt time.Time) (ports.PRReactionReservation, error)
	StartPRReaction(ctx context.Context, prURL, key, ownerToken string, now, leaseExpiresAt time.Time) (ports.PRReactionReservation, error)
	CommitPRReaction(ctx context.Context, prURL, key, ownerToken string) (bool, error)
	ReleasePRReaction(ctx context.Context, prURL, key, ownerToken string) (bool, error)
}

// simplificationEventStore is the transactional local telemetry boundary for
// simplification activity. It is kept separate from sessionStore because only
// review delivery needs to mutate review_run state.
type simplificationEventStore interface {
	EnsureReviewRunSimplificationEvent(ctx context.Context, id, targetSHA string, event ports.TelemetryEvent) (ports.TelemetryEvent, bool, error)
}

// notificationSink is the optional lifecycle-to-notification-producer boundary.
type notificationSink interface {
	Notify(ctx context.Context, intent ports.NotificationIntent) error
}

// MergedSessionCleaner tears down the external resources owned by a session
// whose pull requests reached the lifecycle completion bar. It is a
// resource-only callback: implementations must not call back into lifecycle.
// Lifecycle durably reserves the terminal state before invoking it and clears
// the replay latch only after it succeeds.
type MergedSessionCleaner interface {
	CleanupMergedSession(ctx context.Context, id domain.SessionID) error
}

// CompletedSessionCleaner tears down ephemeral or shared external resources
// after an agent exit or authoritative dead-runtime observation. Implementations
// must leave git worktrees untouched and must not call back into lifecycle.
type CompletedSessionCleaner interface {
	CleanupCompletedSession(ctx context.Context, id domain.SessionID) error
}

// diagnosticRuntime is the read-only runtime surface lifecycle needs to attach
// terminal evidence to abnormal transitions. It is optional so pure reducer
// tests and embeddings can run without a runtime.
type diagnosticRuntime interface {
	GetOutput(ctx context.Context, handle ports.RuntimeHandle, lines int) (string, error)
}

// automatedMessageSender is the confirmed session-manager delivery boundary.
// Unlike the raw pane guard, it detects and durably latches unsubmitted editor
// drafts before reporting success.
type automatedMessageSender interface {
	SendAutomated(ctx context.Context, id domain.SessionID, message string) error
}

type dependencyReconciler interface {
	Wake()
}

const (
	diagnosticTailLines      = 40
	diagnosticCaptureTimeout = 750 * time.Millisecond
)

// Option customizes a Manager.
type Option func(*Manager)

// WithNotificationSink wires lifecycle notification intents to a write-side producer.
func WithNotificationSink(sink notificationSink) Option {
	return func(m *Manager) { m.notifications = sink }
}

// WithTelemetry wires lifecycle activity transitions to the shared telemetry sink.
func WithTelemetry(sink ports.EventSink) Option {
	return func(m *Manager) { m.telemetry = sink }
}

// WithDiagnosticRuntime enables best-effort terminal-tail capture at abnormal
// lifecycle boundaries. Capture errors are intentionally ignored by the state
// reducer: observability must never change a lifecycle decision.
func WithDiagnosticRuntime(runtime diagnosticRuntime) Option {
	return func(m *Manager) { m.diagnosticRuntime = runtime }
}

// Manager reduces runtime, activity, spawn, and termination observations into durable session facts.
// It also owns agent nudges caused by PR observations, including merge-conflict, CI-failure, and review-feedback prompts.
type Manager struct {
	store sessionStore
	// guard is the shared pane-write primitive every reaction nudge goes
	// through (see sessionguard). Nil when no messenger was wired: reaction
	// nudges become no-ops but the reducer still runs.
	guard               *sessionguard.Guard
	automatedSender     automatedMessageSender
	notifications       notificationSink
	mergedCleaner       MergedSessionCleaner
	completedCleaner    CompletedSessionCleaner
	diagnosticRuntime   diagnosticRuntime
	dependencyScheduler dependencyReconciler

	mu        sync.Mutex
	window    time.Duration
	clock     func() time.Time
	react     reactionState
	telemetry ports.EventSink
	// flights tracks, per session, the in-flight tool executions and the
	// pending permission dialog's identity (see toolFlight). Guarded by mu.
	flights map[domain.SessionID]*toolFlight
}

// SetMergedSessionCleaner wires the session-manager teardown path after both
// components have been constructed. Daemon startup creates lifecycle first so
// the session manager can depend on it; this late-bound edge closes that cycle
// before the SCM observer starts polling.
func (m *Manager) SetMergedSessionCleaner(cleaner MergedSessionCleaner) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mergedCleaner = cleaner
}

// SetCompletedSessionCleaner wires resource teardown for naturally completed
// non-git sessions after the session manager has been constructed.
func (m *Manager) SetCompletedSessionCleaner(cleaner CompletedSessionCleaner) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.completedCleaner = cleaner
}

// SetAutomatedMessageSender wires the confirmed session-manager send boundary
// used to recover durable claim-ready deliveries after initial failure or a
// daemon restart. It must be set before observer/reaper work starts.
func (m *Manager) SetAutomatedMessageSender(sender automatedMessageSender) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.automatedSender = sender
}

// SetDependencyScheduler wires the cross-session reconciler after daemon
// construction. Lifecycle only invokes it after it has decided completion; the
// scheduler never participates in terminal-state decisions.
func (m *Manager) SetDependencyScheduler(scheduler dependencyReconciler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dependencyScheduler = scheduler
}

func (m *Manager) reconcileDependencies() error {
	m.mu.Lock()
	scheduler := m.dependencyScheduler
	m.mu.Unlock()
	if scheduler == nil {
		return nil
	}
	scheduler.Wake()
	return nil
}

func (m *Manager) cleanupCompletedSession(ctx context.Context, id domain.SessionID) error {
	m.mu.Lock()
	cleaner := m.completedCleaner
	m.mu.Unlock()
	if cleaner == nil {
		return nil
	}
	return cleaner.CleanupCompletedSession(ctx, id)
}

// New builds a Lifecycle Manager over the session store it writes and the messenger it uses for agent nudges.
func New(store sessionStore, messenger ports.AgentMessenger, opts ...Option) *Manager {
	// UTC so activity-driven LastActivityAt/UpdatedAt match spawn-stamped
	// timestamps (the session manager clock is UTC too); a local clock here left
	// `ao session get` showing created in UTC but updated in local time. A
	// WithClock option may still override this in tests.
	clock := func() time.Time { return time.Now().UTC() }
	m := &Manager{store: store, window: defaultRecentActivityWindow, clock: clock, react: newReactionState(), flights: map[domain.SessionID]*toolFlight{}}
	if messenger != nil {
		m.guard = sessionguard.New(store, messenger, nil)
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

func (m *Manager) mutate(ctx context.Context, id domain.SessionID, fn func(domain.SessionRecord, time.Time) (domain.SessionRecord, bool)) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	rec, ok, err := m.store.GetSession(ctx, id)
	if err != nil || !ok {
		return err
	}
	now := m.clock()
	next, changed := fn(rec, now)
	if !changed {
		return nil
	}
	next = applyPendingSubmitInvariant(next)
	next.UpdatedAt = now
	if err := m.store.UpdateSessionLifecycle(ctx, rec, next); err != nil {
		return err
	}
	return nil
}

// ApplyRuntimeObservation only changes lifecycle state when runtime liveness
// is unambiguous. Failed/dead probes may still persist best-effort diagnostics,
// and a later alive probe clears those transient runtime diagnostics.
func (m *Manager) ApplyRuntimeObservation(ctx context.Context, id domain.SessionID, f ports.RuntimeFacts) error {
	var diagnostic *domain.LifecycleDiagnostic
	switch f.Probe {
	case ports.ProbeFailed:
		diagnostic = m.captureDiagnostic(ctx, id, domain.DiagnosticRuntimeProbeFailed, "")
	case ports.ProbeDead:
		diagnostic = m.captureDiagnostic(ctx, id, domain.DiagnosticRuntimeDead, "")
	}
	terminated := false
	err := m.mutate(ctx, id, func(cur domain.SessionRecord, now time.Time) (domain.SessionRecord, bool) {
		if cur.IsTerminated {
			return cur, false
		}
		if f.Probe == ports.ProbeFailed {
			if diagnostic == nil || sameDiagnosticContent(cur.Diagnostic, diagnostic) {
				return cur, false
			}
			cur.Diagnostic = diagnostic
			return cur, true
		}
		if f.Probe == ports.ProbeAlive && isRuntimeDiagnostic(cur.Diagnostic) {
			cur.Diagnostic = nil
			return cur, true
		}
		if !runtimeClearlyDead(f, cur.Activity, now, m.window) {
			// A recent activity signal delays the terminal conclusion, but the
			// first readable dead-runtime screen may disappear before the grace
			// window closes. Preserve that evidence now without changing state.
			if f.Probe == ports.ProbeDead && diagnostic != nil && !sameDiagnosticContent(cur.Diagnostic, diagnostic) {
				cur.Diagnostic = diagnostic
				return cur, true
			}
			return cur, false
		}
		next := cur
		next.IsTerminated = true
		next.Activity = domain.Activity{State: domain.ActivityExited, LastActivityAt: timeOr(f.ObservedAt, now)}
		if diagnostic != nil {
			next.Diagnostic = diagnostic
		}
		// Reaper-driven death (crash/SIGKILL) never fires a session-end hook,
		// so this is the last chance to release the session's tool-flight
		// state; a leaked entry would otherwise persist for the daemon's life
		// (later observations return early on cur.IsTerminated). Runs under
		// m.mu — mutate holds it across this callback.
		delete(m.flights, id)
		terminated = true
		return next, true
	})
	if err != nil || !terminated {
		return err
	}
	if err := m.cleanupCompletedSession(ctx, id); err != nil {
		return err
	}
	return m.reconcileDependencies()
}

// ApplyActivitySignal records an authoritative agent activity signal and any
// native agent session id carried alongside it. Metadata-only hooks leave the
// existing activity and first-signal facts untouched.
func (m *Manager) ApplyActivitySignal(ctx context.Context, id domain.SessionID, s ports.ActivitySignal) error {
	s.AgentSessionID = strings.TrimSpace(s.AgentSessionID)
	// The hook receiver already parses Claude's documented StopFailure `error`
	// field into ErrorType for diagnostics. Promote only the provider-neutral
	// rate_limit category into a parked lifecycle state; other stop failures
	// retain the normal idle diagnostic path.
	if s.Valid && s.Event == "stop-failure" && s.ErrorType == "rate_limit" {
		s.State = domain.ActivityRateLimited
	}
	if !s.Valid && s.AgentSessionID == "" {
		return nil
	}
	diagnostic := m.captureSignalDiagnostic(ctx, id, s)
	var intent *ports.NotificationIntent
	m.mu.Lock()
	rec, ok, err := m.store.GetSession(ctx, id)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", ports.ErrSessionNotFound, id)
	}
	before := rec
	now := m.clock()
	if rec.IsTerminated {
		delete(m.flights, id)
		m.mu.Unlock()
		if s.Valid && s.State == domain.ActivityExited {
			if err := m.cleanupCompletedSession(ctx, id); err != nil {
				return err
			}
			return m.reconcileDependencies()
		}
		return nil
	}
	// Event-tagged signals fold through the session's tool-flight state first:
	// they may be suppressed (state write skipped) by the blocked-precedence
	// rule, while their tracking side effects still land. Untagged signals
	// (old CLIs, adapters without tool identity) pass through untouched —
	// last-writer-wins, exactly as before.
	metadataChanged := s.AgentSessionID != "" && rec.Metadata.AgentSessionID != s.AgentSessionID
	if s.Valid {
		s = m.applyToolPrecedenceLocked(id, rec.Activity.State, s)
	}
	// Accepted active/idle signals prove a non-terminal anomaly recovered. Do
	// not leave stale "Stuck diagnostic" evidence attached to a healthy
	// session. Terminal diagnostics remain as last-known evidence.
	diagnosticCleared := diagnostic == nil && s.Valid && (s.State == domain.ActivityActive || s.State == domain.ActivityIdle) && isRecoverableDiagnostic(rec.Diagnostic)
	if diagnosticCleared {
		rec.Diagnostic = nil
	}
	// Any authoritative non-waiting signal proves an editor-pending prompt is
	// no longer waiting to be submitted. Clear that delivery latch before the
	// same-state fast path below: activity persistence may otherwise be a no-op,
	// but the safety latch is an independent durable fact.
	pendingSubmitCleared := s.Valid && (!s.State.NeedsInput() || s.State.BlocksAutomatedDelivery()) &&
		(rec.Metadata.PendingSubmitFingerprint != "" || rec.Metadata.PendingSubmitRecoveryAttempted)
	if pendingSubmitCleared {
		rec.Metadata.PendingSubmitFingerprint = ""
		rec.Metadata.PendingSubmitRecoveryAttempted = false
	}
	if !s.Valid && !metadataChanged {
		m.mu.Unlock()
		return nil
	}
	diagnosticChanged := diagnostic != nil && !sameDiagnosticContent(rec.Diagnostic, diagnostic)
	if !s.Valid {
		rec.Metadata.AgentSessionID = s.AgentSessionID
		rec.UpdatedAt = now
		err := m.store.UpdateSessionLifecycle(ctx, before, rec)
		m.mu.Unlock()
		return err
	}
	if metadataChanged {
		// Fold metadata into rec before copying it into next below, so the
		// activity and resume handle land in one store update.
		rec.Metadata.AgentSessionID = s.AgentSessionID
	}
	prevState := rec.Activity.State
	prevAt := rec.Activity.LastActivityAt
	act := domain.Activity{State: s.State, LastActivityAt: timeOr(s.Timestamp, now)}
	sameState := sameActivity(rec.Activity, act)
	// A same-state repeat is still a write when it is the FIRST signal for
	// this spawn: the receipt itself is a durable fact (it clears the
	// no_signal display status). Hook deliveries are best-effort, so the
	// first to ARRIVE may match the seeded state — e.g. a turn's "active"
	// POST is lost and its Stop hook lands idle on the idle-seeded row.
	if sameState && !rec.FirstSignalAt.IsZero() {
		if metadataChanged || pendingSubmitCleared || diagnosticChanged || diagnosticCleared {
			if diagnosticChanged {
				rec.Diagnostic = diagnostic
			}
			rec.UpdatedAt = now
			err := m.store.UpdateSessionLifecycle(ctx, before, rec)
			m.mu.Unlock()
			return err
		}
		m.mu.Unlock()
		return nil
	}
	next := rec
	next.Activity = act
	if diagnosticChanged {
		next.Diagnostic = diagnostic
	}
	if next.FirstSignalAt.IsZero() {
		next.FirstSignalAt = timeOr(s.Timestamp, now)
	}
	if s.State == domain.ActivityExited {
		next.IsTerminated = true
	}
	next.UpdatedAt = now
	if err := m.store.UpdateSessionLifecycle(ctx, before, next); err != nil {
		m.mu.Unlock()
		return err
	}
	// A provider usage-limit transition is a durable park, not a request for
	// user input. Reuse the persisted needs-input notification channel with
	// explicit copy so the condition is prominent without inventing a second
	// storage enum. Same-state repeats do not re-notify.
	if rec.Activity.State != domain.ActivityRateLimited && next.Activity.State == domain.ActivityRateLimited && !next.IsTerminated {
		intent = &ports.NotificationIntent{
			Type:               domain.NotificationNeedsInput,
			SessionID:          next.ID,
			ProjectID:          next.ProjectID,
			CreatedAt:          next.Activity.LastActivityAt,
			SessionDisplayName: next.DisplayName,
			TitleOverride:      "Agent usage limit reached",
			BodyOverride:       "The live session is parked and its worktree is preserved. Wait for the provider limit to reset, then send an explicit retry.",
		}
	}
	// Transition into the needs-input family (waiting_input or blocked) pings
	// the user; an in-family escalation (waiting_input -> blocked) does not
	// re-notify — the user was already pinged once for this pause.
	if intent == nil && !rec.Activity.State.NeedsInput() && next.Activity.State.NeedsInput() && !next.IsTerminated {
		intent = &ports.NotificationIntent{
			Type:               domain.NotificationNeedsInput,
			SessionID:          next.ID,
			ProjectID:          next.ProjectID,
			CreatedAt:          next.Activity.LastActivityAt,
			SessionDisplayName: next.DisplayName,
		}
	}
	waitingEvents := m.waitingInputEvents(next, prevState, prevAt, now)
	m.mu.Unlock()
	var recoveryErr error
	if prevState == domain.ActivityIdle && next.Activity.State != domain.ActivityIdle {
		recoveryErr = m.clearIdleReviewStateForSession(ctx, id)
	}
	for _, ev := range waitingEvents {
		m.emitTelemetry(ctx, ev)
	}
	m.emitNotification(ctx, intent)
	if next.Activity.State == domain.ActivityExited {
		if err := m.cleanupCompletedSession(ctx, id); err != nil {
			return err
		}
		if err := m.reconcileDependencies(); err != nil {
			return err
		}
	}
	return recoveryErr
}

// permissionIdentity distinguishes approvals that reuse a tool-call id in
// different Kimi main/background agents. Native pre/post payloads omit the
// agent id, so tool execution progress remains keyed by tool-call id alone.
type permissionIdentity struct {
	agentID   string
	toolUseID string
}

func activityPermissionIdentity(s ports.ActivitySignal) permissionIdentity {
	return permissionIdentity{agentID: s.AgentID, toolUseID: s.ToolUseID}
}

func (i permissionIdentity) valid() bool {
	return i.agentID != "" && i.toolUseID != ""
}

type toolProgress struct {
	name      string
	started   int
	completed int
}

// toolFlight tracks one session's in-flight tool executions and permission
// identities. Sticky `blocked` is cleared only by the exact approved tool's
// post, while Kimi waiting-input requests are cleared independently by their
// matching agent-scoped permission results. Tool hooks can fire for parallel
// subagents on the same session, whose traffic must never cross-clear.
// In-memory only: a daemon restart loses it and the session degrades to
// clearing at the next turn boundary — fail-safe staleness, never a spurious
// clear.
type toolFlight struct {
	// inflight retains bounded current-turn start/completion counts by native
	// tool-call id. Counts preserve safe post evidence when ids are reused by
	// concurrent main/background agents.
	inflight     map[string]toolProgress
	trackedTools int
	// blockedCandidate is the id of the UNIQUE in-flight tool bearing
	// the dialog's tool_name when it appeared — the tool whose post proves the
	// dialog was answered. hasBlockedCandidate is false when no in-flight tool
	// matched or the match was ambiguous.
	blockedCandidate    string
	hasBlockedCandidate bool
	// pendingPermissions contains every live Kimi approval in the current
	// turn. resolvedPermissions remembers result-before-request delivery so a
	// matching late request cannot recreate a completed wait.
	pendingPermissions  map[permissionIdentity]struct{}
	resolvedPermissions map[permissionIdentity]struct{}
}

func (f *toolFlight) knownPermissionCount(toolUseID string) int {
	count := 0
	for identity := range f.pendingPermissions {
		if identity.toolUseID == toolUseID {
			count++
		}
	}
	for identity := range f.resolvedPermissions {
		if identity.toolUseID == toolUseID {
			count++
		}
	}
	return count
}

func (f *toolFlight) toolFullyCompleted(toolUseID string) bool {
	progress, ok := f.inflight[toolUseID]
	return ok && progress.started > 0 && progress.completed == progress.started
}

func (f *toolFlight) resolveCompletedPermissions(toolUseID string) bool {
	progress, ok := f.inflight[toolUseID]
	if !ok || progress.completed < progress.started {
		return false
	}
	resolved := false
	for identity := range f.pendingPermissions {
		if identity.toolUseID != toolUseID {
			continue
		}
		delete(f.pendingPermissions, identity)
		f.resolvedPermissions[identity] = struct{}{}
		resolved = true
	}
	return resolved
}

// maxInflightTools caps current-turn tracked starts so reused ids and lost
// posts cannot grow correlation state without bound. Hitting the cap resets
// all tool and permission correlation, degrading to turn-boundary clearing.
const maxInflightTools = 128

// isToolUseEvent reports whether the AO hook event is one of the tool-use
// trio whose signals must not demote a sticky state on their own.
func isToolUseEvent(event string) bool {
	return event == "pre-tool-use" || event == "post-tool-use" || event == "post-tool-use-failure"
}

// isDeferredToolCompletionEvent reports observation-only callbacks that may be
// delivered after the awaited Stop callback. Their prerequisite callbacks
// already made an in-turn session non-idle, so a completion callback cannot
// legitimately start work from an idle state.
func isDeferredToolCompletionEvent(event string) bool {
	return event == "post-tool-use" || event == "post-tool-use-failure"
}

// isTurnBoundaryEvent reports the events that reliably mean the pending
// dialog is gone: a prompt cannot be submitted while a dialog holds the
// composer, and a turn cannot end (or the session exit) with one on screen.
func isTurnBoundaryEvent(event string) bool {
	return event == "user-prompt-submit" || event == "stop" || event == "session-end"
}

// applyToolPrecedenceLocked folds an event-tagged activity signal through the
// session's tool-flight state and decides whether its state write may
// proceed. Returned signal with Valid=false means "suppressed": the tracking
// side effects have landed but the state must not change. Signals without an
// Event pass through untouched — the compatibility contract for old CLIs and
// for adapters that don't tag their signals (their last-writer-wins semantics
// are pinned by tests). Caller must hold m.mu.
func (m *Manager) applyToolPrecedenceLocked(id domain.SessionID, cur domain.ActivityState, s ports.ActivitySignal) ports.ActivitySignal {
	if s.Event == "" {
		return s
	}
	suppressed := s
	suppressed.Valid = false

	fl := m.flights[id]
	ensure := func() *toolFlight {
		if fl == nil {
			fl = &toolFlight{
				inflight:            map[string]toolProgress{},
				pendingPermissions:  map[permissionIdentity]struct{}{},
				resolvedPermissions: map[permissionIdentity]struct{}{},
			}
			m.flights[id] = fl
		}
		return fl
	}
	identity := activityPermissionIdentity(s)
	kimiPostResolved := false

	// Tracking side effects happen regardless of what the state decision is.
	switch s.Event {
	case "pre-tool-use":
		if s.ToolUseID != "" {
			f := ensure()
			if f.trackedTools >= maxInflightTools {
				f.inflight = map[string]toolProgress{}
				f.trackedTools = 0
				f.pendingPermissions = map[permissionIdentity]struct{}{}
				f.resolvedPermissions = map[permissionIdentity]struct{}{}
				f.blockedCandidate = ""
				f.hasBlockedCandidate = false
			}
			progress := f.inflight[s.ToolUseID]
			progress.name = s.ToolName
			progress.started++
			f.inflight[s.ToolUseID] = progress
			f.trackedTools++
		}
	case "post-tool-use", "post-tool-use-failure":
		if fl != nil && s.ToolUseID != "" {
			progress, ok := fl.inflight[s.ToolUseID]
			if ok && progress.completed < progress.started {
				progress.completed++
				fl.inflight[s.ToolUseID] = progress
			}
			if s.Harness == "kimi" {
				kimiPostResolved = fl.resolveCompletedPermissions(s.ToolUseID)
			}
		}
	}

	switch {
	case s.Harness == "kimi" && s.Event == "permission-request" && s.State == domain.ActivityWaitingInput:
		// Observation-only Kimi permission callbacks can arrive after Stop.
		// PreToolUse already made a legitimate current-turn request non-idle.
		if cur == domain.ActivityIdle || fl == nil || !identity.valid() {
			return suppressed
		}
		progress, ok := fl.inflight[identity.toolUseID]
		if !ok {
			return suppressed
		}
		if _, resolved := fl.resolvedPermissions[identity]; resolved {
			// The matching result won the fire-and-forget race. Keep the
			// completed identity until its post drains it so duplicate requests
			// cannot recreate the wait.
			return suppressed
		}
		if fl.knownPermissionCount(identity.toolUseID) >= progress.started {
			return suppressed
		}
		if progress.completed >= progress.started {
			fl.resolvedPermissions[identity] = struct{}{}
			return suppressed
		}
		fl.pendingPermissions[identity] = struct{}{}
		return s

	case s.Harness == "kimi" && s.Event == "permission-result":
		if fl == nil || !identity.valid() {
			return suppressed
		}
		progress, ok := fl.inflight[identity.toolUseID]
		if !ok {
			return suppressed
		}
		if _, resolved := fl.resolvedPermissions[identity]; resolved {
			return suppressed
		}
		if _, pending := fl.pendingPermissions[identity]; pending {
			delete(fl.pendingPermissions, identity)
			fl.resolvedPermissions[identity] = struct{}{}
			if len(fl.pendingPermissions) > 0 {
				s.State = domain.ActivityWaitingInput
			}
			return s
		}
		// Result-before-request: remember the completed identity within this
		// turn and suppress its matching late request.
		if fl.knownPermissionCount(identity.toolUseID) >= progress.started {
			return suppressed
		}
		fl.resolvedPermissions[identity] = struct{}{}
		return suppressed

	case s.Harness == "kimi" && (s.Event == "post-tool-use" || s.Event == "post-tool-use-failure") && kimiPostResolved:
		if len(fl.pendingPermissions) > 0 {
			s.State = domain.ActivityWaitingInput
		}
		return s

	case cur == domain.ActivityIdle && isDeferredToolCompletionEvent(s.Event):
		if fl != nil && len(fl.inflight) == 0 {
			delete(m.flights, id)
		}
		return suppressed

	case s.State == domain.ActivityBlocked:
		// Entering (or re-asserting) blocked: snapshot the dialog's identity.
		// permission-request carries the blocking tool_name; the Notification
		// duplicate does not and must not wipe an existing snapshot.
		//
		// The permission hook payload does not carry the blocking tool's
		// tool_use_id (only its name), so we can only identify the blocking
		// tool unambiguously when EXACTLY ONE in-flight tool bears that name.
		// With two same-name tools in flight (a batch of Bash calls, one of
		// them the one at the dialog), a sibling's post could otherwise clear
		// a still-open dialog — the exact permission-bypass this change exists
		// to prevent. So we correlate only in the unique case; otherwise no
		// candidate is recorded and the block clears only at a turn boundary
		// (fail-closed).
		f := ensure()
		if s.ToolName != "" {
			// Recompute from scratch: this is a fresh dialog, so any candidate
			// carried from a prior one must not leak in.
			f.blockedCandidate = ""
			f.hasBlockedCandidate = false
			for candidate, progress := range f.inflight {
				outstanding := progress.started - progress.completed
				if progress.name != s.ToolName || outstanding == 0 {
					continue
				}
				if f.hasBlockedCandidate || outstanding > 1 {
					// A second same-name tool: ambiguous, fail closed by
					// leaving no candidate (only a turn boundary clears).
					f.blockedCandidate = ""
					f.hasBlockedCandidate = false
					break
				}
				f.blockedCandidate = candidate
				f.hasBlockedCandidate = true
			}
		}
		return s

	case cur == domain.ActivityBlocked:
		// Paused on a decision: only a turn boundary or the correlated post
		// may change the state.
		switch {
		case isTurnBoundaryEvent(s.Event):
			delete(m.flights, id)
			return s
		case (s.Event == "post-tool-use" || s.Event == "post-tool-use-failure") &&
			fl != nil && fl.hasBlockedCandidate && s.ToolUseID == fl.blockedCandidate &&
			fl.toolFullyCompleted(s.ToolUseID):
			// Every generation sharing the unambiguous blocking tool id finished:
			// the dialog was answered. Clear the candidate so a later dialog in
			// the same turn starts from a clean slate.
			fl.blockedCandidate = ""
			fl.hasBlockedCandidate = false
			return s
		default:
			// Subagent/sibling tool traffic (including a same-name sibling when
			// the block was ambiguous), notification sub-types (idle_prompt,
			// agent_completed), and anything else that is not proof the dialog
			// closed.
			return suppressed
		}

	case cur.IsSticky() && isToolUseEvent(s.Event):
		// waiting_input: background tool traffic must not clear the "waiting
		// on the user" marker; only an explicit user/turn signal does.
		return suppressed

	default:
		if isTurnBoundaryEvent(s.Event) {
			delete(m.flights, id)
		}
		return s
	}
}

func (m *Manager) waitingInputEvents(next domain.SessionRecord, prevState domain.ActivityState, prevAt, now time.Time) []ports.TelemetryEvent {
	if m.telemetry == nil {
		return nil
	}
	projectID := next.ProjectID
	sessionID := next.ID
	var events []ports.TelemetryEvent
	// Entry/exit is measured on the needs-input family boundary (waiting_input
	// or blocked): the event names stay waiting_input_* for dashboard
	// continuity, the payload state distinguishes the two, and an in-family
	// transition emits neither event so dwell covers the whole pause.
	if !prevState.NeedsInput() && next.Activity.State.NeedsInput() && !next.IsTerminated {
		events = append(events, ports.TelemetryEvent{
			Name:       "ao.session.waiting_input_entered",
			Source:     "lifecycle",
			OccurredAt: now.UTC(),
			Level:      ports.TelemetryLevelInfo,
			ProjectID:  &projectID,
			SessionID:  &sessionID,
			Payload: map[string]any{
				"state": string(next.Activity.State),
			},
		})
	}
	if prevState.NeedsInput() && !next.Activity.State.NeedsInput() {
		payload := map[string]any{
			"state":     string(next.Activity.State),
			"dwell_ms":  now.Sub(prevAt).Milliseconds(),
			"exited_to": string(next.Activity.State),
		}
		events = append(events, ports.TelemetryEvent{
			Name:       "ao.session.waiting_input_exited",
			Source:     "lifecycle",
			OccurredAt: now.UTC(),
			Level:      ports.TelemetryLevelInfo,
			ProjectID:  &projectID,
			SessionID:  &sessionID,
			Payload:    payload,
		})
	}
	return events
}

func (m *Manager) emitTelemetry(ctx context.Context, ev ports.TelemetryEvent) {
	if m.telemetry == nil {
		return
	}
	m.telemetry.Emit(ctx, ev)
}

func (m *Manager) emitNotification(ctx context.Context, intent *ports.NotificationIntent) {
	if intent == nil || m.notifications == nil {
		return
	}
	if err := m.notifications.Notify(ctx, *intent); err != nil {
		slog.Default().Warn("lifecycle: notification failed", "session", intent.SessionID, "type", intent.Type, "err", err)
	}
}

// MarkSpawned marks a newly spawned or restored session live and stores runtime/workspace handles.
func (m *Manager) MarkSpawned(ctx context.Context, id domain.SessionID, metadata domain.SessionMetadata) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok, err := m.store.GetSession(ctx, id)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("lifecycle: MarkSpawned for unknown session %q", id)
	}
	now := m.clock()
	rec.IsTerminated = false
	rec.Activity = domain.Activity{State: domain.ActivityIdle, LastActivityAt: now}
	// Each spawn/restore must re-prove its hook pipeline: clear the receipt so
	// a relaunch with broken hooks degrades to no_signal instead of inheriting
	// a stale "signals worked once" fact.
	rec.FirstSignalAt = time.Time{}
	rec.Diagnostic = nil
	rec.Metadata = mergeMetadata(rec.Metadata, metadata)
	rec.UpdatedAt = now
	return m.store.UpdateSession(ctx, rec)
}

// MarkDependencySpawned is the lifecycle-owned, token-fenced launch transition
// for a promoted child. Unlike MarkSpawned it never writes is_terminated, and a
// concurrent Kill/terminal transition makes the CAS lose rather than allowing
// the launch path to resurrect the child.
func (m *Manager) MarkDependencySpawned(ctx context.Context, id domain.SessionID, token string, metadata domain.SessionMetadata) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.store.MarkReservedDependencySpawned(ctx, id, token, metadata, m.clock())
}

// PrepareDependencyWorkspace atomically publishes every deterministic cleanup
// input before the launcher performs external workspace side effects.
func (m *Manager) PrepareDependencyWorkspace(ctx context.Context, id domain.SessionID, token string, metadata domain.SessionMetadata, worktrees []domain.SessionWorktreeRecord) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.store.PrepareReservedDependencyWorkspace(ctx, id, token, metadata, worktrees, m.clock())
}

// MarkDependencyLaunchSucceeded is the final token-fenced launch transition.
// It records no lifecycle state and loses to concurrent termination.
func (m *Manager) MarkDependencyLaunchSucceeded(ctx context.Context, id domain.SessionID, token string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.store.MarkReservedDependencyLaunchSucceeded(ctx, id, token, m.clock())
}

// ResetDependencyLaunch clears only resources previously committed under the
// same promotion token after an owned post-start delivery failure.
func (m *Manager) ResetDependencyLaunch(ctx context.Context, id domain.SessionID, token string, preserveWorktrees bool) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.store.ResetReservedDependencyLaunch(ctx, id, token, preserveWorktrees, m.clock())
}

// MarkTerminated marks a session terminated without tearing down external resources.
func (m *Manager) MarkTerminated(ctx context.Context, id domain.SessionID) error {
	diagnostic := m.captureDiagnostic(ctx, id, domain.DiagnosticTerminated, "")
	err := m.mutate(ctx, id, func(cur domain.SessionRecord, now time.Time) (domain.SessionRecord, bool) {
		if cur.IsTerminated {
			return cur, false
		}
		cur.IsTerminated = true
		cur.Activity = domain.Activity{State: domain.ActivityExited, LastActivityAt: now}
		if diagnostic != nil {
			cur.Diagnostic = diagnostic
		}
		delete(m.flights, id) // runs under m.mu (mutate holds it)
		return cur, true
	})
	if err != nil {
		return err
	}
	return m.reconcileDependencies()
}

// markTerminatedUnlessRateLimited atomically applies an automated terminal
// transition only when the session is not parked on a provider usage limit.
// Explicit user-owned teardown continues to use MarkTerminated.
func (m *Manager) markTerminatedUnlessRateLimited(ctx context.Context, id domain.SessionID) error {
	diagnostic := m.captureDiagnostic(ctx, id, domain.DiagnosticTerminated, "")
	err := m.mutate(ctx, id, func(cur domain.SessionRecord, now time.Time) (domain.SessionRecord, bool) {
		if cur.IsTerminated || cur.Activity.State == domain.ActivityRateLimited {
			return cur, false
		}
		cur.IsTerminated = true
		cur.Activity = domain.Activity{State: domain.ActivityExited, LastActivityAt: now}
		if diagnostic != nil {
			cur.Diagnostic = diagnostic
		}
		delete(m.flights, id)
		return cur, true
	})
	if err != nil {
		return err
	}
	return m.reconcileDependencies()
}

// reserveMergedCleanup atomically linearizes automated cleanup against a
// provider usage-limit park. It intentionally keeps MergedCleanupPending set:
// external teardown happens after this write and may need replay after a
// failure or daemon restart.
func (m *Manager) reserveMergedCleanup(ctx context.Context, id domain.SessionID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok, err := m.store.GetSession(ctx, id)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: %s", ports.ErrSessionNotFound, id)
	}
	if cur.Activity.State == domain.ActivityRateLimited {
		return errMergedCleanupRateLimited
	}
	if !cur.Metadata.MergedCleanupPending {
		return nil
	}
	if cur.IsTerminated && cur.Activity.State == domain.ActivityExited {
		return nil
	}
	before := cur
	now := m.clock()
	cur.IsTerminated = true
	cur.Activity = domain.Activity{State: domain.ActivityExited, LastActivityAt: now}
	cur = applyPendingSubmitInvariant(cur)
	cur.UpdatedAt = now
	delete(m.flights, id)
	return m.store.UpdateSessionLifecycle(ctx, before, cur)
}

// markMergedCleanupComplete is the only write that clears the durable replay
// latch. The terminal reservation remains intact.
func (m *Manager) markMergedCleanupComplete(ctx context.Context, id domain.SessionID) error {
	return m.mutate(ctx, id, func(cur domain.SessionRecord, _ time.Time) (domain.SessionRecord, bool) {
		if !cur.Metadata.MergedCleanupPending && cur.Metadata.MergedCleanupPRURL == "" {
			return cur, false
		}
		cur.Metadata.MergedCleanupPending = false
		cur.Metadata.MergedCleanupPRURL = ""
		return cur, true
	})
}

func (m *Manager) captureSignalDiagnostic(ctx context.Context, id domain.SessionID, s ports.ActivitySignal) *domain.LifecycleDiagnostic {
	if !s.Valid {
		return nil
	}
	trigger := domain.DiagnosticTrigger("")
	switch {
	case s.Event == "stop-failure":
		trigger = domain.DiagnosticStopFailure
	case s.State == domain.ActivityBlocked:
		trigger = domain.DiagnosticBlocked
	case s.State == domain.ActivityExited:
		trigger = domain.DiagnosticAgentExited
	}
	if trigger == "" {
		return nil
	}
	return m.captureDiagnostic(ctx, id, trigger, s.ErrorType)
}

func (m *Manager) captureDiagnostic(ctx context.Context, id domain.SessionID, trigger domain.DiagnosticTrigger, hookErrorType string) *domain.LifecycleDiagnostic {
	errorType := strings.TrimSpace(domain.SanitizeDiagnosticTail(hookErrorType))
	if len([]rune(errorType)) > 256 {
		errorType = ""
	}
	var tail string
	captured := false
	if m.diagnosticRuntime != nil {
		rec, ok, err := m.store.GetSession(ctx, id)
		if err == nil && ok && rec.Metadata.RuntimeHandleID != "" {
			output, outputErr := m.readDiagnosticOutput(ctx, ports.RuntimeHandle{ID: rec.Metadata.RuntimeHandleID})
			if outputErr == nil {
				tail = domain.SanitizeDiagnosticTail(output)
				captured = tail != ""
			}
		}
	}
	if !captured && errorType == "" {
		return nil
	}
	return &domain.LifecycleDiagnostic{
		Trigger:       trigger,
		TerminalTail:  tail,
		HookErrorType: errorType,
		CapturedAt:    m.clock().UTC(),
	}
}

func (m *Manager) readDiagnosticOutput(ctx context.Context, handle ports.RuntimeHandle) (string, error) {
	captureCtx, cancel := context.WithTimeout(ctx, diagnosticCaptureTimeout)
	defer cancel()
	type result struct {
		output string
		err    error
	}
	resultCh := make(chan result, 1)
	go func() {
		output, err := m.diagnosticRuntime.GetOutput(captureCtx, handle, diagnosticTailLines)
		resultCh <- result{output: output, err: err}
	}()
	select {
	case got := <-resultCh:
		return got.output, got.err
	case <-captureCtx.Done():
		return "", captureCtx.Err()
	}
}

func sameDiagnosticContent(a, b *domain.LifecycleDiagnostic) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Trigger == b.Trigger && a.TerminalTail == b.TerminalTail && a.HookErrorType == b.HookErrorType
}

func isRecoverableDiagnostic(diagnostic *domain.LifecycleDiagnostic) bool {
	if diagnostic == nil {
		return false
	}
	switch diagnostic.Trigger {
	case domain.DiagnosticBlocked, domain.DiagnosticStopFailure, domain.DiagnosticRuntimeProbeFailed, domain.DiagnosticRuntimeDead:
		return true
	default:
		return false
	}
}

func isRuntimeDiagnostic(diagnostic *domain.LifecycleDiagnostic) bool {
	if diagnostic == nil {
		return false
	}
	return diagnostic.Trigger == domain.DiagnosticRuntimeProbeFailed || diagnostic.Trigger == domain.DiagnosticRuntimeDead
}

// sameActivity reports whether two activity signals describe the same state.
// LastActivityAt is intentionally ignored: same-state repeats (e.g. a stream
// of idle notifications) must not rewrite UpdatedAt or fan out a CDC event.
// LastActivityAt now marks when this state was first entered since the last
// transition, which is the timestamp a UI actually wants.
func sameActivity(a, b domain.Activity) bool {
	return a.State == b.State
}

func mergeMetadata(base, in domain.SessionMetadata) domain.SessionMetadata {
	set := func(dst *string, v string) {
		if v != "" {
			*dst = v
		}
	}
	set(&base.Branch, in.Branch)
	set(&base.WorkspacePath, in.WorkspacePath)
	set(&base.RuntimeHandleID, in.RuntimeHandleID)
	set(&base.AgentSessionID, in.AgentSessionID)
	set(&base.Prompt, in.Prompt)
	return base
}

// applyPendingSubmitInvariant makes terminal/delivery-blocked transitions
// explicitly consume the latch visible in the reducer snapshot. Persistence
// compare-and-sets that exact before/after pair, so a newer concurrent latch
// is preserved for its owner rather than being cleared by a stale reduction.
func applyPendingSubmitInvariant(rec domain.SessionRecord) domain.SessionRecord {
	if rec.IsTerminated || rec.Activity.State.BlocksAutomatedDelivery() {
		rec.Metadata.PendingSubmitFingerprint = ""
		rec.Metadata.PendingSubmitRecoveryAttempted = false
	}
	return rec
}
