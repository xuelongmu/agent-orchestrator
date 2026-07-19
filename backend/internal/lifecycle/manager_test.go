package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/designcontract"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/sessionguard"
)

var ctx = context.Background()

type fakeStore struct {
	sessions                map[domain.SessionID]domain.SessionRecord
	prs                     map[domain.SessionID][]domain.PullRequest
	signatures              map[string]string
	simplificationReceipts  map[string]time.Time
	telemetryEvents         map[string]ports.TelemetryEvent
	designContracts         map[string]string
	designContractErrors    map[string]error
	missingDesignContracts  map[string]bool
	pendingContractDelivery map[string]domain.SessionID
	pendingContractTask     map[string]string
	pendingContractToken    map[string]string
	pendingContractRevision map[string]int64
	// afterSessionRead deterministically models a concurrent reducer committing
	// new activity immediately after a caller's store snapshot.
	afterSessionRead func(domain.SessionID, int)
	sessionReads     int

	signatureWriteErr error
	signatureWrites   int
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		sessions: map[domain.SessionID]domain.SessionRecord{}, prs: map[domain.SessionID][]domain.PullRequest{},
		signatures: map[string]string{}, simplificationReceipts: map[string]time.Time{}, telemetryEvents: map[string]ports.TelemetryEvent{}, designContracts: map[string]string{}, designContractErrors: map[string]error{}, missingDesignContracts: map[string]bool{}, pendingContractDelivery: map[string]domain.SessionID{}, pendingContractTask: map[string]string{}, pendingContractToken: map[string]string{}, pendingContractRevision: map[string]int64{},
	}
}

func (f *fakeStore) GetSession(_ context.Context, id domain.SessionID) (domain.SessionRecord, bool, error) {
	r, ok := f.sessions[id]
	f.sessionReads++
	if f.afterSessionRead != nil {
		f.afterSessionRead(id, f.sessionReads)
	}
	return r, ok, nil
}

func (f *fakeStore) ListPRsBySession(_ context.Context, id domain.SessionID) ([]domain.PullRequest, error) {
	return f.prs[id], nil
}

func (f *fakeStore) UpdateSession(_ context.Context, rec domain.SessionRecord) error {
	f.sessions[rec.ID] = rec
	return nil
}

func (f *fakeStore) UpdateSessionLifecycle(ctx context.Context, _, after domain.SessionRecord) error {
	return f.UpdateSession(ctx, after)
}

func (f *fakeStore) GetPRLastNudgeSignature(_ context.Context, prURL string) (string, error) {
	return f.signatures[prURL], nil
}

func (f *fakeStore) UpdatePRLastNudgeSignature(_ context.Context, prURL, payload string) error {
	if f.signatureWriteErr != nil {
		return f.signatureWriteErr
	}
	if f.signatures == nil {
		f.signatures = map[string]string{}
	}
	f.signatures[prURL] = payload
	f.signatureWrites++
	return nil
}

func (f *fakeStore) GetPRDesignContract(_ context.Context, prURL string) (string, bool, error) {
	if err := f.designContractErrors[prURL]; err != nil {
		return "", false, err
	}
	if f.missingDesignContracts[prURL] {
		return "", false, nil
	}
	contract, ok := f.designContracts[prURL]
	if !ok {
		return designcontract.BuildSeed("", ""), true, nil
	}
	return contract, ok, nil
}

func (f *fakeStore) GetPendingPRDesignContractDelivery(_ context.Context, sessionID domain.SessionID, prURL string) (designcontract.PendingDelivery, bool, error) {
	pending := f.pendingContractDelivery[prURL] == sessionID
	return designcontract.PendingDelivery{Contract: f.designContracts[prURL], TaskPrompt: f.pendingContractTask[prURL], Token: f.pendingContractToken[prURL], Revision: f.pendingContractRevision[prURL]}, pending, nil
}

func (f *fakeStore) CompletePRDesignContractDelivery(_ context.Context, sessionID domain.SessionID, prURL, deliveryToken string, contractRevision int64) (bool, error) {
	if f.pendingContractDelivery[prURL] != sessionID || f.pendingContractToken[prURL] != deliveryToken || f.pendingContractRevision[prURL] != contractRevision {
		return false, nil
	}
	delete(f.pendingContractDelivery, prURL)
	delete(f.pendingContractTask, prURL)
	delete(f.pendingContractToken, prURL)
	delete(f.pendingContractRevision, prURL)
	return true, nil
}

func (f *fakeStore) EnsureReviewRunSimplificationEvent(_ context.Context, id, targetSHA string, event ports.TelemetryEvent) (ports.TelemetryEvent, bool, error) {
	key := id + "\x00" + targetSHA
	if _, ok := f.simplificationReceipts[key]; ok {
		persisted, eventOK := f.telemetryEvents[event.ID]
		if !eventOK {
			return ports.TelemetryEvent{}, false, errors.New("receipt exists without telemetry event")
		}
		return persisted, false, nil
	}
	f.simplificationReceipts[key] = event.OccurredAt
	f.telemetryEvents[event.ID] = event
	return event, true, nil
}

type fakeMessenger struct {
	msgs         []string
	err          error
	writeThenErr bool
	onSend       func()
}

type fakeAutomatedSender struct {
	msgs         []string
	err          error
	beforeReturn func()
}

type guardedAutomatedSender struct {
	guard *sessionguard.Guard
}

func (s guardedAutomatedSender) SendAutomated(ctx context.Context, id domain.SessionID, msg string) error {
	outcome, err := s.guard.DeliverAutomated(ctx, id, msg)
	if err != nil {
		return err
	}
	if outcome != sessionguard.Sent {
		return fmt.Errorf("automated delivery suppressed: %s", outcome)
	}
	return nil
}

func (f *fakeAutomatedSender) SendAutomated(_ context.Context, _ domain.SessionID, msg string) error {
	f.msgs = append(f.msgs, msg)
	if f.beforeReturn != nil {
		f.beforeReturn()
	}
	return f.err
}

type retryMergedCleaner struct {
	err   error
	calls int
}

func (c *retryMergedCleaner) CleanupMergedSession(ctx context.Context, id domain.SessionID) error {
	c.calls++
	if c.err != nil {
		return c.err
	}
	return nil
}

type retryCompletedCleaner struct {
	err   error
	calls int
}

func (c *retryCompletedCleaner) CleanupCompletedSession(context.Context, domain.SessionID) error {
	c.calls++
	return c.err
}

type blockingMergedCleaner struct {
	entered chan struct{}
	release chan struct{}
	calls   int
}

func (c *blockingMergedCleaner) CleanupMergedSession(ctx context.Context, id domain.SessionID) error {
	c.calls++
	close(c.entered)
	select {
	case <-c.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type fakeDiagnosticRuntime struct {
	output string
	err    error
	calls  int
}

func (r *fakeDiagnosticRuntime) GetOutput(_ context.Context, _ ports.RuntimeHandle, lines int) (string, error) {
	r.calls++
	if lines != diagnosticTailLines {
		return "", fmt.Errorf("lines = %d, want %d", lines, diagnosticTailLines)
	}
	return r.output, r.err
}

type telemetrySink struct {
	events []ports.TelemetryEvent
}

type droppingTelemetrySink struct{}

func (droppingTelemetrySink) Emit(context.Context, ports.TelemetryEvent) {}
func (droppingTelemetrySink) Close(context.Context) error                { return nil }
func (droppingTelemetrySink) DurableLocalTelemetry() bool                { return true }

func (s *telemetrySink) Emit(_ context.Context, ev ports.TelemetryEvent) {
	if ev.ID != "" {
		for _, existing := range s.events {
			if existing.ID == ev.ID {
				return
			}
		}
	}
	s.events = append(s.events, ev)
}

func (*telemetrySink) Close(context.Context) error { return nil }

func (*telemetrySink) DurableLocalTelemetry() bool { return true }

func (f *fakeMessenger) Send(_ context.Context, _ domain.SessionID, msg string) error {
	if f.onSend != nil {
		f.onSend()
	}
	if f.err != nil {
		if f.writeThenErr {
			f.msgs = append(f.msgs, msg)
		}
		return f.err
	}
	f.msgs = append(f.msgs, msg)
	return nil
}

func (f *fakeMessenger) SendAutomated(ctx context.Context, id domain.SessionID, msg string) error {
	return f.Send(ctx, id, msg)
}

type fakeReactionReservation struct {
	owner     string
	phase     string
	signature string
	attempts  int
	prior     string
	fences    []ports.PRReactionFence
}

// reservationFakeStore adds the production pre-send contract to fakeStore so
// lifecycle tests can deterministically inspect the crash boundary.
type reservationFakeStore struct {
	*fakeStore
	mu           sync.Mutex
	reservations map[string]fakeReactionReservation
	commitErr    error
	beforeStart  func()
	heads        map[string]string
	owners       map[string]domain.SessionID
}

func newReservationFakeStore() *reservationFakeStore {
	return &reservationFakeStore{
		fakeStore: newFakeStore(), reservations: map[string]fakeReactionReservation{},
		heads: map[string]string{}, owners: map[string]domain.SessionID{},
	}
}

func (s *reservationFakeStore) ReservePRReaction(_ context.Context, prURL, key, signature string, maxAttempts int, ownerToken string, fences []ports.PRReactionFence, _, _ time.Time) (ports.PRReactionReservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, fence := range fences {
		if head, ok := s.heads[fence.PRURL]; ok && head != fence.HeadSHA {
			return ports.PRReactionReservation{Status: ports.PRReactionStale}, nil
		}
		if owner, ok := s.owners[fence.PRURL]; ok && owner != fence.SessionID {
			return ports.PRReactionReservation{Status: ports.PRReactionStale}, nil
		}
	}
	reservationKey := prURL + "\x00" + key
	if current, ok := s.reservations[reservationKey]; ok {
		if current.phase == "started" {
			return ports.PRReactionReservation{Status: ports.PRReactionUncertain, Signature: current.signature, Attempts: current.attempts}, nil
		}
		return ports.PRReactionReservation{Status: ports.PRReactionBusy, Signature: current.signature}, nil
	}
	var payload reactionPayload
	_ = json.Unmarshal([]byte(s.signatures[prURL]), &payload)
	if payload.Seen == nil {
		payload.Seen = map[string]string{}
	}
	if payload.Attempts == nil {
		payload.Attempts = map[string]int{}
	}
	if payload.Seen[key] == signature {
		return ports.PRReactionReservation{Status: ports.PRReactionAccounted, Signature: signature, Attempts: payload.Attempts[key]}, nil
	}
	attempts := payload.Attempts[key]
	if prior, ok := payload.Seen[key]; ok && prior != signature {
		attempts = 0
	}
	if maxAttempts > 0 && attempts >= maxAttempts {
		return ports.PRReactionReservation{Status: ports.PRReactionExhausted, Signature: payload.Seen[key], Attempts: attempts}, nil
	}
	s.reservations[reservationKey] = fakeReactionReservation{owner: ownerToken, phase: "reserved", signature: signature, attempts: attempts + 1, prior: s.signatures[prURL], fences: append([]ports.PRReactionFence(nil), fences...)}
	return ports.PRReactionReservation{Status: ports.PRReactionReserved, Signature: signature, Attempts: attempts + 1}, nil
}

func (s *reservationFakeStore) StartPRReaction(_ context.Context, prURL, key, ownerToken string, _, _ time.Time) (ports.PRReactionReservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	reservationKey := prURL + "\x00" + key
	reservation, ok := s.reservations[reservationKey]
	if !ok || reservation.owner != ownerToken || reservation.phase != "reserved" {
		return ports.PRReactionReservation{Status: ports.PRReactionBusy}, nil
	}
	if s.beforeStart != nil {
		s.beforeStart()
	}
	for _, fence := range reservation.fences {
		if head, ok := s.heads[fence.PRURL]; ok && head != fence.HeadSHA {
			delete(s.reservations, reservationKey)
			return ports.PRReactionReservation{Status: ports.PRReactionStale}, nil
		}
		if owner, ok := s.owners[fence.PRURL]; ok && owner != fence.SessionID {
			delete(s.reservations, reservationKey)
			return ports.PRReactionReservation{Status: ports.PRReactionStale}, nil
		}
	}
	var payload reactionPayload
	_ = json.Unmarshal([]byte(s.signatures[prURL]), &payload)
	if payload.Seen == nil {
		payload.Seen = map[string]string{}
	}
	if payload.Attempts == nil {
		payload.Attempts = map[string]int{}
	}
	payload.Seen[key] = reservation.signature
	payload.Attempts[key] = reservation.attempts
	raw, _ := json.Marshal(payload)
	s.signatures[prURL] = string(raw)
	reservation.phase = "started"
	s.reservations[reservationKey] = reservation
	return ports.PRReactionReservation{Status: ports.PRReactionReserved, Signature: reservation.signature, Attempts: reservation.attempts}, nil
}

func (s *reservationFakeStore) CommitPRReaction(_ context.Context, prURL, key, ownerToken string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.commitErr != nil {
		return false, s.commitErr
	}
	reservationKey := prURL + "\x00" + key
	reservation, ok := s.reservations[reservationKey]
	if !ok || reservation.owner != ownerToken {
		return false, nil
	}
	delete(s.reservations, reservationKey)
	return true, nil
}

func (s *reservationFakeStore) ReleasePRReaction(_ context.Context, prURL, key, ownerToken string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	reservationKey := prURL + "\x00" + key
	reservation, ok := s.reservations[reservationKey]
	if !ok || reservation.owner != ownerToken {
		return false, nil
	}
	s.signatures[prURL] = reservation.prior
	delete(s.reservations, reservationKey)
	return true, nil
}

func newManager() (*Manager, *fakeStore, *fakeMessenger) {
	st := newFakeStore()
	msg := &fakeMessenger{}
	m := New(st, msg)
	m.SetAutomatedMessageSender(msg)
	return m, st, msg
}

func working(id domain.SessionID) domain.SessionRecord {
	return domain.SessionRecord{ID: id, ProjectID: "mer", Activity: domain.Activity{State: domain.ActivityActive, LastActivityAt: time.Now()}}
}

func TestRuntimeObservation_InferredDeathSetsTerminated(t *testing.T) {
	m, st, _ := newManager()
	cleaner := &retryCompletedCleaner{}
	m.SetCompletedSessionCleaner(cleaner)
	rec := working("mer-1")
	rec.Activity.LastActivityAt = time.Now().Add(-2 * time.Minute)
	st.sessions["mer-1"] = rec
	if err := m.ApplyRuntimeObservation(ctx, "mer-1", ports.RuntimeFacts{Probe: ports.ProbeDead}); err != nil {
		t.Fatal(err)
	}
	got := st.sessions["mer-1"]
	if !got.IsTerminated || got.Activity.State != domain.ActivityExited {
		t.Fatalf("want terminated/exited, got %+v", got)
	}
	if cleaner.calls != 1 {
		t.Fatalf("completed cleanup calls = %d, want 1", cleaner.calls)
	}
}

func TestActivityExitedRetriesCompletedCleanup(t *testing.T) {
	m, st, _ := newManager()
	rec := working("mer-1")
	st.sessions[rec.ID] = rec
	cleaner := &retryCompletedCleaner{err: errors.New("workspace busy")}
	m.SetCompletedSessionCleaner(cleaner)

	err := m.ApplyActivitySignal(ctx, rec.ID, ports.ActivitySignal{Valid: true, State: domain.ActivityExited})
	if err == nil || !strings.Contains(err.Error(), "workspace busy") {
		t.Fatalf("first cleanup error = %v, want workspace busy", err)
	}
	if got := st.sessions[rec.ID]; !got.IsTerminated || got.Activity.State != domain.ActivityExited {
		t.Fatalf("failed cleanup must preserve terminal state: %+v", got)
	}

	cleaner.err = nil
	if err := m.ApplyActivitySignal(ctx, rec.ID, ports.ActivitySignal{Valid: true, State: domain.ActivityExited}); err != nil {
		t.Fatalf("retry cleanup: %v", err)
	}
	if cleaner.calls != 2 {
		t.Fatalf("completed cleanup calls = %d, want 2", cleaner.calls)
	}
}

func TestRuntimeObservation_DeadRuntimeDoesNotTerminateRateLimitedSession(t *testing.T) {
	m, st, _ := newManager()
	rec := working("mer-1")
	rec.Activity = domain.Activity{State: domain.ActivityRateLimited, LastActivityAt: time.Now().Add(-24 * time.Hour)}
	st.sessions[rec.ID] = rec

	if err := m.ApplyRuntimeObservation(ctx, rec.ID, ports.RuntimeFacts{Probe: ports.ProbeDead}); err != nil {
		t.Fatal(err)
	}
	got := st.sessions[rec.ID]
	if got.IsTerminated || got.Activity.State != domain.ActivityRateLimited {
		t.Fatalf("rate-limited session must remain parked, got %+v", got)
	}
}

func TestRuntimeObservation_FailedProbeDoesNotMutate(t *testing.T) {
	m, st, _ := newManager()
	st.sessions["mer-1"] = working("mer-1")
	before := st.sessions["mer-1"]
	if err := m.ApplyRuntimeObservation(ctx, "mer-1", ports.RuntimeFacts{Probe: ports.ProbeFailed}); err != nil {
		t.Fatal(err)
	}
	if st.sessions["mer-1"] != before {
		t.Fatalf("failed probe should not persist a state, got %+v", st.sessions["mer-1"])
	}
}

func TestRuntimeObservation_DeadRuntimePreservesMergedCleanupForRetry(t *testing.T) {
	m, st, _ := newManager()
	rec := working("mer-1")
	rec.Activity.LastActivityAt = time.Now().Add(-2 * time.Minute)
	rec.Metadata.MergedCleanupPending = true
	rec.Metadata.MergedCleanupPRURL = "pr1"
	st.sessions[rec.ID] = rec
	st.prs[rec.ID] = []domain.PullRequest{{URL: "pr1", Merged: true}}
	cleaner := &retryMergedCleaner{}
	m.SetMergedSessionCleaner(cleaner)

	if err := m.ApplyRuntimeObservation(ctx, rec.ID, ports.RuntimeFacts{Probe: ports.ProbeDead}); err != nil {
		t.Fatal(err)
	}
	dead := st.sessions[rec.ID]
	if !dead.IsTerminated || !dead.Metadata.MergedCleanupPending {
		t.Fatalf("dead runtime must stay terminated+cleanup-pending: %+v", dead)
	}
	if err := m.RetryMergedCleanup(ctx, rec.ID); err != nil {
		t.Fatal(err)
	}
	got := st.sessions[rec.ID]
	if !got.IsTerminated || got.Metadata.MergedCleanupPending || got.Metadata.MergedCleanupPRURL != "" || cleaner.calls != 1 {
		t.Fatalf("retry must clean terminal session and clear replay context: rec=%+v calls=%d", got, cleaner.calls)
	}
}

func TestActivityExitedPreservesMergedCleanupForRetry(t *testing.T) {
	m, st, _ := newManager()
	rec := working("mer-1")
	rec.Metadata.MergedCleanupPending = true
	rec.Metadata.MergedCleanupPRURL = "pr1"
	st.sessions[rec.ID] = rec
	st.prs[rec.ID] = []domain.PullRequest{{URL: "pr1", Merged: true}}
	cleaner := &retryMergedCleaner{}
	m.SetMergedSessionCleaner(cleaner)

	if err := m.ApplyActivitySignal(ctx, rec.ID, ports.ActivitySignal{Valid: true, State: domain.ActivityExited}); err != nil {
		t.Fatal(err)
	}
	exited := st.sessions[rec.ID]
	if !exited.IsTerminated || !exited.Metadata.MergedCleanupPending {
		t.Fatalf("agent exit must stay terminated+cleanup-pending: %+v", exited)
	}
	if err := m.RetryMergedCleanup(ctx, rec.ID); err != nil {
		t.Fatal(err)
	}
	got := st.sessions[rec.ID]
	if !got.IsTerminated || got.Metadata.MergedCleanupPending || cleaner.calls != 1 {
		t.Fatalf("retry must clean exited session: rec=%+v calls=%d", got, cleaner.calls)
	}
}

func TestRuntimeObservation_FailedProbeCapturesSafeDiagnosticWithoutChangingLifecycle(t *testing.T) {
	st := newFakeStore()
	rt := &fakeDiagnosticRuntime{output: "\x1b[31mprobe failed\x1b[0m\nGITHUB_TOKEN=ghp_supersecret\nretrying"}
	m := New(st, nil, WithDiagnosticRuntime(rt))
	rec := working("mer-1")
	rec.Metadata.RuntimeHandleID = "runtime-1"
	st.sessions[rec.ID] = rec

	if err := m.ApplyRuntimeObservation(ctx, rec.ID, ports.RuntimeFacts{Probe: ports.ProbeFailed}); err != nil {
		t.Fatal(err)
	}
	got := st.sessions[rec.ID]
	if got.IsTerminated || got.Activity != rec.Activity {
		t.Fatalf("probe diagnostic changed lifecycle: got %+v, want activity %+v", got, rec.Activity)
	}
	if got.Diagnostic == nil || got.Diagnostic.Trigger != domain.DiagnosticRuntimeProbeFailed {
		t.Fatalf("diagnostic = %#v, want runtime probe failure", got.Diagnostic)
	}
	if strings.Contains(got.Diagnostic.TerminalTail, "\x1b") || strings.Contains(got.Diagnostic.TerminalTail, "supersecret") {
		t.Fatalf("diagnostic was not scrubbed: %q", got.Diagnostic.TerminalTail)
	}
	if !strings.Contains(got.Diagnostic.TerminalTail, "GITHUB_TOKEN=[REDACTED]") {
		t.Fatalf("diagnostic did not preserve useful redacted context: %q", got.Diagnostic.TerminalTail)
	}
}

func TestRuntimeObservation_DiagnosticCaptureFailureDoesNotBlockTerminalTransition(t *testing.T) {
	st := newFakeStore()
	rt := &fakeDiagnosticRuntime{err: errors.New("runtime output unavailable")}
	m := New(st, nil, WithDiagnosticRuntime(rt))
	rec := working("mer-1")
	rec.Activity.LastActivityAt = time.Now().Add(-2 * time.Minute)
	rec.Metadata.RuntimeHandleID = "runtime-1"
	st.sessions[rec.ID] = rec

	if err := m.ApplyRuntimeObservation(ctx, rec.ID, ports.RuntimeFacts{Probe: ports.ProbeDead}); err != nil {
		t.Fatal(err)
	}
	got := st.sessions[rec.ID]
	if !got.IsTerminated || got.Activity.State != domain.ActivityExited {
		t.Fatalf("capture failure blocked terminal transition: %+v", got)
	}
	if got.Diagnostic != nil {
		t.Fatalf("failed capture persisted a fabricated diagnostic: %#v", got.Diagnostic)
	}
}

func TestRuntimeObservation_EmptyDiagnosticCaptureDoesNotPersist(t *testing.T) {
	st := newFakeStore()
	rt := &fakeDiagnosticRuntime{output: "\x1b[31m\x1b[0m"}
	m := New(st, nil, WithDiagnosticRuntime(rt))
	rec := working("mer-1")
	rec.Metadata.RuntimeHandleID = "runtime-1"
	st.sessions[rec.ID] = rec

	if err := m.ApplyRuntimeObservation(ctx, rec.ID, ports.RuntimeFacts{Probe: ports.ProbeFailed}); err != nil {
		t.Fatal(err)
	}
	if got := st.sessions[rec.ID].Diagnostic; got != nil {
		t.Fatalf("empty capture persisted a diagnostic: %#v", got)
	}
}

func TestActivity_StopFailureCapturesHookErrorAndTerminalTail(t *testing.T) {
	st := newFakeStore()
	rt := &fakeDiagnosticRuntime{output: "Hook stopped: validation failed"}
	m := New(st, nil, WithDiagnosticRuntime(rt))
	rec := working("mer-1")
	rec.Metadata.RuntimeHandleID = "runtime-1"
	st.sessions[rec.ID] = rec

	if err := m.ApplyActivitySignal(ctx, rec.ID, ports.ActivitySignal{
		Valid: true, State: domain.ActivityIdle, Event: "stop-failure", ErrorType: "validation_failed",
	}); err != nil {
		t.Fatal(err)
	}
	got := st.sessions[rec.ID]
	if got.Activity.State != domain.ActivityIdle {
		t.Fatalf("activity = %q, want idle", got.Activity.State)
	}
	if got.Diagnostic == nil || got.Diagnostic.Trigger != domain.DiagnosticStopFailure || got.Diagnostic.HookErrorType != "validation_failed" {
		t.Fatalf("diagnostic = %#v", got.Diagnostic)
	}
	if got.Diagnostic.TerminalTail != "Hook stopped: validation failed" {
		t.Fatalf("terminal tail = %q", got.Diagnostic.TerminalTail)
	}
}

func TestActivity_AuthoritativeRecoveryClearsOnlyTransientDiagnostics(t *testing.T) {
	tests := []struct {
		name        string
		trigger     domain.DiagnosticTrigger
		wantCleared bool
	}{
		{name: "blocked", trigger: domain.DiagnosticBlocked, wantCleared: true},
		{name: "stop failure", trigger: domain.DiagnosticStopFailure, wantCleared: true},
		{name: "runtime probe failure", trigger: domain.DiagnosticRuntimeProbeFailed, wantCleared: true},
		{name: "runtime dead during grace", trigger: domain.DiagnosticRuntimeDead, wantCleared: true},
		{name: "agent exited", trigger: domain.DiagnosticAgentExited, wantCleared: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, st, _ := newManager()
			rec := working("mer-1")
			rec.Activity.State = domain.ActivityBlocked
			rec.FirstSignalAt = time.Now().Add(-time.Minute)
			rec.Diagnostic = &domain.LifecycleDiagnostic{Trigger: tt.trigger, TerminalTail: "evidence", CapturedAt: time.Now()}
			st.sessions[rec.ID] = rec

			if err := m.ApplyActivitySignal(ctx, rec.ID, ports.ActivitySignal{Valid: true, State: domain.ActivityIdle, Event: "stop"}); err != nil {
				t.Fatal(err)
			}
			got := st.sessions[rec.ID]
			if got.Activity.State != domain.ActivityIdle {
				t.Fatalf("activity = %q, want idle", got.Activity.State)
			}
			if tt.wantCleared && got.Diagnostic != nil {
				t.Fatalf("transient diagnostic survived recovery: %#v", got.Diagnostic)
			}
			if !tt.wantCleared && got.Diagnostic == nil {
				t.Fatal("non-transient diagnostic was cleared")
			}
		})
	}
}

func TestRuntimeObservation_AliveClearsRecoverableDiagnostics(t *testing.T) {
	for _, trigger := range []domain.DiagnosticTrigger{
		domain.DiagnosticRuntimeProbeFailed,
		domain.DiagnosticRuntimeDead,
	} {
		t.Run(string(trigger), func(t *testing.T) {
			m, st, _ := newManager()
			rec := working("mer-1")
			rec.Diagnostic = &domain.LifecycleDiagnostic{Trigger: trigger, TerminalTail: "old evidence", CapturedAt: time.Now()}
			st.sessions[rec.ID] = rec

			if err := m.ApplyRuntimeObservation(ctx, rec.ID, ports.RuntimeFacts{Probe: ports.ProbeAlive}); err != nil {
				t.Fatal(err)
			}
			got := st.sessions[rec.ID]
			if got.Diagnostic != nil {
				t.Fatalf("recoverable diagnostic survived an alive probe: %#v", got.Diagnostic)
			}
			if got.Activity != rec.Activity || got.IsTerminated {
				t.Fatalf("alive probe changed lifecycle: got %+v, want activity %+v", got, rec.Activity)
			}
		})
	}
}

func TestActivity_InvalidIsIgnored(t *testing.T) {
	m, st, _ := newManager()
	st.sessions["mer-1"] = working("mer-1")
	before := st.sessions["mer-1"]
	if err := m.ApplyActivitySignal(ctx, "mer-1", ports.ActivitySignal{Valid: false, State: domain.ActivityIdle}); err != nil {
		t.Fatal(err)
	}
	if st.sessions["mer-1"] != before {
		t.Fatal("invalid signal must not mutate")
	}
}

func TestActivity_MetadataOnlyStoresAgentSessionIDWithoutChangingActivity(t *testing.T) {
	m, st, _ := newManager()
	rec := working("mer-1")
	rec.FirstSignalAt = time.Now().Add(-time.Minute)
	st.sessions["mer-1"] = rec

	if err := m.ApplyActivitySignal(ctx, "mer-1", ports.ActivitySignal{AgentSessionID: "native-session-1"}); err != nil {
		t.Fatal(err)
	}
	got := st.sessions["mer-1"]
	if got.Metadata.AgentSessionID != "native-session-1" {
		t.Fatalf("AgentSessionID = %q, want native-session-1", got.Metadata.AgentSessionID)
	}
	if got.Activity != rec.Activity {
		t.Fatalf("metadata-only hook changed activity: got %+v, want %+v", got.Activity, rec.Activity)
	}
	if !got.FirstSignalAt.Equal(rec.FirstSignalAt) {
		t.Fatalf("metadata-only hook changed FirstSignalAt: got %v, want %v", got.FirstSignalAt, rec.FirstSignalAt)
	}
}

func TestActivity_SameStateSignalStillStoresAgentSessionID(t *testing.T) {
	m, st, _ := newManager()
	rec := working("mer-1")
	rec.FirstSignalAt = time.Now().Add(-time.Minute)
	st.sessions["mer-1"] = rec

	if err := m.ApplyActivitySignal(ctx, "mer-1", ports.ActivitySignal{
		Valid:          true,
		State:          rec.Activity.State,
		AgentSessionID: "native-session-1",
	}); err != nil {
		t.Fatal(err)
	}
	got := st.sessions["mer-1"]
	if got.Metadata.AgentSessionID != "native-session-1" {
		t.Fatalf("AgentSessionID = %q, want native-session-1", got.Metadata.AgentSessionID)
	}
	if got.Activity != rec.Activity {
		t.Fatalf("same-state metadata signal changed activity: got %+v, want %+v", got.Activity, rec.Activity)
	}
}

func TestActivity_SameStateNonWaitingSignalClearsPendingSubmit(t *testing.T) {
	m, st, _ := newManager()
	rec := working("mer-1")
	rec.FirstSignalAt = time.Now().Add(-time.Minute)
	rec.Metadata.PendingSubmitFingerprint = "sha256-prompt"
	rec.Metadata.PendingSubmitRecoveryAttempted = true
	st.sessions["mer-1"] = rec

	if err := m.ApplyActivitySignal(ctx, "mer-1", ports.ActivitySignal{
		Valid: true,
		State: rec.Activity.State,
	}); err != nil {
		t.Fatal(err)
	}
	got := st.sessions["mer-1"].Metadata
	if got.PendingSubmitFingerprint != "" || got.PendingSubmitRecoveryAttempted {
		t.Fatalf("pending-submit latch = %+v, want cleared by authoritative non-waiting activity", got)
	}
}

func TestActivity_BlankAgentSessionIDDoesNotOverwriteMetadata(t *testing.T) {
	m, st, _ := newManager()
	rec := working("mer-1")
	rec.Metadata.AgentSessionID = "existing-native-1"
	st.sessions["mer-1"] = rec

	if err := m.ApplyActivitySignal(ctx, "mer-1", ports.ActivitySignal{
		Valid:          true,
		State:          domain.ActivityActive,
		AgentSessionID: "   ",
	}); err != nil {
		t.Fatal(err)
	}
	if got := st.sessions["mer-1"].Metadata.AgentSessionID; got != "existing-native-1" {
		t.Fatalf("AgentSessionID = %q, want existing-native-1", got)
	}
}

func TestActivity_MissingSessionReturnsNotFound(t *testing.T) {
	m, _, _ := newManager()
	err := m.ApplyActivitySignal(ctx, "missing-1", ports.ActivitySignal{Valid: true, State: domain.ActivityWaitingInput})
	if !errors.Is(err, ports.ErrSessionNotFound) {
		t.Fatalf("err = %v, want ErrSessionNotFound", err)
	}
}

func TestMarkTerminated(t *testing.T) {
	m, st, _ := newManager()
	st.sessions["mer-1"] = working("mer-1")
	if err := m.MarkTerminated(ctx, "mer-1"); err != nil {
		t.Fatal(err)
	}
	got := st.sessions["mer-1"]
	if !got.IsTerminated || got.Activity.State != domain.ActivityExited {
		t.Fatalf("want terminated/exited, got %+v", got)
	}
}

func TestMarkSpawnedStoresRuntimeMetadata(t *testing.T) {
	m, st, _ := newManager()
	st.sessions["mer-1"] = working("mer-1")
	st.sessions["mer-1"] = domain.SessionRecord{ID: "mer-1", ProjectID: "mer", IsTerminated: true}
	metadata := domain.SessionMetadata{Branch: "b", WorkspacePath: "/ws", RuntimeHandleID: "h1", AgentSessionID: "agent", Prompt: "prompt"}
	if err := m.MarkSpawned(ctx, "mer-1", metadata); err != nil {
		t.Fatal(err)
	}
	got := st.sessions["mer-1"]
	if got.IsTerminated || got.Activity.State != domain.ActivityIdle || got.Metadata.RuntimeHandleID != "h1" {
		t.Fatalf("spawn metadata wrong: %+v", got)
	}
}

// TestMarkSpawned_StampsUTCActivity locks the lifecycle clock to UTC so
// activity-driven timestamps match the session manager's spawn timestamps. A
// local clock here left `ao session get` showing created in UTC but updated in
// local time.
func TestMarkSpawned_StampsUTCActivity(t *testing.T) {
	m, st, _ := newManager()
	st.sessions["mer-1"] = domain.SessionRecord{ID: "mer-1", ProjectID: "mer", IsTerminated: true}
	if err := m.MarkSpawned(ctx, "mer-1", domain.SessionMetadata{RuntimeHandleID: "h1"}); err != nil {
		t.Fatal(err)
	}
	if loc := st.sessions["mer-1"].Activity.LastActivityAt.Location(); loc != time.UTC {
		t.Fatalf("LastActivityAt location = %v, want UTC", loc)
	}
}

func TestActivity_WaitingInputEntryAndExitEmitTelemetry(t *testing.T) {
	st := newFakeStore()
	sink := &telemetrySink{}
	m := New(st, nil, WithTelemetry(sink))
	now := time.Unix(100, 0).UTC()
	m.clock = func() time.Time { return now }
	st.sessions["mer-1"] = domain.SessionRecord{
		ID:        "mer-1",
		ProjectID: "mer",
		Activity:  domain.Activity{State: domain.ActivityIdle, LastActivityAt: now.Add(-time.Minute)},
	}

	if err := m.ApplyActivitySignal(ctx, "mer-1", ports.ActivitySignal{Valid: true, State: domain.ActivityWaitingInput, Timestamp: now}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(3 * time.Second)
	if err := m.ApplyActivitySignal(ctx, "mer-1", ports.ActivitySignal{Valid: true, State: domain.ActivityActive, Timestamp: now}); err != nil {
		t.Fatal(err)
	}

	if len(sink.events) != 2 {
		t.Fatalf("events = %#v, want waiting_input entered/exited", sink.events)
	}
	if sink.events[0].Name != "ao.session.waiting_input_entered" || sink.events[1].Name != "ao.session.waiting_input_exited" {
		t.Fatalf("event names = %#v", []string{sink.events[0].Name, sink.events[1].Name})
	}
	if got := sink.events[1].Payload["dwell_ms"]; got != int64(3000) {
		t.Fatalf("dwell_ms = %#v, want 3000", got)
	}
}

func TestPRObservation_CIFailingNudgesAgentWithLogs(t *testing.T) {
	m, st, msg := newManager()
	st.sessions["mer-1"] = working("mer-1")
	o := ports.PRObservation{Fetched: true, URL: "pr1", HeadSHA: "c1", CI: domain.CIFailing, Checks: []ports.PRCheckObservation{
		{Name: "build", CommitHash: "c1", Status: domain.PRCheckFailed, URL: "https://ci.example/build", LogTail: "boom"},
		{Name: "lint", CommitHash: "c1", Status: domain.PRCheckCancelled, URL: "https://ci.example/lint"},
	}}
	if err := m.ApplyPRObservation(ctx, "mer-1", o); err != nil {
		t.Fatal(err)
	}
	if len(msg.msgs) != 1 {
		t.Fatalf("want one CI nudge with log tail, got %v", msg.msgs)
	}
	for _, want := range []string{
		"CI is failing on your PR.",
		"Failed: build (failed)",
		"Failure URL: https://ci.example/build",
		"Log tail (last 1 line):",
		"boom",
		"fetch full CI logs only if you need additional context",
	} {
		if !strings.Contains(msg.msgs[0], want) {
			t.Fatalf("CI nudge missing %q:\n%s", want, msg.msgs[0])
		}
	}
	if strings.Contains(msg.msgs[0], "lint") || strings.Contains(msg.msgs[0], "cancelled") {
		t.Fatalf("cancelled checks must not be included in CI nudge:\n%s", msg.msgs[0])
	}
}

func TestPRObservation_CancelledChecksDoNotNudge(t *testing.T) {
	m, st, msg := newManager()
	st.sessions["mer-1"] = working("mer-1")
	o := ports.PRObservation{Fetched: true, URL: "pr1", CI: domain.CIFailing, Checks: []ports.PRCheckObservation{
		{Name: "lint", CommitHash: "c1", Status: domain.PRCheckCancelled, URL: "https://ci.example/lint"},
	}}
	if err := m.ApplyPRObservation(ctx, "mer-1", o); err != nil {
		t.Fatal(err)
	}
	if len(msg.msgs) != 0 {
		t.Fatalf("cancelled-only checks must not nudge, got %v", msg.msgs)
	}
}

func TestReviewCommentsSignatureTracksContentNotCount(t *testing.T) {
	original := []ports.PRCommentObservation{
		{ID: "c1", ThreadID: "t1", Author: "alice", File: "old.go", Line: 10, Body: "old", URL: "https://old"},
		{ID: "c2", ThreadID: "t2", Author: "bob", File: "old.go", Line: 20, Body: "old", URL: "https://old"},
	}
	reordered := []ports.PRCommentObservation{
		{ID: "c2", ThreadID: "t2", Author: "bob", File: "old.go", Line: 20, Body: "old", URL: "https://new"},
		{ID: "c1", ThreadID: "t1", Author: "alice", File: "old.go", Line: 10, Body: "old", URL: "https://new"},
	}
	if got, want := reviewCommentsSignature(reordered), reviewCommentsSignature(original); got != want {
		t.Fatalf("signature changed after reorder/link refresh\n got %q\nwant %q", got, want)
	}

	edited := append([]ports.PRCommentObservation(nil), original...)
	edited[0].Body = "new actionable detail"
	if got, old := reviewCommentsSignature(edited), reviewCommentsSignature(original); got == old {
		t.Fatalf("edited comment body should change signature, got %q", got)
	}

	replacedAtSameCount := append([]ports.PRCommentObservation(nil), original...)
	replacedAtSameCount[0] = ports.PRCommentObservation{ID: "c3", ThreadID: "t3", Author: "carol", File: "new.go", Line: 30, Body: "replacement"}
	if got, old := reviewCommentsSignature(replacedAtSameCount), reviewCommentsSignature(original); got == old {
		t.Fatalf("same-count replacement should change signature, got %q", got)
	}

	withoutProviderIDs := []ports.PRCommentObservation{{File: "fallback.go", Line: 7, Body: "first"}}
	editedWithoutProviderIDs := []ports.PRCommentObservation{{File: "fallback.go", Line: 7, Body: "second"}}
	if got, old := reviewCommentsSignature(editedWithoutProviderIDs), reviewCommentsSignature(withoutProviderIDs); got == old {
		t.Fatalf("content-only comments should still have distinct signatures, got %q", got)
	}
}

func TestFormatCIFailureMessageUsesNonMutatingFence(t *testing.T) {
	logTail := "start\n```\ninner\n````\nend"
	msg := formatCIFailureMessage([]ports.PRCheckObservation{{
		Name: "build", Status: domain.PRCheckFailed, LogTail: logTail,
	}})
	if !strings.Contains(msg, logTail) {
		t.Fatalf("message should preserve log text without zero-width mutation:\n%s", msg)
	}
	if strings.Contains(msg, "\u200b") {
		t.Fatalf("message must not insert zero-width characters:\n%s", msg)
	}
	if !strings.Contains(msg, "`````\n"+logTail+"\n`````") {
		t.Fatalf("message should wrap log in a fence longer than embedded runs:\n%s", msg)
	}
}

func TestPRObservation_ReviewCommentsNudgeAgent(t *testing.T) {
	m, st, msg := newManager()
	st.sessions["mer-1"] = working("mer-1")
	o := ports.PRObservation{Fetched: true, URL: "pr1", HeadSHA: "head-1", Review: domain.ReviewChangesRequest, Comments: []ports.PRCommentObservation{
		{ID: "1", ThreadID: "T1", Author: "alice", File: "foo.go", Line: 12, Body: "fix this", URL: "https://github.com/o/r/pull/1#discussion_r1"},
		{ID: "2", Author: "bob", Body: "already handled", Resolved: true},
	}}
	if err := m.ApplyPRObservation(ctx, "mer-1", o); err != nil {
		t.Fatal(err)
	}
	if len(msg.msgs) != 1 {
		t.Fatalf("want review nudge, got %v", msg.msgs)
	}
	for _, want := range []string{
		"The following 1 unresolved review comment(s)",
		"foo.go:12 (@alice):",
		"fix this",
		"https://github.com/o/r/pull/1#discussion_r1",
		"Thread ID: T1",
		"re-fetch review data unless you need additional context",
	} {
		if !strings.Contains(msg.msgs[0], want) {
			t.Fatalf("review nudge missing %q:\n%s", want, msg.msgs[0])
		}
	}
	if strings.Contains(msg.msgs[0], "already handled") {
		t.Fatalf("review nudge included resolved comment:\n%s", msg.msgs[0])
	}
}

func TestPRObservation_CIFailingAndReviewBothNudge(t *testing.T) {
	m, st, msg := newManager()
	st.sessions["mer-1"] = working("mer-1")
	o := ports.PRObservation{
		Fetched:  true,
		URL:      "pr1",
		HeadSHA:  "c1",
		CI:       domain.CIFailing,
		Checks:   []ports.PRCheckObservation{{Name: "build", CommitHash: "c1", Status: domain.PRCheckFailed, LogTail: "boom"}},
		Review:   domain.ReviewChangesRequest,
		Comments: []ports.PRCommentObservation{{ID: "1", Author: "alice", Body: "fix this"}},
	}
	if err := m.ApplyPRObservation(ctx, "mer-1", o); err != nil {
		t.Fatal(err)
	}
	// Both actionable items fire — neither is suppressed by the other — in queue
	// order (CI first, then review).
	if len(msg.msgs) != 2 {
		t.Fatalf("want CI and review nudges, got %d: %v", len(msg.msgs), msg.msgs)
	}
	if !strings.Contains(msg.msgs[0], "boom") {
		t.Fatalf("first nudge should carry the CI failure, got %q", msg.msgs[0])
	}
	if !strings.Contains(msg.msgs[1], "fix this") {
		t.Fatalf("second nudge should carry the review feedback, got %q", msg.msgs[1])
	}
	// Re-observing the identical state re-nudges nothing: per-item dedup is intact.
	if err := m.ApplyPRObservation(ctx, "mer-1", o); err != nil {
		t.Fatal(err)
	}
	if len(msg.msgs) != 2 {
		t.Fatalf("re-observation should not re-nudge, got %d: %v", len(msg.msgs), msg.msgs)
	}
}

func TestPRObservation_CINudgeSanitizesLogTailControlChars(t *testing.T) {
	m, st, msg := newManager()
	st.sessions["mer-1"] = working("mer-1")
	// A CI log tail with an embedded ANSI escape sequence and a NUL byte; the
	// agent's pane must receive the visible text without the control bytes.
	o := ports.PRObservation{Fetched: true, URL: "pr1", HeadSHA: "c1", CI: domain.CIFailing, Checks: []ports.PRCheckObservation{{Name: "build", CommitHash: "c1", Status: domain.PRCheckFailed, LogTail: "line1\x1b[2Jline2\x00\ttabbed"}}}
	if err := m.ApplyPRObservation(ctx, "mer-1", o); err != nil {
		t.Fatal(err)
	}
	if len(msg.msgs) != 1 {
		t.Fatalf("want one CI nudge, got %v", msg.msgs)
	}
	got := msg.msgs[0]
	if strings.ContainsRune(got, '\x1b') || strings.ContainsRune(got, '\x00') {
		t.Fatalf("nudge still carries control bytes: %q", got)
	}
	if !strings.Contains(got, "line1") || !strings.Contains(got, "line2") || !strings.Contains(got, "\ttabbed") {
		t.Fatalf("nudge dropped visible text or tab: %q", got)
	}
}

func TestPRObservation_ReviewNudgeSanitizesCommentControlChars(t *testing.T) {
	m, st, msg := newManager()
	st.sessions["mer-1"] = working("mer-1")
	o := ports.PRObservation{Fetched: true, URL: "pr1", HeadSHA: "head-1", Review: domain.ReviewChangesRequest, Comments: []ports.PRCommentObservation{{ID: "1", Body: "please\x1b]0;pwned\afix this"}}}
	if err := m.ApplyPRObservation(ctx, "mer-1", o); err != nil {
		t.Fatal(err)
	}
	if len(msg.msgs) != 1 {
		t.Fatalf("want one review nudge, got %v", msg.msgs)
	}
	got := msg.msgs[0]
	if strings.ContainsRune(got, '\x1b') || strings.ContainsRune(got, '\a') {
		t.Fatalf("review nudge still carries control bytes: %q", got)
	}
	if !strings.Contains(got, "please") || !strings.Contains(got, "fix this") {
		t.Fatalf("review nudge dropped visible text: %q", got)
	}
}

func TestSCMObservationProjectsToExistingPRReactions(t *testing.T) {
	m, st, msg := newManager()
	st.sessions["mer-1"] = working("mer-1")
	o := ports.SCMObservation{
		Fetched: true,
		PR:      ports.SCMPRObservation{URL: "pr1", Number: 1, HeadSHA: "c1"},
		CI: ports.SCMCIObservation{
			Summary: string(domain.CIFailing),
			HeadSHA: "c1",
			FailedChecks: []ports.SCMCheckObservation{{
				Name: "build", Status: string(domain.PRCheckFailed), LogTail: "boom",
			}},
		},
	}
	if err := m.ApplySCMObservation(ctx, "mer-1", o); err != nil {
		t.Fatal(err)
	}
	if len(msg.msgs) != 1 || !strings.Contains(msg.msgs[0], "boom") {
		t.Fatalf("want SCM CI nudge with log tail, got %v", msg.msgs)
	}
}

func TestSCMObservation_MissingSessionIsIgnored(t *testing.T) {
	st := newFakeStore()
	m := New(st, nil)
	o := ports.SCMObservation{
		Fetched: true,
		PR:      ports.SCMPRObservation{URL: "pr1", Number: 1},
	}
	if err := m.ApplySCMObservation(ctx, "missing-1", o); err != nil {
		t.Fatalf("ApplySCMObservation missing session: %v", err)
	}
}

func TestSCMObservationUsesPRHeadWhenCIHeadMissing(t *testing.T) {
	m, st, msg := newManager()
	st.sessions["mer-1"] = working("mer-1")
	o := ports.SCMObservation{
		Fetched: true,
		PR:      ports.SCMPRObservation{URL: "pr1", HeadSHA: "c1"},
		CI: ports.SCMCIObservation{
			Summary: string(domain.CIFailing),
			FailedChecks: []ports.SCMCheckObservation{{
				Name: "build", Status: string(domain.PRCheckFailed),
			}},
		},
	}
	if err := m.ApplySCMObservation(ctx, "mer-1", o); err != nil {
		t.Fatal(err)
	}
	o.PR.HeadSHA = "c2"
	if err := m.ApplySCMObservation(ctx, "mer-1", o); err != nil {
		t.Fatal(err)
	}
	if len(msg.msgs) != 2 {
		t.Fatalf("want separate CI nudges for distinct PR heads when CI head is absent, got %d: %v", len(msg.msgs), msg.msgs)
	}
}

func TestPRObservation_MergeConflictNudgesAgent(t *testing.T) {
	m, st, msg := newManager()
	st.sessions["mer-1"] = working("mer-1")
	o := ports.PRObservation{
		Fetched: true, URL: "pr1", Number: 7, Title: "sync\x1b[2Jme",
		HeadSHA: "head-1\x1b[31m", SourceBranch: "feature/sync", TargetBranch: "release/1.0\x1b[2J",
		Mergeability: domain.MergeConflicting,
	}
	if err := m.ApplyPRObservation(ctx, "mer-1", o); err != nil {
		t.Fatal(err)
	}
	if len(msg.msgs) != 1 || !strings.Contains(msg.msgs[0], "merge conflicts") {
		t.Fatalf("want merge-conflict nudge, got %v", msg.msgs)
	}
	for _, want := range []string{"PR #7", "head-1[31m", "release/1.0[2J", "PR: pr1"} {
		if !strings.Contains(msg.msgs[0], want) {
			t.Fatalf("merge-conflict nudge missing %q: %q", want, msg.msgs[0])
		}
	}
	if strings.Contains(msg.msgs[0], "\x1b") {
		t.Fatalf("merge-conflict nudge contains terminal escape bytes: %q", msg.msgs[0])
	}
}

func TestPRObservation_MergeConflictRequiresExactHead(t *testing.T) {
	m, st, msg := newManager()
	st.sessions["mer-1"] = working("mer-1")
	if err := m.ApplyPRObservation(ctx, "mer-1", ports.PRObservation{
		Fetched: true, URL: "pr1", Mergeability: domain.MergeConflicting,
	}); err != nil {
		t.Fatal(err)
	}
	if len(msg.msgs) != 0 || st.signatures["pr1"] != "" {
		t.Fatalf("headless conflict must fail closed: messages=%v signature=%q", msg.msgs, st.signatures["pr1"])
	}
}

func TestPRObservation_MergeConflictDedupsPerHeadAcrossRestart(t *testing.T) {
	st := newFakeStore()
	st.sessions["mer-1"] = working("mer-1")
	o := ports.PRObservation{
		Fetched: true, URL: "pr1", HeadSHA: "head-1", TargetBranch: "main",
		Mergeability: domain.MergeConflicting,
	}
	first := &fakeMessenger{}
	if err := New(st, first).ApplyPRObservation(ctx, "mer-1", o); err != nil {
		t.Fatal(err)
	}
	if err := New(st, first).ApplyPRObservation(ctx, "mer-1", o); err != nil {
		t.Fatal(err)
	}
	if len(first.msgs) != 1 {
		t.Fatalf("same exact head re-dispatched across restart: %v", first.msgs)
	}

	o.HeadSHA = "head-2"
	if err := New(st, first).ApplyPRObservation(ctx, "mer-1", o); err != nil {
		t.Fatal(err)
	}
	if len(first.msgs) != 2 {
		t.Fatalf("new conflicting head did not get one fresh dispatch: %v", first.msgs)
	}
}

func TestPRObservation_MergeConflictRecoveryResetsSameHead(t *testing.T) {
	m, st, msg := newManager()
	st.sessions["mer-1"] = working("mer-1")
	o := ports.PRObservation{
		Fetched: true, URL: "pr1", HeadSHA: "head-1", TargetBranch: "main",
		Mergeability: domain.MergeConflicting,
	}
	if err := m.ApplyPRObservation(ctx, "mer-1", o); err != nil {
		t.Fatal(err)
	}
	// UNKNOWN is a transient provider recompute, not proof that conflicts
	// recovered, so it must not reopen the same exact-head dispatch budget.
	o.Mergeability = domain.MergeUnknown
	if err := m.ApplyPRObservation(ctx, "mer-1", o); err != nil {
		t.Fatal(err)
	}
	o.Mergeability = domain.MergeConflicting
	if err := m.ApplyPRObservation(ctx, "mer-1", o); err != nil {
		t.Fatal(err)
	}
	if len(msg.msgs) != 1 {
		t.Fatalf("transient unknown mergeability reopened conflict episode: %v", msg.msgs)
	}
	o.Mergeability = domain.MergeMergeable
	if err := m.ApplyPRObservation(ctx, "mer-1", o); err != nil {
		t.Fatal(err)
	}
	m = New(st, msg) // recovery reset must survive a daemon restart
	o.Mergeability = domain.MergeConflicting
	if err := m.ApplyPRObservation(ctx, "mer-1", o); err != nil {
		t.Fatal(err)
	}
	if len(msg.msgs) != 2 {
		t.Fatalf("same head must dispatch again after a confirmed recovery: %v", msg.msgs)
	}
}

func TestSCMObservation_MergeConflictCarriesExactPRHead(t *testing.T) {
	m, st, msg := newManager()
	st.sessions["mer-1"] = working("mer-1")
	o := ports.SCMObservation{
		Fetched: true,
		PR: ports.SCMPRObservation{
			URL: "pr1", Number: 9, HeadSHA: "provider-head", TargetBranch: "develop",
		},
		Mergeability: ports.SCMMergeabilityObservation{State: string(domain.MergeConflicting)},
	}
	if err := m.ApplySCMObservation(ctx, "mer-1", o); err != nil {
		t.Fatal(err)
	}
	if len(msg.msgs) != 1 || !strings.Contains(msg.msgs[0], "Exact head: provider-head") {
		t.Fatalf("SCM conflict dispatch was not bound to provider head: %v", msg.msgs)
	}
}

func TestPRObservation_MergeConflictWaitsForWorkableSession(t *testing.T) {
	m, st, msg := newManager()
	rec := working("mer-1")
	rec.Activity.State = domain.ActivityWaitingInput
	st.sessions["mer-1"] = rec
	o := ports.PRObservation{
		Fetched: true, URL: "pr1", HeadSHA: "head-1", TargetBranch: "main",
		Mergeability: domain.MergeConflicting,
	}
	if err := m.ApplyPRObservation(ctx, "mer-1", o); err != nil {
		t.Fatal(err)
	}
	if len(msg.msgs) != 0 || st.signatures["pr1"] != "" {
		t.Fatalf("waiting-input session consumed conflict dispatch: messages=%v signature=%q", msg.msgs, st.signatures["pr1"])
	}
	st.sessions["mer-1"] = working("mer-1")
	if err := m.ApplyPRObservation(ctx, "mer-1", o); err != nil {
		t.Fatal(err)
	}
	if len(msg.msgs) != 1 {
		t.Fatalf("conflict did not dispatch after session became workable: %v", msg.msgs)
	}
}

func TestPRObservation_NudgeIncludesPRIdentity(t *testing.T) {
	m, st, msg := newManager()
	st.sessions["mer-1"] = working("mer-1")
	o := ports.PRObservation{
		Fetched:      true,
		URL:          "https://github.com/o/r/pull/7",
		HeadSHA:      "c1",
		Number:       7,
		Title:        "Add auth",
		SourceBranch: "feat/x/auth",
		TargetBranch: "feat/x",
		CI:           domain.CIFailing,
		Checks:       []ports.PRCheckObservation{{Name: "build", CommitHash: "c1", Status: domain.PRCheckFailed, LogTail: "boom"}},
	}
	if err := m.ApplyPRObservation(ctx, "mer-1", o); err != nil {
		t.Fatal(err)
	}
	if len(msg.msgs) != 1 {
		t.Fatalf("want one CI nudge, got %d: %v", len(msg.msgs), msg.msgs)
	}
	got := msg.msgs[0]
	if !strings.Contains(got, `PR #7 "Add auth" (feat/x/auth → feat/x)`) {
		t.Fatalf("nudge missing PR identity: %q", got)
	}
	if !strings.Contains(got, "PR: https://github.com/o/r/pull/7") {
		t.Fatalf("nudge missing PR URL: %q", got)
	}
}

func TestPRObservationRecoversPendingClaimContractAfterRestartBeforeOtherWork(t *testing.T) {
	st := newFakeStore()
	prURL := "https://github.com/o/r/pull/7"
	rec := working("mer-1")
	rec.Metadata.WorkspacePath = t.TempDir() // non-git: projection failure must not clear or block delivery
	st.sessions[rec.ID] = rec
	st.designContracts[prURL] = "# Design Contract\n\n## Invariants\n- Preserve predecessor knowledge.\x1b[31m\n"
	st.pendingContractDelivery[prURL] = rec.ID
	st.pendingContractTask[prURL] = "Fix the exact claimed PR."
	observation := ports.PRObservation{Fetched: true, URL: prURL, HeadSHA: "head-7", CI: domain.CIFailing, Checks: []ports.PRCheckObservation{{Name: "build", CommitHash: "head-7", Status: domain.PRCheckFailed}}}

	failedMessenger := &fakeMessenger{err: errors.New("pane unavailable")}
	first := New(st, failedMessenger)
	first.SetAutomatedMessageSender(failedMessenger)
	if err := first.ApplyPRObservation(context.Background(), rec.ID, observation); err == nil {
		t.Fatal("failed claim-ready delivery did not remain retryable")
	}
	if st.pendingContractDelivery[prURL] != rec.ID || len(failedMessenger.msgs) != 0 || st.signatures[prURL] != "" {
		t.Fatalf("failed barrier allowed work or acknowledgement: pending=%q msgs=%v sig=%q", st.pendingContractDelivery[prURL], failedMessenger.msgs, st.signatures[prURL])
	}

	// A fresh manager models daemon restart: no in-memory reaction state is
	// carried over, so recovery must come entirely from the durable obligation.
	recoveredMessenger := &fakeMessenger{}
	restarted := New(st, recoveredMessenger)
	restarted.SetAutomatedMessageSender(recoveredMessenger)
	if err := restarted.ApplyPRObservation(context.Background(), rec.ID, observation); err != nil {
		t.Fatal(err)
	}
	if st.pendingContractDelivery[prURL] != "" {
		t.Fatalf("restart did not clear delivered barrier: %q", st.pendingContractDelivery[prURL])
	}
	if len(recoveredMessenger.msgs) != 2 || !strings.Contains(recoveredMessenger.msgs[0], "[AO PR claim ready]") || !strings.Contains(recoveredMessenger.msgs[0], "Preserve predecessor knowledge") || !strings.Contains(recoveredMessenger.msgs[0], "Fix the exact claimed PR.") || !strings.Contains(recoveredMessenger.msgs[1], "failed") {
		t.Fatalf("delivery order after restart = %v", recoveredMessenger.msgs)
	}
	for _, forbidden := range []string{"\x1b", "\x00", "\u0085"} {
		if strings.Contains(recoveredMessenger.msgs[0], forbidden) {
			t.Fatalf("claim-ready delivery retained control %q: %q", forbidden, recoveredMessenger.msgs[0])
		}
	}
}

func TestPendingClaimContractAutomatedRetryWaitsForSafePaneState(t *testing.T) {
	for _, tc := range []struct {
		name        string
		state       domain.ActivityState
		fingerprint string
	}{
		{name: "blocked", state: domain.ActivityBlocked},
		{name: "rate limited", state: domain.ActivityRateLimited},
		{name: "pending submit", state: domain.ActivityActive, fingerprint: "sha256-draft"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newFakeStore()
			prURL := "https://github.com/o/r/pull/7"
			rec := working("mer-1")
			rec.Activity.State = tc.state
			rec.Metadata.PendingSubmitFingerprint = tc.fingerprint
			st.sessions[rec.ID] = rec
			st.designContracts[prURL] = designcontract.BuildSeed("61", "")
			st.pendingContractDelivery[prURL] = rec.ID
			msg := &fakeMessenger{}
			m := New(st, msg)
			m.SetAutomatedMessageSender(guardedAutomatedSender{guard: m.guard})
			if err := m.ApplyPRObservation(ctx, rec.ID, ports.PRObservation{Fetched: true, URL: prURL, CI: domain.CIFailing}); err != nil {
				t.Fatalf("unsafe state should remain a quiet retry condition: %v", err)
			}
			if len(msg.msgs) != 0 || st.pendingContractDelivery[prURL] != rec.ID {
				t.Fatalf("unsafe state crossed claim barrier: messages=%v pending=%q", msg.msgs, st.pendingContractDelivery[prURL])
			}
		})
	}
}

func TestPRObservation_MergedTerminatesWithoutNudge(t *testing.T) {
	m, st, msg := newManager()
	st.sessions["mer-1"] = working("mer-1")
	st.prs["mer-1"] = []domain.PullRequest{{URL: "pr1", Merged: true}}
	if err := m.ApplyPRObservation(ctx, "mer-1", ports.PRObservation{Fetched: true, URL: "pr1", Merged: true}); err != nil {
		t.Fatal(err)
	}
	got := st.sessions["mer-1"]
	if !got.IsTerminated || got.Activity.State != domain.ActivityExited {
		t.Fatalf("merged PR should terminate session, got %+v", got)
	}
	if len(msg.msgs) != 0 {
		t.Fatalf("merged PR should not send nudge, got %v", msg.msgs)
	}
}

func TestPRObservationClaimRetryRequiresConfirmedAutomatedSubmission(t *testing.T) {
	st := newFakeStore()
	prURL := "https://github.com/o/r/pull/7"
	rec := working("mer-1")
	rec.Metadata.WorkspacePath = t.TempDir()
	st.sessions[rec.ID] = rec
	st.designContracts[prURL] = designcontract.BuildSeed("61", "")
	st.pendingContractDelivery[prURL] = rec.ID
	st.pendingContractToken[prURL] = "generation-1"
	st.pendingContractTask[prURL] = strings.Repeat("x", 4096)
	raw := &fakeMessenger{}
	confirmation := &fakeAutomatedSender{
		err: errors.New("input remains an unsubmitted editor draft"),
		beforeReturn: func() {
			latched := st.sessions[rec.ID]
			latched.Metadata.PendingSubmitFingerprint = "sha256-large-claim-ready"
			st.sessions[rec.ID] = latched
		},
	}
	m := New(st, raw)
	m.SetAutomatedMessageSender(confirmation)

	if err := m.ApplyPRObservation(ctx, rec.ID, ports.PRObservation{Fetched: true, URL: prURL, CI: domain.CIFailing}); err != nil {
		t.Fatalf("latched input should remain a quiet retry condition: %v", err)
	}
	if len(confirmation.msgs) != 1 || !strings.Contains(confirmation.msgs[0], strings.Repeat("x", 4096)) {
		t.Fatalf("confirmed delivery calls = %d, want full large claim message", len(confirmation.msgs))
	}
	if len(raw.msgs) != 0 {
		t.Fatalf("claim retry bypassed confirmed sender through raw pane: %v", raw.msgs)
	}
	if st.pendingContractDelivery[prURL] != rec.ID {
		t.Fatal("unconfirmed claim retry cleared durable barrier")
	}
}

func TestPRObservationClaimRetryPropagatesUnexpectedSenderError(t *testing.T) {
	st := newFakeStore()
	prURL := "https://github.com/o/r/pull/7"
	rec := working("mer-1")
	st.sessions[rec.ID] = rec
	st.designContracts[prURL] = designcontract.BuildSeed("61", "")
	st.pendingContractDelivery[prURL] = rec.ID
	sender := &fakeAutomatedSender{err: errors.New("runtime transport failed")}
	m := New(st, &fakeMessenger{})
	m.SetAutomatedMessageSender(sender)

	err := m.ApplyPRObservation(ctx, rec.ID, ports.PRObservation{Fetched: true, URL: prURL, CI: domain.CIFailing})
	if err == nil || !strings.Contains(err.Error(), "runtime transport failed") {
		t.Fatalf("ApplyPRObservation error = %v, want unexpected sender failure", err)
	}
	if st.pendingContractDelivery[prURL] != rec.ID {
		t.Fatal("unexpected sender failure cleared durable barrier")
	}
}

func TestPRObservation_TerminalStateBypassesPendingClaimDelivery(t *testing.T) {
	for _, terminal := range []ports.PRObservation{
		{Fetched: true, URL: "pr1", Merged: true},
		{Fetched: true, URL: "pr1", Closed: true},
	} {
		name := "closed"
		if terminal.Merged {
			name = "merged"
		}
		t.Run(name, func(t *testing.T) {
			m, st, msg := newManager()
			st.sessions["mer-1"] = working("mer-1")
			st.pendingContractDelivery["pr1"] = "mer-1"
			st.pendingContractToken["pr1"] = "generation-1"
			st.designContracts["pr1"] = designcontract.BuildSeed("61", "")
			st.prs["mer-1"] = []domain.PullRequest{{URL: "pr1", Merged: terminal.Merged, Closed: terminal.Closed}}

			if err := m.ApplyPRObservation(ctx, "mer-1", terminal); err != nil {
				t.Fatal(err)
			}
			if len(msg.msgs) != 0 {
				t.Fatalf("terminal observation attempted claim-ready delivery: %v", msg.msgs)
			}
			if terminal.Merged && !st.sessions["mer-1"].IsTerminated {
				t.Fatalf("merged observation did not complete session: %+v", st.sessions["mer-1"])
			}
		})
	}
}

func TestPRObservation_MergedCleanupFailureRetriesFromDurableLatch(t *testing.T) {
	m, st, _ := newManager()
	st.sessions["mer-1"] = working("mer-1")
	st.prs["mer-1"] = []domain.PullRequest{{URL: "pr1", Merged: true}}
	cleaner := &retryMergedCleaner{err: errors.New("runtime busy")}
	m.SetMergedSessionCleaner(cleaner)

	err := m.ApplyPRObservation(ctx, "mer-1", ports.PRObservation{Fetched: true, URL: "pr1", Merged: true})
	if err == nil || !strings.Contains(err.Error(), "runtime busy") {
		t.Fatalf("ApplyPRObservation error = %v, want runtime busy", err)
	}
	failed := st.sessions["mer-1"]
	if !failed.IsTerminated || failed.Activity.State != domain.ActivityExited || !failed.Metadata.MergedCleanupPending {
		t.Fatalf("failed cleanup must stay terminal and durably pending: %+v", failed)
	}
	if failed.Metadata.MergedCleanupPRURL != "pr1" {
		t.Fatalf("failed cleanup trigger URL = %q, want pr1", failed.Metadata.MergedCleanupPRURL)
	}

	cleaner.err = nil
	if err := m.RetryMergedCleanup(ctx, "mer-1"); err != nil {
		t.Fatalf("RetryMergedCleanup: %v", err)
	}
	got := st.sessions["mer-1"]
	if !got.IsTerminated || got.Metadata.MergedCleanupPending || got.Metadata.MergedCleanupPRURL != "" {
		t.Fatalf("successful retry must terminate and clear latch: %+v", got)
	}
	if cleaner.calls != 2 {
		t.Fatalf("cleanup calls = %d, want 2", cleaner.calls)
	}
	if err := m.ApplyPRObservation(ctx, "mer-1", ports.PRObservation{Fetched: true, URL: "pr1", Merged: true}); err != nil {
		t.Fatalf("repeat terminal observation: %v", err)
	}
	if cleaner.calls != 2 {
		t.Fatalf("terminal re-observation repeated cleanup: calls = %d, want 2", cleaner.calls)
	}
}

func TestPRObservation_MergedCleanupWaitsWhileRateLimited(t *testing.T) {
	m, st, _ := newManager()
	rec := working("mer-1")
	rec.Activity.State = domain.ActivityRateLimited
	st.sessions[rec.ID] = rec
	st.prs[rec.ID] = []domain.PullRequest{{URL: "pr1", Merged: true}}
	cleaner := &retryMergedCleaner{}
	m.SetMergedSessionCleaner(cleaner)

	if err := m.ApplyPRObservation(ctx, rec.ID, ports.PRObservation{Fetched: true, URL: "pr1", Merged: true}); err != nil {
		t.Fatal(err)
	}
	got := st.sessions[rec.ID]
	if cleaner.calls != 0 || got.IsTerminated || !got.Metadata.MergedCleanupPending || got.Metadata.MergedCleanupPRURL != "pr1" || got.Activity.State != domain.ActivityRateLimited {
		t.Fatalf("rate-limited merged cleanup mutated session: rec=%+v cleaner calls=%d", got, cleaner.calls)
	}

	// The provider no longer returns the terminal PR on subsequent open-PR
	// polls. The durable latch must still survive recovery and drive cleanup.
	st.prs[rec.ID] = nil
	if err := m.ApplyActivitySignal(ctx, rec.ID, ports.ActivitySignal{Valid: true, Event: "user-prompt-submit", State: domain.ActivityActive}); err != nil {
		t.Fatal(err)
	}
	if err := m.RetryMergedCleanup(ctx, rec.ID); err != nil {
		t.Fatal(err)
	}
	got = st.sessions[rec.ID]
	if cleaner.calls != 1 || !got.IsTerminated || got.Activity.State != domain.ActivityExited || got.Metadata.MergedCleanupPending || got.Metadata.MergedCleanupPRURL != "" {
		t.Fatalf("recovered cleanup did not replay after provider dropped PR: rec=%+v cleaner calls=%d", got, cleaner.calls)
	}
}

func TestRetryMergedCleanupWaitsWhileRateLimited(t *testing.T) {
	m, st, _ := newManager()
	rec := working("mer-1")
	rec.Activity.State = domain.ActivityRateLimited
	rec.Metadata.MergedCleanupPending = true
	rec.Metadata.MergedCleanupPRURL = "pr1"
	st.sessions[rec.ID] = rec
	st.prs[rec.ID] = []domain.PullRequest{{URL: "pr1", Merged: true}}
	cleaner := &retryMergedCleaner{}
	m.SetMergedSessionCleaner(cleaner)

	if err := m.RetryMergedCleanup(ctx, rec.ID); err != nil {
		t.Fatal(err)
	}
	got := st.sessions[rec.ID]
	if cleaner.calls != 0 || got.IsTerminated || !got.Metadata.MergedCleanupPending || got.Activity.State != domain.ActivityRateLimited {
		t.Fatalf("rate-limited cleanup retry mutated session: rec=%+v cleaner calls=%d", got, cleaner.calls)
	}
}

func TestMergedCleanupReservationWinsConcurrentRateLimitWithoutHoldingLifecycleLock(t *testing.T) {
	m, st, _ := newManager()
	st.sessions["mer-1"] = working("mer-1")
	st.prs["mer-1"] = []domain.PullRequest{{URL: "pr1", Merged: true}}
	cleaner := &blockingMergedCleaner{entered: make(chan struct{}), release: make(chan struct{})}
	m.SetMergedSessionCleaner(cleaner)

	cleanupDone := make(chan error, 1)
	go func() {
		cleanupDone <- m.ApplyPRObservation(ctx, "mer-1", ports.PRObservation{Fetched: true, URL: "pr1", Merged: true})
	}()
	select {
	case <-cleaner.entered:
	case <-time.After(time.Second):
		t.Fatal("cleanup did not reach resource callback")
	}
	reserved := st.sessions["mer-1"]
	if !reserved.IsTerminated || reserved.Activity.State != domain.ActivityExited || !reserved.Metadata.MergedCleanupPending {
		t.Fatalf("resource cleanup must start after durable terminal reservation: %+v", reserved)
	}

	// The cleaner is still blocked on external I/O. Activity delivery must not
	// block behind it, and the already-terminal reservation must win without a
	// rate-limited rewrite.
	signalDone := make(chan error, 1)
	go func() {
		signalDone <- m.ApplyActivitySignal(ctx, "mer-1", ports.ActivitySignal{Valid: true, Event: "stop-failure", ErrorType: "rate_limit", State: domain.ActivityIdle})
	}()
	select {
	case err := <-signalDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("activity signal blocked behind external cleanup")
	}
	if got := st.sessions["mer-1"]; !got.IsTerminated || got.Activity.State != domain.ActivityExited || !got.Metadata.MergedCleanupPending {
		t.Fatalf("concurrent rate-limit rewrote terminal reservation: %+v", got)
	}

	close(cleaner.release)
	if err := <-cleanupDone; err != nil {
		t.Fatal(err)
	}
	got := st.sessions["mer-1"]
	if !got.IsTerminated || got.Activity.State != domain.ActivityExited || got.Metadata.MergedCleanupPending || got.Metadata.MergedCleanupPRURL != "" || cleaner.calls != 1 {
		t.Fatalf("completed cleanup = %+v, calls=%d", got, cleaner.calls)
	}
}

func TestPRObservation_MergedCleanupRetryResumesTerminalNotification(t *testing.T) {
	cases := []struct {
		name     string
		prs      []domain.PullRequest
		obs      ports.SCMObservation
		wantType domain.NotificationType
		wantURL  string
	}{
		{
			name:     "merged trigger",
			prs:      []domain.PullRequest{{URL: "pr1", Number: 1, Title: "merged", Merged: true, Provider: "github", Repo: "o/r"}},
			obs:      ports.SCMObservation{Fetched: true, Provider: "github", Repo: "o/r", PR: ports.SCMPRObservation{URL: "pr1", Number: 1, Merged: true}},
			wantType: domain.NotificationPRMerged,
			wantURL:  "pr1",
		},
		{
			name: "closed sibling trigger after another merge",
			prs: []domain.PullRequest{
				{URL: "pr1", Number: 1, Merged: true, Provider: "github", Repo: "o/r"},
				{URL: "pr2", Number: 2, Closed: true, Provider: "github", Repo: "o/r"},
			},
			obs:      ports.SCMObservation{Fetched: true, Provider: "github", Repo: "o/r", PR: ports.SCMPRObservation{URL: "pr2", Number: 2, Closed: true}},
			wantType: domain.NotificationPRClosedUnmerged,
			wantURL:  "pr2",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newFakeStore()
			st.sessions["mer-1"] = working("mer-1")
			st.prs["mer-1"] = tc.prs
			sink := &fakeNotificationSink{}
			m := New(st, nil, WithNotificationSink(sink))
			cleaner := &retryMergedCleaner{err: errors.New("workspace busy")}
			m.SetMergedSessionCleaner(cleaner)

			if err := m.ApplySCMObservation(ctx, "mer-1", tc.obs); err == nil {
				t.Fatal("first cleanup unexpectedly succeeded")
			}
			if len(sink.intents) != 0 {
				t.Fatalf("notification emitted before cleanup success: %v", sink.intents)
			}
			cleaner.err = nil
			if err := m.RetryMergedCleanup(ctx, "mer-1"); err != nil {
				t.Fatal(err)
			}
			if len(sink.intents) != 1 || sink.intents[0].Type != tc.wantType || sink.intents[0].PRURL != tc.wantURL {
				t.Fatalf("retry intents = %+v, want one %s for %s", sink.intents, tc.wantType, tc.wantURL)
			}
			if err := m.RetryMergedCleanup(ctx, "mer-1"); err != nil {
				t.Fatal(err)
			}
			if len(sink.intents) != 1 {
				t.Fatalf("completed retry duplicated notification: %+v", sink.intents)
			}
		})
	}
}

// A session with one merged PR and one still-open PR must NOT terminate: the
// completion bar is "no open PR remains AND at least one merged".
func TestPRObservation_MergedWithOpenSiblingDoesNotTerminate(t *testing.T) {
	m, st, _ := newManager()
	st.sessions["mer-1"] = working("mer-1")
	st.prs["mer-1"] = []domain.PullRequest{
		{URL: "pr1", Merged: true},
		{URL: "pr2"},
	}
	if err := m.ApplyPRObservation(ctx, "mer-1", ports.PRObservation{Fetched: true, URL: "pr1", Merged: true}); err != nil {
		t.Fatal(err)
	}
	if got := st.sessions["mer-1"]; got.IsTerminated {
		t.Fatalf("session with an open sibling PR must stay alive, got %+v", got)
	}
}

// Once the last open PR merges (all PRs now merged), the session terminates.
func TestPRObservation_LastMergeTerminatesSession(t *testing.T) {
	m, st, _ := newManager()
	st.sessions["mer-1"] = working("mer-1")
	st.prs["mer-1"] = []domain.PullRequest{
		{URL: "pr1", Merged: true},
		{URL: "pr2", Merged: true},
	}
	if err := m.ApplyPRObservation(ctx, "mer-1", ports.PRObservation{Fetched: true, URL: "pr2", Merged: true}); err != nil {
		t.Fatal(err)
	}
	if got := st.sessions["mer-1"]; !got.IsTerminated {
		t.Fatalf("session should terminate once all PRs are merged, got %+v", got)
	}
}

// A closed PR that leaves the session with an open sibling and no merge does not
// terminate; closing the last PR with no merge also does not terminate (nothing
// shipped).
func TestPRObservation_ClosedWithoutMergeDoesNotTerminate(t *testing.T) {
	m, st, _ := newManager()
	st.sessions["mer-1"] = working("mer-1")
	st.prs["mer-1"] = []domain.PullRequest{{URL: "pr1", Closed: true}}
	if err := m.ApplyPRObservation(ctx, "mer-1", ports.PRObservation{Fetched: true, URL: "pr1", Closed: true}); err != nil {
		t.Fatal(err)
	}
	if got := st.sessions["mer-1"]; got.IsTerminated {
		t.Fatalf("a closed-without-merge PR must not terminate the session, got %+v", got)
	}
	if got := st.sessions["mer-1"]; got.Metadata.MergedCleanupPending {
		t.Fatalf("a closed-without-merge PR must not latch cleanup, got %+v", got)
	}
}

// A PR stacked on an open parent (its target branch is the parent's source
// branch) is exempt from the merge-conflict nudge: conflicts there are expected
// until the parent merges.
func TestPRObservation_StackedChildConflictSuppressed(t *testing.T) {
	m, st, msg := newManager()
	st.sessions["mer-1"] = working("mer-1")
	st.prs["mer-1"] = []domain.PullRequest{
		{URL: "parent", SourceBranch: "ao/x", TargetBranch: "main"},
		{URL: "child", SourceBranch: "ao/x/auth", TargetBranch: "ao/x"},
	}
	o := ports.PRObservation{Fetched: true, URL: "child", HeadSHA: "child-head", Mergeability: domain.MergeConflicting}
	if err := m.ApplyPRObservation(ctx, "mer-1", o); err != nil {
		t.Fatal(err)
	}
	if len(msg.msgs) != 0 {
		t.Fatalf("stacked child conflict should be suppressed, got %v", msg.msgs)
	}
}

// The bottom-of-stack PR (not stacked on any open parent) still gets the
// merge-conflict nudge even when it has open stacked children.
func TestPRObservation_BottomOfStackConflictNudges(t *testing.T) {
	m, st, msg := newManager()
	st.sessions["mer-1"] = working("mer-1")
	st.prs["mer-1"] = []domain.PullRequest{
		{URL: "parent", SourceBranch: "ao/x", TargetBranch: "main"},
		{URL: "child", SourceBranch: "ao/x/auth", TargetBranch: "ao/x"},
	}
	o := ports.PRObservation{Fetched: true, URL: "parent", HeadSHA: "parent-head", TargetBranch: "main", Mergeability: domain.MergeConflicting}
	if err := m.ApplyPRObservation(ctx, "mer-1", o); err != nil {
		t.Fatal(err)
	}
	if len(msg.msgs) != 1 || !strings.Contains(msg.msgs[0], "merge conflicts") {
		t.Fatalf("bottom-of-stack conflict should nudge, got %v", msg.msgs)
	}
}

// TestPRObservation_DedupSurvivesManagerRestart simulates a daemon restart by
// constructing a second Manager over the same store and asserts that an
// identical PR observation does not re-fire the nudge — the dedup signature
// must survive process restart, not just live in the Manager's maps.
func TestPRObservation_DedupSurvivesManagerRestart(t *testing.T) {
	st := newFakeStore()
	st.sessions["mer-1"] = working("mer-1")

	o := ports.PRObservation{
		Fetched: true,
		URL:     "https://github.com/o/r/pull/1",
		HeadSHA: "c1",
		CI:      domain.CIFailing,
		Checks:  []ports.PRCheckObservation{{Name: "build", CommitHash: "c1", Status: domain.PRCheckFailed, LogTail: "boom"}},
	}

	first := &fakeMessenger{}
	m1 := New(st, first)
	if err := m1.ApplyPRObservation(ctx, "mer-1", o); err != nil {
		t.Fatalf("first ApplyPRObservation: %v", err)
	}
	if len(first.msgs) != 1 {
		t.Fatalf("first manager: want 1 nudge, got %d", len(first.msgs))
	}
	if got := st.signatures[o.URL]; got == "" {
		t.Fatalf("signature was not persisted; want a non-empty JSON payload for %q", o.URL)
	}

	// Simulate daemon restart: the second Manager has no in-memory state but
	// shares the same store, so it should hydrate seen/attempts from the
	// persisted payload and suppress the re-send.
	second := &fakeMessenger{}
	m2 := New(st, second)
	if err := m2.ApplyPRObservation(ctx, "mer-1", o); err != nil {
		t.Fatalf("second ApplyPRObservation: %v", err)
	}
	if len(second.msgs) != 0 {
		t.Fatalf("post-restart manager re-nudged on identical observation, got %d msgs: %v", len(second.msgs), second.msgs)
	}

	// And a genuinely new signature (different log tail) still fires — proving
	// the persisted state is per-signature, not a blanket "this PR was nudged".
	o2 := o
	o2.Checks = []ports.PRCheckObservation{{Name: "build", CommitHash: "c1", Status: domain.PRCheckFailed, LogTail: "different boom"}}
	if err := m2.ApplyPRObservation(ctx, "mer-1", o2); err != nil {
		t.Fatalf("third ApplyPRObservation: %v", err)
	}
	if len(second.msgs) != 1 {
		t.Fatalf("new signature should send, got %d msgs", len(second.msgs))
	}
}

func TestPRObservation_DurableAttemptExistsBeforeMessengerWrite(t *testing.T) {
	st := newReservationFakeStore()
	st.sessions["mer-1"] = working("mer-1")
	prURL := "https://github.com/o/r/pull/28"
	o := ports.PRObservation{
		Fetched: true, URL: prURL, HeadSHA: "head-a", CI: domain.CIFailing,
		Checks: []ports.PRCheckObservation{{Name: "build", Status: domain.PRCheckFailed, LogTail: "boom"}},
	}
	msg := &fakeMessenger{}
	msg.onSend = func() {
		var payload reactionPayload
		if err := json.Unmarshal([]byte(st.signatures[prURL]), &payload); err != nil {
			t.Fatalf("decode pre-send payload: %v", err)
		}
		key := "ci:" + prURL
		if payload.Seen[key] == "" || payload.Attempts[key] != 1 {
			t.Fatalf("messenger crossed before durable attempt: %+v", payload)
		}
	}
	if err := New(st, msg).ApplyPRObservation(ctx, "mer-1", o); err != nil {
		t.Fatal(err)
	}
	if len(msg.msgs) != 1 {
		t.Fatalf("messages = %v, want one", msg.msgs)
	}
}

func TestPRObservation_BlankHeadNeverDispatchesHeadSpecificNudge(t *testing.T) {
	st := newReservationFakeStore()
	st.sessions["mer-1"] = working("mer-1")
	msg := &fakeMessenger{}
	o := ports.PRObservation{
		Fetched: true, URL: "https://github.com/o/r/pull/28", CI: domain.CIFailing,
		// Even a unanimous check commit can be stale relative to an omitted PR
		// head and must never authorize CI or review dispatch.
		Checks: []ports.PRCheckObservation{{Name: "build", CommitHash: "stale-head", Status: domain.PRCheckFailed, LogTail: "boom"}},
	}
	if err := New(st, msg).ApplyPRObservation(ctx, "mer-1", o); err != nil {
		t.Fatal(err)
	}
	if len(msg.msgs) != 0 || st.signatures[o.URL] != "" {
		t.Fatalf("blank-head reaction dispatched: messages=%v signature=%q", msg.msgs, st.signatures[o.URL])
	}
}

func TestPRObservation_WriteThenErrorStaysAccountedAcrossRestart(t *testing.T) {
	st := newReservationFakeStore()
	st.sessions["mer-1"] = working("mer-1")
	prURL := "https://github.com/o/r/pull/28"
	o := ports.PRObservation{
		Fetched: true, URL: prURL, HeadSHA: "head-a", CI: domain.CIFailing,
		Checks: []ports.PRCheckObservation{{Name: "build", CommitHash: "head-a", Status: domain.PRCheckFailed, LogTail: "boom"}},
	}
	msg := &fakeMessenger{err: errors.New("ambiguous terminal write"), writeThenErr: true}
	if err := New(st, msg).ApplyPRObservation(ctx, "mer-1", o); err == nil {
		t.Fatal("want messenger error")
	}
	if len(msg.msgs) != 1 {
		t.Fatalf("first messages = %v, want exactly one attempted write", msg.msgs)
	}
	msg.err = nil
	if err := New(st, msg).ApplyPRObservation(ctx, "mer-1", o); err != nil {
		t.Fatalf("restart observation: %v", err)
	}
	if len(msg.msgs) != 1 {
		t.Fatalf("ambiguous write retried after restart: %v", msg.msgs)
	}
}

func TestPRObservation_StartedCrashAlertsOnceAndDoesNotBlockSiblingReaction(t *testing.T) {
	st := newReservationFakeStore()
	st.sessions["mer-1"] = working("mer-1")
	prURL := "https://github.com/o/r/pull/28"
	o := ports.PRObservation{
		Fetched: true, URL: prURL, HeadSHA: "head-a", CI: domain.CIFailing, Review: domain.ReviewChangesRequest,
		Checks:   []ports.PRCheckObservation{{Name: "build", CommitHash: "head-a", Status: domain.PRCheckFailed, LogTail: "boom"}},
		Comments: []ports.PRCommentObservation{{ID: "review-1", Body: "fix review feedback"}},
	}
	key := "ci:" + prURL
	sig := ciFailureSignature(failedPRChecks(o.Checks))
	now := time.Now().UTC()
	fences := []ports.PRReactionFence{{PRURL: prURL, SessionID: "mer-1", HeadSHA: "head-a"}}
	reserved, err := st.ReservePRReaction(ctx, prURL, key, sig, 0, "crashed-owner", fences, now, now.Add(time.Minute))
	if err != nil || reserved.Status != ports.PRReactionReserved {
		t.Fatalf("reserve = %+v, err=%v", reserved, err)
	}
	started, err := st.StartPRReaction(ctx, prURL, key, "crashed-owner", now, now.Add(time.Minute))
	if err != nil || started.Status != ports.PRReactionReserved {
		t.Fatalf("start = %+v, err=%v", started, err)
	}
	msg := &fakeMessenger{}
	sink := &fakeNotificationSink{err: errors.New("notification unavailable")}
	err = New(st, msg, WithNotificationSink(sink)).ApplyPRObservation(ctx, "mer-1", o)
	if !errors.Is(err, errPRReactionHandoffPending) {
		t.Fatalf("failed uncertain handoff error = %v, want retryable pending handoff", err)
	}
	if len(msg.msgs) != 1 || !strings.Contains(msg.msgs[0], "review feedback") {
		t.Fatalf("uncertain CI must not re-send or block the sibling review reaction: %v", msg.msgs)
	}
	if len(sink.intents) != 1 || !strings.Contains(sink.intents[0].BodyOverride, "will not resend automatically") {
		t.Fatalf("uncertain delivery handoff = %+v, want one explicit no-auto-recovery alert", sink.intents)
	}
	sink.err = nil
	if err := New(st, msg, WithNotificationSink(sink)).ApplyPRObservation(ctx, "mer-1", o); err != nil {
		t.Fatalf("replayed uncertain handoff did not recover: %v", err)
	}
	if err := New(st, msg, WithNotificationSink(sink)).ApplyPRObservation(ctx, "mer-1", o); err != nil {
		t.Fatalf("latched uncertain handoff failed replay: %v", err)
	}
	if len(msg.msgs) != 1 || len(sink.intents) != 2 {
		t.Fatalf("uncertain replay duplicated pane delivery or successful alert: messages=%v intents=%+v", msg.msgs, sink.intents)
	}
}

func TestPRObservation_CrashAfterWriteBeforeCommitNeverResends(t *testing.T) {
	st := newReservationFakeStore()
	st.sessions["mer-1"] = working("mer-1")
	st.commitErr = errors.New("simulated crash before commit")
	sink := &fakeNotificationSink{}
	prURL := "https://github.com/o/r/pull/28"
	o := ports.PRObservation{
		Fetched: true, URL: prURL, HeadSHA: "head-a", CI: domain.CIFailing,
		Checks: []ports.PRCheckObservation{{Name: "build", CommitHash: "head-a", Status: domain.PRCheckFailed, LogTail: "boom"}},
	}
	msg := &fakeMessenger{}
	if err := New(st, msg, WithNotificationSink(sink)).ApplyPRObservation(ctx, "mer-1", o); err != nil {
		t.Fatalf("commit uncertainty must be accounted after alert: %v", err)
	}
	if len(msg.msgs) != 1 {
		t.Fatalf("pre-crash writes = %v, want one", msg.msgs)
	}
	st.commitErr = nil
	err := New(st, msg, WithNotificationSink(sink)).ApplyPRObservation(ctx, "mer-1", o)
	if err != nil {
		t.Fatalf("restart uncertainty must not fail the control plane: %v", err)
	}
	if len(msg.msgs) != 1 {
		t.Fatalf("post-write crash re-sent: %v", msg.msgs)
	}
	if len(sink.intents) != 1 {
		t.Fatalf("commit uncertainty alerts = %+v, want one durable handoff", sink.intents)
	}
}

func TestPRObservation_EditedReviewCommentRenudgesAcrossRestart(t *testing.T) {
	st := newFakeStore()
	st.sessions["mer-1"] = working("mer-1")
	o := ports.PRObservation{
		Fetched: true,
		URL:     "https://github.com/o/r/pull/49",
		HeadSHA: "head-49",
		Review:  domain.ReviewChangesRequest,
		Comments: []ports.PRCommentObservation{{
			ID: "c1", ThreadID: "t1", Author: "alice", File: "main.go", Line: 49, Body: "first request",
		}},
	}

	first := &fakeMessenger{}
	if err := New(st, first).ApplyPRObservation(ctx, "mer-1", o); err != nil {
		t.Fatalf("first ApplyPRObservation: %v", err)
	}
	if len(first.msgs) != 1 {
		t.Fatalf("first observation: want 1 nudge, got %d", len(first.msgs))
	}

	// A new manager proves the signature is loaded from durable PR state, not
	// retained only in memory. Identical content remains suppressed.
	second := &fakeMessenger{}
	m2 := New(st, second)
	if err := m2.ApplyPRObservation(ctx, "mer-1", o); err != nil {
		t.Fatalf("identical post-restart ApplyPRObservation: %v", err)
	}
	if len(second.msgs) != 0 {
		t.Fatalf("identical post-restart feedback re-nudged: %v", second.msgs)
	}

	// GitHub keeps the comment/thread IDs when a reviewer edits a comment.
	// The actionable content changed while the unresolved count stayed one, so
	// the worker must receive the revised request.
	edited := o
	edited.Comments = append([]ports.PRCommentObservation(nil), o.Comments...)
	edited.Comments[0].Body = "revised request with a new constraint"
	if err := m2.ApplyPRObservation(ctx, "mer-1", edited); err != nil {
		t.Fatalf("edited ApplyPRObservation: %v", err)
	}
	if len(second.msgs) != 1 || !strings.Contains(second.msgs[0], "new constraint") {
		t.Fatalf("edited same-count feedback did not re-nudge with new content: %v", second.msgs)
	}
}

func TestPRObservation_NewReviewRoundResetsExhaustedBudgetForIdleSession(t *testing.T) {
	st := newFakeStore()
	st.sessions["mer-1"] = working("mer-1")
	prURL := "https://github.com/o/r/pull/39"

	first := &fakeMessenger{}
	m1 := New(st, first)
	for round := 1; round <= reviewMaxNudge; round++ {
		o := ports.PRObservation{
			Fetched: true,
			URL:     prURL,
			HeadSHA: fmt.Sprintf("head-%d", round),
			Review:  domain.ReviewChangesRequest,
			Comments: []ports.PRCommentObservation{{
				ID: fmt.Sprintf("comment-%d", round), Body: fmt.Sprintf("round %d feedback", round),
			}},
		}
		if err := m1.ApplyPRObservation(ctx, "mer-1", o); err != nil {
			t.Fatalf("round %d ApplyPRObservation: %v", round, err)
		}
	}
	if len(first.msgs) != reviewMaxNudge {
		t.Fatalf("initial review nudges = %d, want %d", len(first.msgs), reviewMaxNudge)
	}

	// Simulate the stalled production state: the prior review budget is durable,
	// the daemon restarts, and the worker remains idle while a new round arrives.
	rec := st.sessions["mer-1"]
	rec.Activity = domain.Activity{State: domain.ActivityIdle, LastActivityAt: time.Now().Add(-2 * time.Minute)}
	st.sessions["mer-1"] = rec
	second := &fakeMessenger{}
	m2 := New(st, second)
	roundTwo := ports.PRObservation{
		Fetched: true,
		URL:     prURL,
		HeadSHA: "head-new",
		Review:  domain.ReviewChangesRequest,
		Comments: []ports.PRCommentObservation{{
			ID: "comment-new", Body: "new review round feedback",
		}},
	}
	if err := m2.ApplyPRObservation(ctx, "mer-1", roundTwo); err != nil {
		t.Fatalf("new idle review round: %v", err)
	}
	if len(second.msgs) != 1 || !strings.Contains(second.msgs[0], "new review round feedback") {
		t.Fatalf("new review round did not reach idle worker: %v", second.msgs)
	}

	if err := m2.ApplyPRObservation(ctx, "mer-1", roundTwo); err != nil {
		t.Fatalf("repeat new review round: %v", err)
	}
	if len(second.msgs) != 1 {
		t.Fatalf("identical review fingerprint re-nudged: %v", second.msgs)
	}
}

func TestPRObservation_NewCIFailureReachesIdleSessionAndDedups(t *testing.T) {
	st := newFakeStore()
	rec := working("mer-1")
	rec.Activity = domain.Activity{State: domain.ActivityIdle, LastActivityAt: time.Now().Add(-2 * time.Minute)}
	st.sessions[rec.ID] = rec
	msg := &fakeMessenger{}
	m := New(st, msg)
	prURL := "https://github.com/o/r/pull/39"

	observation := func(commit, tail string) ports.PRObservation {
		return ports.PRObservation{
			Fetched: true,
			URL:     prURL,
			HeadSHA: commit,
			CI:      domain.CIFailing,
			Checks: []ports.PRCheckObservation{{
				Name: "build", CommitHash: commit, Status: domain.PRCheckFailed, LogTail: tail,
			}},
		}
	}
	first := observation("commit-1", "first failure")
	if err := m.ApplyPRObservation(ctx, rec.ID, first); err != nil {
		t.Fatalf("first CI failure: %v", err)
	}
	if err := m.ApplyPRObservation(ctx, rec.ID, first); err != nil {
		t.Fatalf("repeat CI failure: %v", err)
	}
	if len(msg.msgs) != 1 {
		t.Fatalf("identical CI fingerprint nudges = %d, want 1", len(msg.msgs))
	}

	second := observation("commit-2", "new failure")
	if err := m.ApplyPRObservation(ctx, rec.ID, second); err != nil {
		t.Fatalf("new CI failure: %v", err)
	}
	if len(msg.msgs) != 2 || !strings.Contains(msg.msgs[1], "new failure") {
		t.Fatalf("new CI failure did not reach idle worker: %v", msg.msgs)
	}
}

func TestPRObservation_DedupPersistsAcrossPRs(t *testing.T) {
	st := newFakeStore()
	st.sessions["mer-1"] = working("mer-1")
	msg := &fakeMessenger{}
	m := New(st, msg)

	for _, url := range []string{"https://github.com/o/r/pull/1", "https://github.com/o/r/pull/2"} {
		o := ports.PRObservation{
			Fetched: true, URL: url, HeadSHA: "c1", CI: domain.CIFailing,
			Checks: []ports.PRCheckObservation{{Name: "build", CommitHash: "c1", Status: domain.PRCheckFailed, LogTail: "boom"}},
		}
		if err := m.ApplyPRObservation(ctx, "mer-1", o); err != nil {
			t.Fatalf("ApplyPRObservation for %s: %v", url, err)
		}
	}
	if len(msg.msgs) != 2 {
		t.Fatalf("distinct PRs should each get one nudge, got %d", len(msg.msgs))
	}
	if _, ok := st.signatures["https://github.com/o/r/pull/1"]; !ok {
		t.Fatal("missing persisted signature for PR 1")
	}
	if _, ok := st.signatures["https://github.com/o/r/pull/2"]; !ok {
		t.Fatal("missing persisted signature for PR 2")
	}
}

func TestApplyReviewResultSendsAndDedupsThroughPRSignature(t *testing.T) {
	st := newFakeStore()
	st.sessions["mer-1"] = working("mer-1")
	msg := &fakeMessenger{}
	m := New(st, msg)
	result := ReviewResult{
		RunID:          "run-1",
		WorkerID:       "mer-1",
		PRURL:          "https://github.com/o/r/pull/1",
		TargetSHA:      "sha1",
		Verdict:        domain.VerdictChangesRequested,
		Body:           "fix the bug",
		GithubReviewID: "98\x1b[2J765",
	}

	outcome, err := m.ApplyReviewResult(ctx, "mer-1", result)
	if err != nil {
		t.Fatalf("ApplyReviewResult: %v", err)
	}
	if outcome != ReviewDeliverySent || len(msg.msgs) != 1 {
		t.Fatalf("outcome/messages = %q/%v, want sent once", outcome, msg.msgs)
	}
	got := msg.msgs[0]
	for _, want := range []string{"[AO reviewer]", "PR: " + result.PRURL, "Verdict: changes_requested", "Review body:\nfix the bug", "GitHub review: 98[2J765"} {
		if !strings.Contains(got, want) {
			t.Fatalf("AO review nudge missing %q: %q", want, got)
		}
	}
	if strings.Contains(got, "\x1b") {
		t.Fatalf("AO review nudge should sanitize control bytes: %q", got)
	}
	if st.signatures[result.PRURL] == "" {
		t.Fatal("AO review nudge did not persist sendOnce signature")
	}

	outcome, err = m.ApplyReviewResult(ctx, "mer-1", result)
	if err != nil {
		t.Fatalf("repeat ApplyReviewResult: %v", err)
	}
	if outcome != ReviewDeliverySent || len(msg.msgs) != 1 {
		t.Fatalf("repeat should report delivered outcome and suppress duplicate send, outcome=%q msgs=%v", outcome, msg.msgs)
	}

	result.RunID = "run-2"
	result.TargetSHA = "sha2"
	outcome, err = m.ApplyReviewResult(ctx, "mer-1", result)
	if err != nil {
		t.Fatalf("new pass ApplyReviewResult: %v", err)
	}
	if outcome != ReviewDeliverySent || len(msg.msgs) != 2 {
		t.Fatalf("new review pass should send again, outcome=%q msgs=%v", outcome, msg.msgs)
	}
}

func TestApplyReviewResultContractReadFailureIsRetryable(t *testing.T) {
	st := newFakeStore()
	st.sessions["mer-1"] = working("mer-1")
	prURL := "https://github.com/o/r/pull/1"
	st.designContractErrors[prURL] = errors.New("database busy")
	msg := &fakeMessenger{}
	m := New(st, msg)
	result := ReviewResult{RunID: "run-1", WorkerID: "mer-1", PRURL: prURL, TargetSHA: "sha1", Verdict: domain.VerdictChangesRequested, Body: "fix"}
	if outcome, err := m.ApplyReviewResult(ctx, "mer-1", result); err == nil || outcome != ReviewDeliveryNoop {
		t.Fatalf("contract read failure = %q, %v; want retryable error", outcome, err)
	}
	if len(msg.msgs) != 0 || st.signatures[prURL] != "" {
		t.Fatalf("failed contract read delivered or deduped review: msgs=%v signature=%q", msg.msgs, st.signatures[prURL])
	}
	delete(st.designContractErrors, prURL)
	if outcome, err := m.ApplyReviewResult(ctx, "mer-1", result); err != nil || outcome != ReviewDeliverySent {
		t.Fatalf("retry after contract recovery = %q, %v", outcome, err)
	}
	if len(msg.msgs) != 1 || st.signatures[prURL] == "" {
		t.Fatalf("recovered review not delivered exactly once: msgs=%v signature=%q", msg.msgs, st.signatures[prURL])
	}
}

func TestApplyReviewResultThirdClassOccurrenceUsesSimplificationRound(t *testing.T) {
	st := newFakeStore()
	st.sessions["mer-1"] = working("mer-1")
	msg := &fakeMessenger{}
	telemetry := &telemetrySink{}
	m := New(st, msg, WithTelemetry(telemetry))
	result := ReviewResult{
		RunID: "run-3", WorkerID: "mer-1", PRURL: "https://github.com/o/r/pull/1", TargetSHA: "sha3",
		Verdict:             domain.VerdictChangesRequested,
		Findings:            []domain.ReviewFinding{{ClassTag: "missing-notify", File: "notify.go", Body: "broken path skips notify"}},
		Ledger:              domain.FindingLedgerSummary{TotalFindings: 3, Rounds: 3, Classes: []domain.FindingClassCount{{ClassTag: "missing-notify", Count: 3}}},
		SimplificationClass: "missing-notify",
	}
	outcome, err := m.ApplyReviewResult(ctx, "mer-1", result)
	if err != nil || outcome != ReviewDeliverySent {
		t.Fatalf("ApplyReviewResult = %q, %v", outcome, err)
	}
	if len(msg.msgs) != 1 {
		t.Fatalf("messages = %v", msg.msgs)
	}
	for _, want := range []string{"Finding-class ledger: 3 findings over 3 rounds", "KIND: SIMPLIFICATION ROUND", "enforce it at ONE chokepoint", "enumerate every sibling code path"} {
		if !strings.Contains(msg.msgs[0], want) {
			t.Fatalf("simplification dispatch missing %q: %s", want, msg.msgs[0])
		}
	}
	if len(telemetry.events) != 1 || telemetry.events[0].Name != "review_simplification_round" {
		t.Fatalf("telemetry = %+v", telemetry.events)
	}
}

func TestApplyReviewResultReplaysDurableSimplificationEventAfterSinkDropAndRestart(t *testing.T) {
	st := newFakeStore()
	st.sessions["mer-1"] = working("mer-1")
	msg := &fakeMessenger{}
	result := ReviewResult{
		RunID: "run-3", WorkerID: "mer-1", PRURL: "https://github.com/o/r/pull/1", TargetSHA: "sha3",
		Verdict: domain.VerdictChangesRequested, SimplificationClass: "missing-notify",
	}

	first := New(st, msg, WithTelemetry(droppingTelemetrySink{}))
	firstAt := time.Unix(100, 0).UTC()
	first.clock = func() time.Time { return firstAt }
	if outcome, err := first.ApplyReviewResult(ctx, "mer-1", result); err != nil || outcome != ReviewDeliverySent {
		t.Fatalf("first ApplyReviewResult = %q, %v", outcome, err)
	}
	if len(st.telemetryEvents) != 1 || len(msg.msgs) != 1 {
		t.Fatalf("durable events/messages = %d/%d, want 1/1", len(st.telemetryEvents), len(msg.msgs))
	}

	// The first sink dropped the fanout after the atomic local write. A fresh
	// manager replays the still-undelivered run using the same stable event ID.
	drained := &telemetrySink{}
	restarted := New(st, msg, WithTelemetry(drained))
	restarted.clock = func() time.Time { return firstAt.Add(time.Hour) }
	if outcome, err := restarted.ApplyReviewResult(ctx, "mer-1", result); err != nil || outcome != ReviewDeliverySent {
		t.Fatalf("restart ApplyReviewResult = %q, %v", outcome, err)
	}
	if len(drained.events) != 1 || len(msg.msgs) != 1 {
		t.Fatalf("restart events/messages = %d/%d, want 1/1", len(drained.events), len(msg.msgs))
	}
	if drained.events[0].ID == "" || len(st.telemetryEvents) != 1 {
		t.Fatalf("stable event/local rows = %q/%d", drained.events[0].ID, len(st.telemetryEvents))
	}
	if !drained.events[0].OccurredAt.Equal(firstAt) {
		t.Fatalf("replayed occurredAt = %s, want persisted %s", drained.events[0].OccurredAt, firstAt)
	}
	if _, err := restarted.ApplyReviewResult(ctx, "mer-1", result); err != nil {
		t.Fatalf("repeat ApplyReviewResult: %v", err)
	}
	if len(drained.events) != 1 || len(st.telemetryEvents) != 1 {
		t.Fatalf("repeat events/local rows = %d/%d, want 1/1", len(drained.events), len(st.telemetryEvents))
	}
}

func TestApplyReviewResultIncludesCanonicalPRDesignContract(t *testing.T) {
	st := newFakeStore()
	rec := working("mer-1")
	st.sessions[rec.ID] = rec
	st.designContracts["https://github.com/o/r/pull/1"] = "# Design Contract\n\n## Invariants\n- Every idle backlog poll notifies or exits with a reason.\x1b[31m\x00\u0085\n"
	msg := &fakeMessenger{}
	m := New(st, msg)
	result := ReviewResult{RunID: "run-1", WorkerID: rec.ID, PRURL: "https://github.com/o/r/pull/1", TargetSHA: "sha1", Verdict: domain.VerdictChangesRequested, Body: "fix the missed path"}

	outcome, err := m.ApplyReviewResult(ctx, rec.ID, result)
	if err != nil || outcome != ReviewDeliverySent {
		t.Fatalf("ApplyReviewResult = %q, %v", outcome, err)
	}
	got := msg.msgs[0]
	for _, want := range []string{"canonical AO state", "Every idle backlog poll notifies or exits with a reason", "AO-Review-Fix-Invariant", `"mode":"preserve"`, "Never edit the workspace projection", "atomically appends", "ao verify <configured-profile>", "Before pushing the review-fix commit", "read the printed log"} {
		if !strings.Contains(got, want) {
			t.Fatalf("review dispatch missing %q:\n%s", want, got)
		}
	}
	for _, forbidden := range []string{"\x1b", "\x00", "\u0085"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("review dispatch retained control %q: %q", forbidden, got)
		}
	}
}

func TestPRObservationReviewDispatchIncludesMatchingCanonicalContract(t *testing.T) {
	st := newFakeStore()
	rec := working("mer-1")
	st.sessions[rec.ID] = rec
	st.designContracts["https://github.com/o/r/pull/1"] = "# Design Contract\n\n## Invariants\n- Resolve feedback by enforcing one ownership boundary.\n"
	msg := &fakeMessenger{}
	m := New(st, msg)
	o := ports.PRObservation{
		Fetched: true, URL: "https://github.com/o/r/pull/1", HeadSHA: "sha1", Review: domain.ReviewChangesRequest,
		Comments: []ports.PRCommentObservation{{ID: "c1", Body: "fix ownership", Author: "reviewer"}},
	}

	if err := m.ApplyPRObservation(ctx, rec.ID, o); err != nil {
		t.Fatal(err)
	}
	if len(msg.msgs) != 1 || !strings.Contains(msg.msgs[0], "Resolve feedback by enforcing one ownership boundary") || !strings.Contains(msg.msgs[0], "AO-Review-Fix-Invariant") {
		t.Fatalf("review observation dispatch missing design contract policy: %v", msg.msgs)
	}
}

func TestDesignContractProjectionSelectsExactPRAcrossDispatchAndRestart(t *testing.T) {
	workspace := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", workspace).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	prA, prB := "https://github.com/o/r/pull/1", "https://github.com/o/r/pull/2"
	st := newFakeStore()
	st.designContracts[prA] = "# Design Contract\n\n## Invariants\n- invariant-A\n"
	st.designContracts[prB] = "# Design Contract\n\n## Invariants\n- invariant-B\n"
	m := New(st, &fakeMessenger{})
	for _, item := range []struct{ pr, invariant string }{{prA, "invariant-A"}, {prB, "invariant-B"}} {
		var msg strings.Builder
		m.writeDesignContractDispatch(ctx, &msg, workspace, item.pr)
		if !strings.Contains(msg.String(), item.invariant) {
			t.Fatalf("dispatch for %s = %s", item.pr, msg.String())
		}
	}
	current, err := os.ReadFile(filepath.Join(workspace, ".ao", "CONTRACT.md"))
	if err != nil || !strings.Contains(string(current), "Scope: Pull request: "+prB) || strings.Contains(string(current), "invariant-A") {
		t.Fatalf("current projection after PR B = %q, %v", current, err)
	}
	entries, err := os.ReadDir(filepath.Join(workspace, ".ao", "contracts"))
	if err != nil || len(entries) != 2 {
		t.Fatalf("per-PR projections = %d, %v", len(entries), err)
	}

	// A review submission has atomically added a new explicit invariant before
	// lifecycle delivery. A restarted manager refreshes the exact PR projection
	// immediately, without overwriting the sibling's per-PR file.
	st.designContracts[prA] += "\n## Review-discovered invariants\n\n- invariant-A-new\n"
	restarted := New(st, &fakeMessenger{})
	var msg strings.Builder
	restarted.writeDesignContractDispatch(ctx, &msg, workspace, prA)
	current, err = os.ReadFile(filepath.Join(workspace, ".ao", "CONTRACT.md"))
	if err != nil || !strings.Contains(string(current), "Scope: Pull request: "+prA) || !strings.Contains(string(current), "invariant-A-new") || strings.Contains(string(current), "invariant-B") {
		t.Fatalf("current projection after restart/PR A = %q, %v", current, err)
	}
	entries, err = os.ReadDir(filepath.Join(workspace, ".ao", "contracts"))
	if err != nil || len(entries) != 2 {
		t.Fatalf("per-PR projections after refresh = %d, %v", len(entries), err)
	}
}

func TestActionableReviewBodyKeepsPartialDeflection(t *testing.T) {
	result := ReviewResult{Findings: []domain.ReviewFinding{
		{ClassTag: "partial", Body: "still resolve the bound thread", OutOfScope: true, DeferredIssueURL: "issue", ThreadID: "thread"},
		{ClassTag: "done", Body: "already deferred", OutOfScope: true, DeferredIssueURL: "issue", ThreadID: "thread", ThreadResolved: true},
	}}
	body := actionableReviewBody(result)
	if !strings.Contains(body, "still resolve the bound thread") {
		t.Fatalf("partial deflection omitted from dispatch: %q", body)
	}
	if strings.Contains(body, "already deferred") {
		t.Fatalf("fully deflected finding remained actionable: %q", body)
	}
}

func TestApplyReviewResultSuppressedByJITGuardIsNotDelivered(t *testing.T) {
	// The worker is working at ApplyReviewResult's entry guard (read #1) but a
	// permission dialog stores blocked before sendOnce's just-in-time re-read
	// (read #2). The nudge must be SUPPRESSED, and the outcome must be
	// ReviewDeliveryNoop — NOT Sent — so the caller does not stamp the run
	// delivered and the changes-requested feedback re-fires once unblocked.
	st := newFakeStore()
	st.sessions["mer-1"] = working("mer-1")
	bst := &blockOnNthGetStore{fakeStore: st, id: "mer-1", flipAt: 2}
	msg := &fakeMessenger{}
	m := New(bst, msg)
	result := ReviewResult{
		RunID: "run-1", WorkerID: "mer-1", PRURL: "https://github.com/o/r/pull/1",
		TargetSHA: "sha1", Verdict: domain.VerdictChangesRequested, Body: "fix the bug",
	}

	outcome, err := m.ApplyReviewResult(ctx, "mer-1", result)
	if err != nil {
		t.Fatalf("ApplyReviewResult: %v", err)
	}
	if outcome != ReviewDeliveryNoop {
		t.Fatalf("outcome = %q, want no_op (suppressed nudge must not be stamped delivered)", outcome)
	}
	if len(msg.msgs) != 0 {
		t.Fatalf("nudge pasted into a session that went blocked before send: %v", msg.msgs)
	}
	if st.signatures[result.PRURL] != "" {
		t.Fatal("suppressed nudge must not persist a sendOnce signature (it re-fires next observation)")
	}
}

func TestApplyReviewBatchSendsCombinedAndDedups(t *testing.T) {
	st := newFakeStore()
	st.sessions["mer-1"] = working("mer-1")
	msg := &fakeMessenger{}
	m := New(st, msg)
	results := []ReviewResult{
		{RunID: "run-2", BatchID: "batch-1", WorkerID: "mer-1", PRURL: "https://github.com/o/r/pull/2", TargetSHA: "sha2", Verdict: domain.VerdictChangesRequested, Body: "fix tests", GithubReviewID: "102"},
		{RunID: "run-1", BatchID: "batch-1", WorkerID: "mer-1", PRURL: "https://github.com/o/r/pull/1", TargetSHA: "sha1", Verdict: domain.VerdictChangesRequested, Body: "fix auth", GithubReviewID: "101"},
	}

	outcome, err := m.ApplyReviewBatch(ctx, "mer-1", "batch-1", results)
	if err != nil {
		t.Fatalf("ApplyReviewBatch: %v", err)
	}
	if outcome != ReviewDeliverySent || len(msg.msgs) != 1 {
		t.Fatalf("outcome/messages = %q/%v, want sent once", outcome, msg.msgs)
	}
	got := msg.msgs[0]
	for _, want := range []string{
		"submitted 2 review(s) requesting changes",
		"PR: https://github.com/o/r/pull/1",
		"GitHub review: 101",
		"Review body:\nfix auth",
		"PR: https://github.com/o/r/pull/2",
		"GitHub review: 102",
		"Review body:\nfix tests",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("batch nudge missing %q: %q", want, got)
		}
	}
	if st.signatures["https://github.com/o/r/pull/1"] == "" {
		t.Fatal("batch nudge did not persist signature on anchor PR")
	}

	outcome, err = m.ApplyReviewBatch(ctx, "mer-1", "batch-1", results)
	if err != nil {
		t.Fatalf("repeat ApplyReviewBatch: %v", err)
	}
	if outcome != ReviewDeliverySent || len(msg.msgs) != 1 {
		t.Fatalf("repeat should suppress duplicate send, outcome=%q msgs=%v", outcome, msg.msgs)
	}
}

func TestApplyReviewBatchBusyReservationIsNotStampedDeliveredAndRetries(t *testing.T) {
	st := newReservationFakeStore()
	st.sessions["mer-1"] = working("mer-1")
	msg := &fakeMessenger{}
	result := ReviewResult{
		RunID: "run-1", BatchID: "batch-1", WorkerID: "mer-1",
		PRURL: "https://github.com/o/r/pull/1", TargetSHA: "sha1",
		Verdict: domain.VerdictChangesRequested, Body: "fix auth", GithubReviewID: "101",
	}
	key := "review-batch:" + result.PRURL + ":batch-1"
	sig := strings.Join([]string{result.RunID, result.PRURL, result.TargetSHA, result.GithubReviewID, result.Body}, "\x00")
	now := time.Now().UTC()
	fences := []ports.PRReactionFence{{PRURL: result.PRURL, SessionID: "mer-1", HeadSHA: result.TargetSHA}}
	reserved, err := st.ReservePRReaction(ctx, result.PRURL, key, sig, reviewMaxNudge, "owner-a", fences, now, now.Add(time.Minute))
	if err != nil || reserved.Status != ports.PRReactionReserved {
		t.Fatalf("owner reserve = %+v, err=%v", reserved, err)
	}

	outcome, err := New(st, msg).ApplyReviewBatch(ctx, "mer-1", "batch-1", []ReviewResult{result})
	if err != nil {
		t.Fatalf("busy ApplyReviewBatch: %v", err)
	}
	if outcome != ReviewDeliveryNoop || len(msg.msgs) != 0 {
		t.Fatalf("busy outcome/messages = %q/%v, want no delivery proof", outcome, msg.msgs)
	}
	// Model owner A's guard suppression: no pane write occurred, so releasing
	// the pre-boundary reservation must let a current owner retry.
	if released, err := st.ReleasePRReaction(ctx, result.PRURL, key, "owner-a"); err != nil || !released {
		t.Fatalf("owner suppression release = %v, err=%v", released, err)
	}
	outcome, err = New(st, msg).ApplyReviewBatch(ctx, "mer-1", "batch-1", []ReviewResult{result})
	if err != nil {
		t.Fatalf("retry ApplyReviewBatch: %v", err)
	}
	if outcome != ReviewDeliverySent || len(msg.msgs) != 1 {
		t.Fatalf("retry outcome/messages = %q/%v, want sent once", outcome, msg.msgs)
	}
}

func TestApplyReviewBatchFencesEveryPRBeforePaneWrite(t *testing.T) {
	st := newReservationFakeStore()
	st.sessions["mer-1"] = working("mer-1")
	st.heads["https://github.com/o/r/pull/1"] = "sha1"
	st.heads["https://github.com/o/r/pull/2"] = "sha2"
	st.owners["https://github.com/o/r/pull/1"] = "mer-1"
	st.owners["https://github.com/o/r/pull/2"] = "mer-1"
	msg := &fakeMessenger{}
	results := []ReviewResult{
		{RunID: "run-1", BatchID: "batch-1", WorkerID: "mer-1", PRURL: "https://github.com/o/r/pull/1", TargetSHA: "sha1", Verdict: domain.VerdictChangesRequested, Body: "fix auth"},
		{RunID: "run-2", BatchID: "batch-1", WorkerID: "mer-1", PRURL: "https://github.com/o/r/pull/2", TargetSHA: "sha2", Verdict: domain.VerdictChangesRequested, Body: "fix tests"},
	}
	st.beforeStart = func() {
		// The non-anchor PR advances after selection/reservation but before the
		// durable pane boundary. Validating only pull/1 would send stale feedback.
		st.heads["https://github.com/o/r/pull/2"] = "sha3"
		st.beforeStart = nil
	}
	outcome, err := New(st, msg).ApplyReviewBatch(ctx, "mer-1", "batch-1", results)
	if err != nil {
		t.Fatalf("stale batch: %v", err)
	}
	if outcome != ReviewDeliveryNoop || len(msg.msgs) != 0 {
		t.Fatalf("stale secondary PR outcome/messages = %q/%v", outcome, msg.msgs)
	}
	st.heads["https://github.com/o/r/pull/2"] = "sha2"
	outcome, err = New(st, msg).ApplyReviewBatch(ctx, "mer-1", "batch-1", results)
	if err != nil {
		t.Fatalf("current batch retry: %v", err)
	}
	if outcome != ReviewDeliverySent || len(msg.msgs) != 1 {
		t.Fatalf("current retry outcome/messages = %q/%v", outcome, msg.msgs)
	}
}

func TestApplyReviewBatchNoopsWithoutDeliverableResults(t *testing.T) {
	st := newFakeStore()
	st.sessions["mer-1"] = working("mer-1")
	msg := &fakeMessenger{}
	m := New(st, msg)

	outcome, err := m.ApplyReviewBatch(ctx, "mer-1", "batch-1", nil)
	if err != nil {
		t.Fatalf("ApplyReviewBatch: %v", err)
	}
	if outcome != ReviewDeliveryNoop || len(msg.msgs) != 0 || st.signatureWrites != 0 {
		t.Fatalf("empty batch should no-op, outcome=%q msgs=%v signatureWrites=%d", outcome, msg.msgs, st.signatureWrites)
	}
}

func TestApplyReviewResultNoopsWhenIrrelevant(t *testing.T) {
	deliveredAt := time.Unix(100, 0).UTC()
	tests := []struct {
		name   string
		result ReviewResult
		rec    domain.SessionRecord
	}{
		{
			name:   "approved",
			result: ReviewResult{RunID: "run-1", PRURL: "pr1", Verdict: domain.VerdictApproved},
			rec:    working("mer-1"),
		},
		{
			name:   "already delivered",
			result: ReviewResult{RunID: "run-1", PRURL: "pr1", Verdict: domain.VerdictChangesRequested, DeliveredAt: &deliveredAt},
			rec:    working("mer-1"),
		},
		{
			name:   "terminated worker",
			result: ReviewResult{RunID: "run-1", PRURL: "pr1", Verdict: domain.VerdictChangesRequested},
			rec:    func() domain.SessionRecord { r := working("mer-1"); r.IsTerminated = true; return r }(),
		},
		{
			name:   "worker waiting input",
			result: ReviewResult{RunID: "run-1", PRURL: "pr1", Verdict: domain.VerdictChangesRequested},
			rec:    domain.SessionRecord{ID: "mer-1", Activity: domain.Activity{State: domain.ActivityWaitingInput}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, st, msg := newManager()
			st.sessions["mer-1"] = tt.rec
			outcome, err := m.ApplyReviewResult(ctx, "mer-1", tt.result)
			if err != nil {
				t.Fatalf("ApplyReviewResult: %v", err)
			}
			if outcome != ReviewDeliveryNoop || len(msg.msgs) != 0 || st.signatureWrites != 0 {
				t.Fatalf("irrelevant result should no-op, outcome=%q msgs=%v signatureWrites=%d", outcome, msg.msgs, st.signatureWrites)
			}
		})
	}
}

func TestApplyTrackerFacts_TerminalStateMarksTerminated(t *testing.T) {
	for _, state := range []domain.NormalizedIssueState{domain.IssueDone, domain.IssueCancelled} {
		t.Run(string(state), func(t *testing.T) {
			m, st, msg := newManager()
			st.sessions["mer-1"] = working("mer-1")
			o := ports.TrackerObservation{
				Fetched: true,
				Issue:   ports.TrackerIssueObservation{URL: "https://github.com/o/r/issues/1", State: state},
			}
			if err := m.ApplyTrackerFacts(ctx, "mer-1", o); err != nil {
				t.Fatalf("ApplyTrackerFacts: %v", err)
			}
			got := st.sessions["mer-1"]
			if !got.IsTerminated || got.Activity.State != domain.ActivityExited {
				t.Fatalf("want terminated/exited for state %q, got %+v", state, got)
			}
			if len(msg.msgs) != 0 {
				t.Fatalf("terminal state should not nudge, got %v", msg.msgs)
			}
		})
	}
}

func TestApplyTrackerFacts_AssigneeChangedIsLogOnly(t *testing.T) {
	m, st, msg := newManager()
	st.sessions["mer-1"] = working("mer-1")
	before := st.sessions["mer-1"]
	o := ports.TrackerObservation{
		Fetched: true,
		Issue:   ports.TrackerIssueObservation{URL: "https://github.com/o/r/issues/1", State: domain.IssueOpen, Assignee: "someone-else"},
		Changed: ports.TrackerChanged{Assignee: true},
	}
	if err := m.ApplyTrackerFacts(ctx, "mer-1", o); err != nil {
		t.Fatalf("ApplyTrackerFacts: %v", err)
	}
	if st.sessions["mer-1"] != before {
		t.Fatalf("assignee-only change must not mutate the session row, got %+v", st.sessions["mer-1"])
	}
	if len(msg.msgs) != 0 {
		t.Fatalf("assignee-only change must not nudge, got %v", msg.msgs)
	}
}

func TestApplyTrackerFacts_NewBotCommentNudges(t *testing.T) {
	m, st, msg := newManager()
	st.sessions["mer-1"] = working("mer-1")
	o := ports.TrackerObservation{
		Fetched: true,
		Issue:   ports.TrackerIssueObservation{URL: "https://github.com/o/r/issues/1", State: domain.IssueOpen},
		Comments: []ports.TrackerCommentObservation{
			{ID: "human-1", Author: "alice", Body: "human chime-in, must NOT nudge", IsBot: false},
			{ID: "bot-1", Author: "ci-bot[bot]", Body: "please rerun the migration", IsBot: true},
		},
		Changed: ports.TrackerChanged{Comments: true},
	}
	if err := m.ApplyTrackerFacts(ctx, "mer-1", o); err != nil {
		t.Fatalf("ApplyTrackerFacts: %v", err)
	}
	if len(msg.msgs) != 1 {
		t.Fatalf("want one bot-mention nudge, got %d: %v", len(msg.msgs), msg.msgs)
	}
	if !strings.Contains(msg.msgs[0], "please rerun the migration") {
		t.Fatalf("nudge should include the bot comment body, got %q", msg.msgs[0])
	}
	if strings.Contains(msg.msgs[0], "human chime-in") {
		t.Fatalf("nudge must not include human comments, got %q", msg.msgs[0])
	}
}

func TestApplyTrackerFacts_NudgeSuppressedOnRepeat(t *testing.T) {
	m, st, msg := newManager()
	st.sessions["mer-1"] = working("mer-1")
	o := ports.TrackerObservation{
		Fetched: true,
		Issue:   ports.TrackerIssueObservation{URL: "https://github.com/o/r/issues/1", State: domain.IssueOpen},
		Comments: []ports.TrackerCommentObservation{
			{ID: "bot-1", Author: "ci-bot[bot]", Body: "please rerun the migration", IsBot: true},
		},
		Changed: ports.TrackerChanged{Comments: true},
	}
	if err := m.ApplyTrackerFacts(ctx, "mer-1", o); err != nil {
		t.Fatalf("first ApplyTrackerFacts: %v", err)
	}
	if err := m.ApplyTrackerFacts(ctx, "mer-1", o); err != nil {
		t.Fatalf("second ApplyTrackerFacts: %v", err)
	}
	if len(msg.msgs) != 1 {
		t.Fatalf("repeat observation must dedup; got %d nudges: %v", len(msg.msgs), msg.msgs)
	}

	// A genuinely new bot comment still fires.
	o.Comments = append(o.Comments, ports.TrackerCommentObservation{ID: "bot-2", Author: "ci-bot[bot]", Body: "now check the seed", IsBot: true})
	if err := m.ApplyTrackerFacts(ctx, "mer-1", o); err != nil {
		t.Fatalf("third ApplyTrackerFacts: %v", err)
	}
	if len(msg.msgs) != 2 {
		t.Fatalf("new bot comment id should re-fire, got %d: %v", len(msg.msgs), msg.msgs)
	}
}

func TestApplyTrackerFacts_BotCommentWithEmptyIDIsIgnored(t *testing.T) {
	m, st, msg := newManager()
	st.sessions["mer-1"] = working("mer-1")
	// Bot comment lacks an ID — without one we cannot dedup, and the
	// zero-value signature collides with m.react.seen's empty default and
	// would silently suppress every future nudge for this issue. The
	// reducer must skip it entirely.
	o := ports.TrackerObservation{
		Fetched: true,
		Issue:   ports.TrackerIssueObservation{URL: "https://github.com/o/r/issues/1", State: domain.IssueOpen},
		Comments: []ports.TrackerCommentObservation{
			{ID: "", Author: "ci-bot[bot]", Body: "no id, must be skipped", IsBot: true},
		},
		Changed: ports.TrackerChanged{Comments: true},
	}
	if err := m.ApplyTrackerFacts(ctx, "mer-1", o); err != nil {
		t.Fatalf("ApplyTrackerFacts: %v", err)
	}
	if len(msg.msgs) != 0 {
		t.Fatalf("bot comment with empty ID must not nudge, got %v", msg.msgs)
	}
	// A subsequent, properly-formed bot comment must still nudge — the
	// earlier empty-ID entry must not have polluted the dedup signature.
	o.Comments = []ports.TrackerCommentObservation{
		{ID: "bot-1", Author: "ci-bot[bot]", Body: "now with an id", IsBot: true},
	}
	if err := m.ApplyTrackerFacts(ctx, "mer-1", o); err != nil {
		t.Fatalf("second ApplyTrackerFacts: %v", err)
	}
	if len(msg.msgs) != 1 {
		t.Fatalf("follow-up bot comment with real ID should nudge, got %d: %v", len(msg.msgs), msg.msgs)
	}
}

func TestApplyTrackerFacts_NotFetchedIsNoop(t *testing.T) {
	m, st, msg := newManager()
	st.sessions["mer-1"] = working("mer-1")
	before := st.sessions["mer-1"]
	if err := m.ApplyTrackerFacts(ctx, "mer-1", ports.TrackerObservation{Fetched: false}); err != nil {
		t.Fatalf("ApplyTrackerFacts: %v", err)
	}
	if st.sessions["mer-1"] != before {
		t.Fatalf("not-fetched observation must not mutate state")
	}
	if len(msg.msgs) != 0 {
		t.Fatalf("not-fetched observation must not nudge")
	}
}

func TestApplyTrackerFacts_TerminatedSessionDoesNotRefireOrNudge(t *testing.T) {
	m, st, msg := newManager()
	st.sessions["mer-1"] = domain.SessionRecord{ID: "mer-1", ProjectID: "mer", IsTerminated: true, Activity: domain.Activity{State: domain.ActivityExited}}
	o := ports.TrackerObservation{
		Fetched: true,
		Issue:   ports.TrackerIssueObservation{URL: "https://github.com/o/r/issues/1", State: domain.IssueOpen},
		Comments: []ports.TrackerCommentObservation{
			{ID: "bot-1", Body: "x", IsBot: true},
		},
		Changed: ports.TrackerChanged{Comments: true},
	}
	if err := m.ApplyTrackerFacts(ctx, "mer-1", o); err != nil {
		t.Fatalf("ApplyTrackerFacts: %v", err)
	}
	if len(msg.msgs) != 0 {
		t.Fatalf("terminated session must not receive nudges, got %v", msg.msgs)
	}
}

func TestPRObservation_RetriesAfterMessengerFailure(t *testing.T) {
	m, st, msg := newManager()
	st.sessions["mer-1"] = working("mer-1")
	o := ports.PRObservation{Fetched: true, URL: "pr1", HeadSHA: "head-1", TargetBranch: "main", Mergeability: domain.MergeConflicting}
	msg.err = errors.New("temporary send failure")
	if err := m.ApplyPRObservation(ctx, "mer-1", o); err == nil {
		t.Fatal("want send error")
	}
	msg.err = nil
	if err := m.ApplyPRObservation(ctx, "mer-1", o); err != nil {
		t.Fatal(err)
	}
	if len(msg.msgs) != 1 {
		t.Fatalf("want retry to send once, got %v", msg.msgs)
	}
}

func TestActivity_FirstSignalStampsReceipt(t *testing.T) {
	m, st, _ := newManager()
	st.sessions["mer-1"] = domain.SessionRecord{ID: "mer-1", ProjectID: "mer", Activity: domain.Activity{State: domain.ActivityIdle, LastActivityAt: time.Now()}}
	// A same-state repeat (idle on an idle-seeded row) must still write: the
	// receipt itself is the durable fact that clears no_signal.
	if err := m.ApplyActivitySignal(ctx, "mer-1", ports.ActivitySignal{Valid: true, State: domain.ActivityIdle}); err != nil {
		t.Fatal(err)
	}
	got := st.sessions["mer-1"]
	if got.FirstSignalAt.IsZero() {
		t.Fatalf("first signal not stamped: %+v", got)
	}
	stamped := got.FirstSignalAt
	// Later signals must not move the receipt.
	if err := m.ApplyActivitySignal(ctx, "mer-1", ports.ActivitySignal{Valid: true, State: domain.ActivityActive, Timestamp: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if got := st.sessions["mer-1"]; !got.FirstSignalAt.Equal(stamped) {
		t.Fatalf("first signal moved: %v -> %v", stamped, got.FirstSignalAt)
	}
}

func TestActivity_SameStateRepeatAfterReceiptIsNoOp(t *testing.T) {
	m, st, _ := newManager()
	rec := working("mer-1")
	rec.FirstSignalAt = time.Now()
	st.sessions["mer-1"] = rec
	before := st.sessions["mer-1"]
	if err := m.ApplyActivitySignal(ctx, "mer-1", ports.ActivitySignal{Valid: true, State: domain.ActivityActive}); err != nil {
		t.Fatal(err)
	}
	if st.sessions["mer-1"] != before {
		t.Fatalf("same-state repeat after receipt must not rewrite: %+v", st.sessions["mer-1"])
	}
}

func TestMarkSpawnedClearsFirstSignal(t *testing.T) {
	m, st, _ := newManager()
	rec := working("mer-1")
	rec.FirstSignalAt = time.Now().Add(-time.Hour)
	st.sessions["mer-1"] = rec
	if err := m.MarkSpawned(ctx, "mer-1", domain.SessionMetadata{}); err != nil {
		t.Fatal(err)
	}
	if got := st.sessions["mer-1"]; !got.FirstSignalAt.IsZero() {
		t.Fatalf("spawn/restore must clear the receipt, got %+v", got)
	}
}

type fakeNotificationSink struct {
	intents []ports.NotificationIntent
	err     error
}

func reviewRoundCapObservation() ports.SCMObservation {
	return ports.SCMObservation{
		Fetched:  true,
		Provider: "github",
		Repo:     "o/r",
		PR: ports.SCMPRObservation{
			URL:          "https://github.com/o/r/pull/1",
			Number:       1,
			Title:        "still blocked",
			SourceBranch: "fix/review",
			TargetBranch: "main",
			HeadSHA:      "sha7",
		},
	}
}

func reviewHandoffState(t *testing.T, raw, key string) humanHandoffOutcome {
	t.Helper()
	var payload reactionPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("decode reaction payload: %v", err)
	}
	return payload.Handoffs[key]
}

func TestReviewRoundCapHandoffRequiresConfirmedNotification(t *testing.T) {
	for _, tt := range []struct {
		name string
		sink *fakeNotificationSink
	}{
		{name: "no notification sink"},
		{name: "notification delivery fails", sink: &fakeNotificationSink{err: errors.New("push unavailable")}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st := newFakeStore()
			m := New(st, nil)
			if tt.sink != nil {
				m = New(st, nil, WithNotificationSink(tt.sink))
			}
			st.sessions["mer-1"] = working("mer-1")
			obs := reviewRoundCapObservation()

			if err := m.ApplyReviewRoundCapHandoff(ctx, "mer-1", obs, 6); err == nil {
				t.Fatal("handoff error = nil, want undelivered fallback")
			}
			if got := st.sessions["mer-1"].Activity.State; got != domain.ActivityActive {
				t.Fatalf("activity = %q, want active until notification is confirmed", got)
			}
			key := reviewRoundCapHandoffKey(obs.PR.URL)
			got := reviewHandoffState(t, st.signatures[obs.PR.URL], key)
			if got.Outcome != humanHandoffBlocked || got.Reason != reviewRoundCapNotificationFailed {
				t.Fatalf("handoff = %+v, want blocked/%s", got, reviewRoundCapNotificationFailed)
			}
		})
	}
}

func TestReviewRoundCapHandoffRetriesBlockedOutcomeAndParksAfterDelivery(t *testing.T) {
	st := newFakeStore()
	sink := &fakeNotificationSink{err: errors.New("push unavailable")}
	m := New(st, nil, WithNotificationSink(sink))
	st.sessions["mer-1"] = working("mer-1")
	obs := reviewRoundCapObservation()

	if err := m.ApplyReviewRoundCapHandoff(ctx, "mer-1", obs, 6); err == nil {
		t.Fatal("first handoff error = nil, want failed delivery")
	}
	// Retry immediately. A normal review-fetch cadence must not overwrite or
	// defer a previously blocked, undelivered handoff.
	sink.err = nil
	if err := m.ApplyReviewRoundCapHandoff(ctx, "mer-1", obs, 6); err != nil {
		t.Fatalf("retry handoff: %v", err)
	}
	if len(sink.intents) != 2 {
		t.Fatalf("notification attempts = %d, want failed attempt plus immediate retry", len(sink.intents))
	}
	if got := st.sessions["mer-1"].Activity.State; got != domain.ActivityWaitingInput {
		t.Fatalf("activity = %q, want waiting_input after confirmed fallback", got)
	}
	key := reviewRoundCapHandoffKey(obs.PR.URL)
	got := reviewHandoffState(t, st.signatures[obs.PR.URL], key)
	if got.Outcome != humanHandoffNotified || got.Reason != reviewRoundCapNotificationFailed {
		t.Fatalf("handoff = %+v, want notified/%s", got, reviewRoundCapNotificationFailed)
	}

	if err := m.ApplyReviewRoundCapHandoff(ctx, "mer-1", obs, 6); err != nil {
		t.Fatalf("latched handoff repeat: %v", err)
	}
	if len(sink.intents) != 2 {
		t.Fatalf("latched handoff re-notified: %+v", sink.intents)
	}
}

func TestReviewRoundCapHandoffPreservesPendingInputSuppression(t *testing.T) {
	st := newFakeStore()
	sink := &fakeNotificationSink{}
	m := New(st, nil, WithNotificationSink(sink))
	rec := working("mer-1")
	rec.Activity.State = domain.ActivityBlocked
	st.sessions["mer-1"] = rec

	if err := m.ApplyReviewRoundCapHandoff(ctx, "mer-1", reviewRoundCapObservation(), 6); err != nil {
		t.Fatalf("pending-input handoff: %v", err)
	}
	if len(sink.intents) != 0 || len(st.signatures) != 0 {
		t.Fatalf("pending-input session emitted handoff: intents=%+v signatures=%+v", sink.intents, st.signatures)
	}
}

func (f *fakeNotificationSink) Notify(_ context.Context, intent ports.NotificationIntent) error {
	f.intents = append(f.intents, intent)
	return f.err
}

func TestActivity_WaitingInputTransitionEmitsNotification(t *testing.T) {
	st := newFakeStore()
	sink := &fakeNotificationSink{}
	m := New(st, nil, WithNotificationSink(sink))
	now := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)
	m.clock = func() time.Time { return now }
	st.sessions["mer-1"] = domain.SessionRecord{ID: "mer-1", ProjectID: "mer", DisplayName: "checkout-flow", Activity: domain.Activity{State: domain.ActivityActive, LastActivityAt: now.Add(-time.Minute)}, FirstSignalAt: now.Add(-time.Minute)}

	if err := m.ApplyActivitySignal(ctx, "mer-1", ports.ActivitySignal{Valid: true, State: domain.ActivityWaitingInput}); err != nil {
		t.Fatal(err)
	}
	if len(sink.intents) != 1 {
		t.Fatalf("intents = %d, want 1", len(sink.intents))
	}
	intent := sink.intents[0]
	if intent.Type != domain.NotificationNeedsInput || intent.SessionID != "mer-1" || intent.ProjectID != "mer" || intent.SessionDisplayName != "checkout-flow" {
		t.Fatalf("intent = %+v", intent)
	}
}

func TestActivity_RateLimitedPersistsNotifiesOnceAndResumesOnActivity(t *testing.T) {
	st := newFakeStore()
	sink := &fakeNotificationSink{}
	m := New(st, nil, WithNotificationSink(sink))
	now := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	m.clock = func() time.Time { return now }
	st.sessions["mer-1"] = domain.SessionRecord{ID: "mer-1", ProjectID: "mer", DisplayName: "checkout-flow", Activity: domain.Activity{State: domain.ActivityActive, LastActivityAt: now.Add(-time.Minute)}, FirstSignalAt: now.Add(-time.Minute)}

	if err := m.ApplyActivitySignal(ctx, "mer-1", ports.ActivitySignal{Valid: true, State: domain.ActivityIdle, Event: "stop-failure", ErrorType: "rate_limit"}); err != nil {
		t.Fatal(err)
	}
	parked := st.sessions["mer-1"]
	if parked.IsTerminated || parked.Activity.State != domain.ActivityRateLimited {
		t.Fatalf("parked session = %+v", parked)
	}
	if len(sink.intents) != 1 || sink.intents[0].TitleOverride != "Agent usage limit reached" || !strings.Contains(sink.intents[0].BodyOverride, "worktree is preserved") {
		t.Fatalf("rate-limit notification = %+v", sink.intents)
	}
	// A reconstructed manager proves the persisted state survives daemon
	// restart; an authoritative active hook is the safe resume signal.
	m = New(st, nil, WithNotificationSink(sink))
	m.clock = func() time.Time { return now.Add(time.Hour) }
	if err := m.ApplyActivitySignal(ctx, "mer-1", ports.ActivitySignal{Valid: true, State: domain.ActivityActive}); err != nil {
		t.Fatal(err)
	}
	resumed := st.sessions["mer-1"]
	if resumed.IsTerminated || resumed.Activity.State != domain.ActivityActive {
		t.Fatalf("resumed session = %+v", resumed)
	}
	if len(sink.intents) != 1 {
		t.Fatalf("resume emitted duplicate notifications: %+v", sink.intents)
	}
}

func TestActivity_WaitingInputSameStateDoesNotEmitNotification(t *testing.T) {
	st := newFakeStore()
	sink := &fakeNotificationSink{}
	m := New(st, nil, WithNotificationSink(sink))
	now := time.Now()
	st.sessions["mer-1"] = domain.SessionRecord{ID: "mer-1", ProjectID: "mer", Activity: domain.Activity{State: domain.ActivityWaitingInput, LastActivityAt: now}, FirstSignalAt: now}

	if err := m.ApplyActivitySignal(ctx, "mer-1", ports.ActivitySignal{Valid: true, State: domain.ActivityWaitingInput}); err != nil {
		t.Fatal(err)
	}
	if len(sink.intents) != 0 {
		t.Fatalf("same-state waiting_input emitted %+v", sink.intents)
	}
}

func TestActivity_BlockedTransitionEmitsNotification(t *testing.T) {
	st := newFakeStore()
	sink := &fakeNotificationSink{}
	m := New(st, nil, WithNotificationSink(sink))
	now := time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC)
	m.clock = func() time.Time { return now }
	st.sessions["mer-1"] = domain.SessionRecord{ID: "mer-1", ProjectID: "mer", DisplayName: "checkout-flow", Activity: domain.Activity{State: domain.ActivityActive, LastActivityAt: now.Add(-time.Minute)}, FirstSignalAt: now.Add(-time.Minute)}

	if err := m.ApplyActivitySignal(ctx, "mer-1", ports.ActivitySignal{Valid: true, State: domain.ActivityBlocked}); err != nil {
		t.Fatal(err)
	}
	if len(sink.intents) != 1 {
		t.Fatalf("intents = %d, want 1 (blocked is a needs-input entry)", len(sink.intents))
	}
	if sink.intents[0].Type != domain.NotificationNeedsInput {
		t.Fatalf("intent type = %q, want needs_input", sink.intents[0].Type)
	}
}

func TestActivity_WaitingInputToBlockedDoesNotReNotify(t *testing.T) {
	// waiting_input -> blocked is an in-family escalation: the user was already
	// pinged once for this pause, so no second notification and no telemetry
	// entry/exit pair.
	st := newFakeStore()
	sink := &fakeNotificationSink{}
	tele := &telemetrySink{}
	m := New(st, nil, WithNotificationSink(sink), WithTelemetry(tele))
	now := time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC)
	m.clock = func() time.Time { return now }
	st.sessions["mer-1"] = domain.SessionRecord{ID: "mer-1", ProjectID: "mer", Activity: domain.Activity{State: domain.ActivityWaitingInput, LastActivityAt: now.Add(-time.Minute)}, FirstSignalAt: now.Add(-time.Minute)}

	if err := m.ApplyActivitySignal(ctx, "mer-1", ports.ActivitySignal{Valid: true, State: domain.ActivityBlocked}); err != nil {
		t.Fatal(err)
	}
	if len(sink.intents) != 0 {
		t.Fatalf("in-family escalation emitted notification: %+v", sink.intents)
	}
	if len(tele.events) != 0 {
		t.Fatalf("in-family escalation emitted telemetry: %+v", tele.events)
	}
}

func TestActivity_BlockedEntryAndExitEmitTelemetry(t *testing.T) {
	st := newFakeStore()
	sink := &telemetrySink{}
	m := New(st, nil, WithTelemetry(sink))
	now := time.Unix(100, 0).UTC()
	m.clock = func() time.Time { return now }
	st.sessions["mer-1"] = domain.SessionRecord{
		ID:        "mer-1",
		ProjectID: "mer",
		Activity:  domain.Activity{State: domain.ActivityIdle, LastActivityAt: now.Add(-time.Minute)},
	}

	if err := m.ApplyActivitySignal(ctx, "mer-1", ports.ActivitySignal{Valid: true, State: domain.ActivityBlocked, Timestamp: now}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second)
	if err := m.ApplyActivitySignal(ctx, "mer-1", ports.ActivitySignal{Valid: true, State: domain.ActivityActive, Timestamp: now}); err != nil {
		t.Fatal(err)
	}

	if len(sink.events) != 2 {
		t.Fatalf("events = %#v, want entered/exited", sink.events)
	}
	if sink.events[0].Name != "ao.session.waiting_input_entered" || sink.events[1].Name != "ao.session.waiting_input_exited" {
		t.Fatalf("event names = %#v (family events keep the waiting_input_* names)", []string{sink.events[0].Name, sink.events[1].Name})
	}
	if got := sink.events[0].Payload["state"]; got != "blocked" {
		t.Fatalf("entered payload state = %#v, want blocked", got)
	}
}

func TestSCMObservation_ReadyToMergeSuppressedWhileBlocked(t *testing.T) {
	st := newFakeStore()
	sink := &fakeNotificationSink{}
	m := New(st, nil, WithNotificationSink(sink))
	rec := working("mer-1")
	rec.Activity.State = domain.ActivityBlocked
	st.sessions["mer-1"] = rec
	obs := ports.SCMObservation{
		Fetched:      true,
		PR:           ports.SCMPRObservation{URL: "https://github.com/o/r/pull/1", Number: 1},
		CI:           ports.SCMCIObservation{Summary: string(domain.CIPassing)},
		Mergeability: ports.SCMMergeabilityObservation{State: string(domain.MergeMergeable)},
	}
	if err := m.ApplySCMObservation(ctx, "mer-1", obs); err != nil {
		t.Fatal(err)
	}
	if len(sink.intents) != 0 {
		t.Fatalf("blocked session emitted ready notification: %+v", sink.intents)
	}
}

// blockOnNthGetStore wraps fakeStore and flips a session to ActivityBlocked on
// the Nth GetSession call, reproducing the reactions TOCTOU: the handler's
// entry guard (1st read) sees the session working, but a permission hook stores
// blocked before sendOnce's just-in-time re-read (2nd read).
type blockOnNthGetStore struct {
	*fakeStore
	id     domain.SessionID
	reads  int
	flipAt int
}

func (s *blockOnNthGetStore) GetSession(ctx context.Context, id domain.SessionID) (domain.SessionRecord, bool, error) {
	s.reads++
	if s.reads == s.flipAt {
		if rec, ok := s.sessions[s.id]; ok {
			rec.Activity.State = domain.ActivityBlocked
			s.sessions[s.id] = rec
		}
	}
	return s.fakeStore.GetSession(ctx, id)
}

func TestSendOnce_NoNudgeWhenBlockedAppearsBeforeSend(t *testing.T) {
	// The entry guard in ApplyPRObservation reads the session working (read #1);
	// a permission dialog then stores blocked before sendOnce's just-in-time
	// re-read (read #2), which must suppress the paste+Enter into the dialog.
	st := newFakeStore()
	st.sessions["mer-1"] = working("mer-1")
	bst := &blockOnNthGetStore{fakeStore: st, id: "mer-1", flipAt: 2}
	msg := &fakeMessenger{}
	m := New(bst, msg)
	o := ports.PRObservation{Fetched: true, URL: "pr1", CI: domain.CIFailing, Checks: []ports.PRCheckObservation{{Name: "build", CommitHash: "c1", Status: domain.PRCheckFailed, LogTail: "boom"}}}
	if err := m.ApplyPRObservation(ctx, "mer-1", o); err != nil {
		t.Fatal(err)
	}
	if len(msg.msgs) != 0 {
		t.Fatalf("nudge sent into a session that went blocked before send: %v", msg.msgs)
	}
}

func TestSCMObservation_PendingSubmitHandsOffOnceWithoutSpendingNudgeBudget(t *testing.T) {
	st := newFakeStore()
	sink := &fakeNotificationSink{}
	msg := &fakeMessenger{}
	m := New(st, msg, WithNotificationSink(sink))
	rec := working("mer-1")
	rec.Metadata.PendingSubmitFingerprint = "sha256-prompt"
	rec.Metadata.PendingSubmitRecoveryAttempted = true
	st.sessions["mer-1"] = rec
	obs := ports.SCMObservation{
		Fetched:  true,
		Provider: "github",
		Repo:     "o/r",
		PR:       ports.SCMPRObservation{URL: "https://github.com/o/r/pull/1", Number: 1, Title: "fix pending input", HeadSHA: "sha1"},
		CI: ports.SCMCIObservation{
			Summary:      string(domain.CIFailing),
			HeadSHA:      "sha1",
			FailedChecks: []ports.SCMCheckObservation{{Name: "build", Status: string(domain.PRCheckFailed), LogTail: "boom"}},
		},
	}

	if err := m.ApplySCMObservation(ctx, "mer-1", obs); err != nil {
		t.Fatalf("ApplySCMObservation: %v", err)
	}
	// Rebuild the manager to simulate a daemon refresh/replay. The durable
	// handoff and pending-submit facts must suppress both duplicate text and a
	// duplicate human notification.
	m = New(st, msg, WithNotificationSink(sink))
	if err := m.ApplySCMObservation(ctx, "mer-1", obs); err != nil {
		t.Fatalf("ApplySCMObservation after restart: %v", err)
	}
	if len(msg.msgs) != 0 {
		t.Fatalf("pending editor input received duplicate nudge text: %v", msg.msgs)
	}
	if len(sink.intents) != 1 {
		t.Fatalf("human handoff intents = %d, want exactly 1: %+v", len(sink.intents), sink.intents)
	}
	if got := sink.intents[0]; got.Type != domain.NotificationNeedsInput || got.PRURL != obs.PR.URL {
		t.Fatalf("handoff = %+v, want needs-input notification for %s", got, obs.PR.URL)
	}
	if got := sink.intents[0]; got.TitleOverride == "" || !strings.Contains(got.BodyOverride, "pull request feedback") {
		t.Fatalf("handoff copy does not surface the suppressed PR condition: %+v", got)
	}
	var payload reactionPayload
	if err := json.Unmarshal([]byte(st.signatures[obs.PR.URL]), &payload); err != nil {
		t.Fatalf("decode reaction payload: %v", err)
	}
	key := "ci:" + obs.PR.URL
	if got := payload.Attempts[key]; got != 0 {
		t.Fatalf("nudge attempts = %d, want 0 while delivery is suppressed", got)
	}
	if len(payload.Handoffs) != 1 {
		t.Fatalf("durable handoffs = %+v, want exactly one notified outcome", payload.Handoffs)
	}
	for _, handoff := range payload.Handoffs {
		if handoff.Outcome != humanHandoffNotified || handoff.Reason != suppressedNudgeNotificationFailed {
			t.Fatalf("handoff = %+v, want notified/%s", handoff, suppressedNudgeNotificationFailed)
		}
	}
}

func TestPRObservation_IdleReviewSendFailureHandsOffOnceAcrossRestart(t *testing.T) {
	st := newFakeStore()
	sink := &fakeNotificationSink{}
	msg := &fakeMessenger{err: errors.New("pane unavailable")}
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	rec := working("mer-1")
	rec.DisplayName = "review worker"
	rec.Activity = domain.Activity{State: domain.ActivityIdle, LastActivityAt: now.Add(-2 * time.Minute)}
	st.sessions[rec.ID] = rec
	o := ports.PRObservation{
		Fetched: true,
		URL:     "https://github.com/o/r/pull/1",
		HeadSHA: "sha1",
		Number:  1,
		Title:   "fix review feedback",
		Review:  domain.ReviewChangesRequest,
		Comments: []ports.PRCommentObservation{{
			ID: "c1", ThreadID: "t1", Author: "reviewer", Body: "fix this",
		}},
	}

	m := New(st, msg, WithNotificationSink(sink))
	m.clock = func() time.Time { return now }
	if err := m.ApplyPRObservation(ctx, rec.ID, o); err == nil {
		t.Fatal("first review send error = nil, want failed agent delivery")
	}
	// Restart while the same agent delivery is still failing. The durable human
	// handoff must suppress duplicate notifications without turning the failed
	// send into delivery proof.
	m = New(st, msg, WithNotificationSink(sink))
	m.clock = func() time.Time { return now.Add(time.Minute) }
	if err := m.ApplyPRObservation(ctx, rec.ID, o); err == nil {
		t.Fatal("replayed review send error = nil, want failed agent delivery")
	}
	if len(sink.intents) != 1 {
		t.Fatalf("human handoff intents = %d, want exactly 1: %+v", len(sink.intents), sink.intents)
	}
	if got := sink.intents[0]; got.Type != domain.NotificationNeedsInput || got.PRURL != o.URL {
		t.Fatalf("handoff = %+v, want needs-input notification for %s", got, o.URL)
	}
	var payload reactionPayload
	if err := json.Unmarshal([]byte(st.signatures[o.URL]), &payload); err != nil {
		t.Fatalf("decode reaction payload: %v", err)
	}
	if got := payload.Seen["review:"+o.URL]; got != "" {
		t.Fatalf("failed review send recorded as delivered: %q", got)
	}
	if len(payload.Handoffs) != 1 {
		t.Fatalf("durable handoffs = %+v, want one send-failure handoff", payload.Handoffs)
	}
	for _, handoff := range payload.Handoffs {
		if handoff.Outcome != humanHandoffNotified || handoff.Reason != reviewNudgeSendFailed {
			t.Fatalf("handoff = %+v, want notified/%s", handoff, reviewNudgeSendFailed)
		}
	}
}

func TestReviewFailureHandoffs_JITRecheckSkipsAfterConcurrentRecovery(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	prURL := "https://github.com/o/r/pull/1"
	for _, tc := range []struct {
		name            string
		flipAfterRead   int
		apply           func(*Manager, domain.SessionID) error
		wantDeliveryErr bool
	}{
		{
			name:          "fetch failure",
			flipAfterRead: 1,
			apply: func(m *Manager, id domain.SessionID) error {
				return m.ApplySCMReviewFetchFailure(ctx, id, ports.SCMObservation{
					Fetched: true,
					PR:      ports.SCMPRObservation{URL: prURL, Number: 1},
					Review:  ports.SCMReviewObservation{Decision: string(domain.ReviewChangesRequest)},
				})
			},
		},
		{
			name:            "review send failure",
			flipAfterRead:   3,
			wantDeliveryErr: true,
			apply: func(m *Manager, id domain.SessionID) error {
				return m.ApplyPRObservation(ctx, id, ports.PRObservation{
					Fetched:  true,
					URL:      prURL,
					HeadSHA:  "sha1",
					Review:   domain.ReviewChangesRequest,
					Comments: []ports.PRCommentObservation{{ID: "c1", ThreadID: "t1", Author: "reviewer", Body: "fix"}},
				})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newFakeStore()
			sink := &fakeNotificationSink{}
			rec := working("mer-1")
			rec.Activity = domain.Activity{State: domain.ActivityIdle, LastActivityAt: now.Add(-2 * time.Minute)}
			st.sessions[rec.ID] = rec
			st.afterSessionRead = func(id domain.SessionID, read int) {
				if read != tc.flipAfterRead {
					return
				}
				active := st.sessions[id]
				active.Activity = domain.Activity{State: domain.ActivityActive, LastActivityAt: now}
				st.sessions[id] = active
			}
			m := New(st, &fakeMessenger{err: errors.New("pane unavailable")}, WithNotificationSink(sink))
			m.clock = func() time.Time { return now }
			err := tc.apply(m, rec.ID)
			if tc.wantDeliveryErr && err == nil {
				t.Fatal("agent delivery error = nil")
			}
			if !tc.wantDeliveryErr && err != nil {
				t.Fatal(err)
			}
			if len(sink.intents) != 0 {
				t.Fatalf("recovered episode received stale failure handoff: %+v", sink.intents)
			}
		})
	}
}

func TestSCMReviewFetchFailureHandsOffObservedOverlayWithoutMutatingDeliveryProof(t *testing.T) {
	st := newFakeStore()
	sink := &fakeNotificationSink{}
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	rec := working("mer-1")
	rec.Activity = domain.Activity{State: domain.ActivityIdle, LastActivityAt: now.Add(-2 * time.Minute)}
	st.sessions[rec.ID] = rec
	prURL := "https://github.com/o/r/pull/1"
	obs := ports.SCMObservation{
		Fetched:  true,
		Provider: "github",
		Repo:     "o/r",
		PR:       ports.SCMPRObservation{URL: prURL, Number: 1, Title: "review overlay"},
		Review:   ports.SCMReviewObservation{Decision: string(domain.ReviewChangesRequest)},
	}

	// A durable escalation/handoff with no Seen review signature is explicitly
	// undelivered. It must not become agent-delivery proof after restart, but the
	// observed idle review overlay still needs its own human fetch-failure fallback.
	undelivered := reactionPayload{Handoffs: map[string]humanHandoffOutcome{
		"review-handoff:" + prURL + ":round-cap": {Outcome: humanHandoffNotified, Reason: reviewRoundCapNotificationFailed},
	}}
	raw, err := json.Marshal(undelivered)
	if err != nil {
		t.Fatal(err)
	}
	st.signatures[prURL] = string(raw)
	m := New(st, nil, WithNotificationSink(sink))
	m.clock = func() time.Time { return now }
	if err := m.ApplySCMReviewFetchFailure(ctx, rec.ID, obs); err != nil {
		t.Fatalf("undelivered fetch fallback: %v", err)
	}
	m = New(st, nil, WithNotificationSink(sink))
	m.clock = func() time.Time { return now.Add(time.Minute) }
	if err := m.ApplySCMReviewFetchFailure(ctx, rec.ID, obs); err != nil {
		t.Fatalf("replayed undelivered fetch fallback: %v", err)
	}
	if len(sink.intents) != 1 {
		t.Fatalf("observed-overlay fetch fallback intents = %d, want exactly 1: %+v", len(sink.intents), sink.intents)
	}
	var afterUndelivered reactionPayload
	if err := json.Unmarshal([]byte(st.signatures[prURL]), &afterUndelivered); err != nil {
		t.Fatalf("decode undelivered fallback: %v", err)
	}
	if got := afterUndelivered.Seen["review:"+prURL]; got != "" {
		t.Fatalf("escalated-undelivered state became agent delivery proof: %q", got)
	}

	// Once a real review delivery signature is present, the same observed overlay
	// is already covered by the durable fetch-failure handoff and must not notify
	// again or rewrite that delivery proof.
	delivered := reactionPayload{
		Seen:     map[string]string{"review:" + prURL: "thread-c1"},
		Handoffs: afterUndelivered.Handoffs,
	}
	raw, err = json.Marshal(delivered)
	if err != nil {
		t.Fatal(err)
	}
	st.signatures[prURL] = string(raw)
	m = New(st, nil, WithNotificationSink(sink))
	m.clock = func() time.Time { return now }
	if err := m.ApplySCMReviewFetchFailure(ctx, rec.ID, obs); err != nil {
		t.Fatalf("delivered fetch fallback: %v", err)
	}
	m = New(st, nil, WithNotificationSink(sink))
	m.clock = func() time.Time { return now.Add(time.Minute) }
	if err := m.ApplySCMReviewFetchFailure(ctx, rec.ID, obs); err != nil {
		t.Fatalf("replayed delivered fetch fallback: %v", err)
	}
	if len(sink.intents) != 1 {
		t.Fatalf("fetch-failure handoff intents = %d, want still exactly 1: %+v", len(sink.intents), sink.intents)
	}
	var persisted reactionPayload
	if err := json.Unmarshal([]byte(st.signatures[prURL]), &persisted); err != nil {
		t.Fatalf("decode persisted handoff: %v", err)
	}
	if got := persisted.Seen["review:"+prURL]; got != "thread-c1" {
		t.Fatalf("fetch fallback mutated agent delivery proof: %q", got)
	}

	// A genuinely clean current pipeline must not be re-alerted from that stale
	// Seen value. Current/local overlay state, not historical delivery, owns the
	// fetch-failure fallback decision.
	clean := obs
	clean.PR.HeadSHA = "sha-clean"
	clean.Review.HeadSHA = "sha-clean"
	clean.Review.Decision = string(domain.ReviewNone)
	clean.Mergeability.State = string(domain.MergeUnknown)
	clean.CI.Summary = string(domain.CIPassing)
	m = New(st, nil, WithNotificationSink(sink))
	m.clock = func() time.Time { return now.Add(2 * time.Minute) }
	if err := m.ApplySCMReviewFetchFailure(ctx, rec.ID, clean); err != nil {
		t.Fatalf("clean pipeline fetch failure: %v", err)
	}
	if len(sink.intents) != 1 {
		t.Fatalf("stale Seen re-alerted a clean current pipeline: %+v", sink.intents)
	}

	// Recovery followed by a later idle episode must rearm the one-time
	// fallback even when the provider overlay/signature is otherwise identical.
	recovered := st.sessions[rec.ID]
	recovered.Activity = domain.Activity{State: domain.ActivityActive, LastActivityAt: now.Add(3 * time.Minute)}
	st.sessions[rec.ID] = recovered
	recovered.Activity = domain.Activity{State: domain.ActivityIdle, LastActivityAt: now.Add(4 * time.Minute)}
	st.sessions[rec.ID] = recovered
	m = New(st, nil, WithNotificationSink(sink))
	m.clock = func() time.Time { return now.Add(6 * time.Minute) }
	if err := m.ApplySCMReviewFetchFailure(ctx, rec.ID, obs); err != nil {
		t.Fatalf("new idle episode fetch fallback: %v", err)
	}
	if len(sink.intents) != 2 {
		t.Fatalf("recovered fetch-failure episode did not rearm: %+v", sink.intents)
	}
}

func TestIdleReviewHandoffIsCanonicalAcrossFetchAndSnapshotPathsAfterRestart(t *testing.T) {
	st := newFakeStore()
	sink := &fakeNotificationSink{}
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	prURL := "https://github.com/o/r/pull/1"
	rec := working("mer-1")
	rec.Activity = domain.Activity{State: domain.ActivityIdle, LastActivityAt: now.Add(-2 * time.Minute)}
	rec.FirstSignalAt = now.Add(-2 * time.Minute)
	st.sessions[rec.ID] = rec
	failedFetch := ports.SCMObservation{
		Fetched:  true,
		Provider: "github",
		Repo:     "o/r",
		PR:       ports.SCMPRObservation{URL: prURL, Number: 1, HeadSHA: "sha1"},
		Review:   ports.SCMReviewObservation{Decision: string(domain.ReviewChangesRequest), HeadSHA: "sha1"},
	}
	m := New(st, nil, WithNotificationSink(sink))
	m.clock = func() time.Time { return now }
	if err := m.ApplySCMReviewFetchFailure(ctx, rec.ID, failedFetch); err != nil {
		t.Fatalf("fetch-failure handoff: %v", err)
	}

	// Restart, then successfully fetch a complete actionable backlog for the
	// same idle episode. With no agent-delivery proof the snapshot path also
	// wants a handoff, but it must reuse the fetch path's canonical latch.
	complete := idleReviewSnapshot(prURL, false, ports.SCMReviewThreadObservation{
		ID: "t1", Comments: []ports.SCMReviewCommentObservation{{ID: "c1", Author: "alice", Body: "fix"}},
	})
	m = New(st, &fakeMessenger{}, WithNotificationSink(sink))
	m.clock = func() time.Time { return now.Add(time.Minute) }
	if err := m.ApplyIdleReviewSnapshot(ctx, rec.ID, complete); err != nil {
		t.Fatalf("complete-snapshot handoff: %v", err)
	}
	if len(sink.intents) != 1 {
		t.Fatalf("same idle episode notified across both paths: %+v", sink.intents)
	}
	var payload reactionPayload
	if err := json.Unmarshal([]byte(st.signatures[prURL]), &payload); err != nil {
		t.Fatal(err)
	}
	key := idleReviewHandoffKey(prURL, rec.Activity.LastActivityAt)
	if len(payload.Handoffs) != 1 || payload.Handoffs[key].Outcome != humanHandoffNotified {
		t.Fatalf("handoffs = %+v, want one canonical notified latch at %q", payload.Handoffs, key)
	}
}

func TestIdleReviewSnapshotCannotBypassPendingClaimContract(t *testing.T) {
	st := newFakeStore()
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	prURL := "https://github.com/o/r/pull/1"
	rec := working("mer-1")
	rec.Activity = domain.Activity{State: domain.ActivityIdle, LastActivityAt: now.Add(-2 * time.Minute)}
	rec.FirstSignalAt = rec.Activity.LastActivityAt
	st.sessions[rec.ID] = rec
	st.designContracts[prURL] = designcontract.BuildSeed("61", "## Invariants\n- Preserve claim order.")
	st.pendingContractDelivery[prURL] = rec.ID
	snapshot := idleReviewSnapshot(prURL, false, ports.SCMReviewThreadObservation{ID: "t1", Comments: []ports.SCMReviewCommentObservation{{ID: "c1", Author: "alice", Body: "fix"}}})
	msg := &fakeMessenger{err: errors.New("pane unavailable")}
	m := New(st, msg)
	m.SetAutomatedMessageSender(guardedAutomatedSender{guard: m.guard})
	m.clock = func() time.Time { return now }
	if err := m.ApplyIdleReviewSnapshot(ctx, rec.ID, snapshot); err == nil {
		t.Fatal("idle review bypassed failed claim-ready delivery")
	}
	if st.pendingContractDelivery[prURL] != rec.ID || len(msg.msgs) != 0 || st.signatures[prURL] != "" {
		t.Fatalf("idle review crossed pending claim barrier: pending=%q msgs=%v signature=%q", st.pendingContractDelivery[prURL], msg.msgs, st.signatures[prURL])
	}
}

func TestIdleReviewSnapshotMigratesLegacyFailureLatchBeforeDedup(t *testing.T) {
	st := newFakeStore()
	sink := &fakeNotificationSink{}
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	prURL := "https://github.com/o/r/pull/1"
	rec := working("mer-1")
	rec.Activity = domain.Activity{State: domain.ActivityIdle, LastActivityAt: now.Add(-2 * time.Minute)}
	rec.FirstSignalAt = now.Add(-2 * time.Minute)
	st.sessions[rec.ID] = rec
	legacy := reactionPayload{Handoffs: map[string]humanHandoffOutcome{
		"review-failure:" + prURL + ":legacy-condition": {Outcome: humanHandoffNotified, Reason: reviewFetchFailed},
	}}
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	st.signatures[prURL] = string(raw)

	// The first post-upgrade path is a complete snapshot, not another fetch
	// failure. It must collapse the old #111 key before checking canonical dedup.
	complete := idleReviewSnapshot(prURL, false, ports.SCMReviewThreadObservation{
		ID: "t1", Comments: []ports.SCMReviewCommentObservation{{ID: "c1", Author: "alice", Body: "fix"}},
	})
	m := New(st, &fakeMessenger{}, WithNotificationSink(sink))
	m.clock = func() time.Time { return now }
	if err := m.ApplyIdleReviewSnapshot(ctx, rec.ID, complete); err != nil {
		t.Fatal(err)
	}
	if len(sink.intents) != 0 {
		t.Fatalf("legacy notified latch duplicated on snapshot-first upgrade: %+v", sink.intents)
	}
	var got reactionPayload
	if err := json.Unmarshal([]byte(st.signatures[prURL]), &got); err != nil {
		t.Fatal(err)
	}
	key := idleReviewHandoffKey(prURL, rec.Activity.LastActivityAt)
	if len(got.Handoffs) != 1 || got.Handoffs[key].Outcome != humanHandoffNotified {
		t.Fatalf("legacy latch was not collapsed to canonical key %q: %+v", key, got.Handoffs)
	}
}

func idleReviewSnapshot(prURL string, partial bool, threads ...ports.SCMReviewThreadObservation) ports.SCMObservation {
	return ports.SCMObservation{
		Fetched:  true,
		Provider: "github",
		Repo:     "o/r",
		PR: ports.SCMPRObservation{
			URL:     prURL,
			Number:  1,
			Title:   "idle review backlog",
			HeadSHA: "sha1",
		},
		Review: ports.SCMReviewObservation{
			Decision: string(domain.ReviewChangesRequest),
			HeadSHA:  "sha1",
			Threads:  threads,
			Partial:  partial,
		},
	}
}

func idleReviewDeliverySignature(obs ports.SCMObservation) string {
	return reviewCommentsSignature(unresolvedReviewComments(scmToPRObservation(obs).Comments))
}

func TestReviewSignatureCoversV2ShrinkingBacklogOnly(t *testing.T) {
	delivered := reviewCommentsSignature([]ports.PRCommentObservation{
		{ID: "c1", ThreadID: "t1", Author: "alice", File: "a.go", Line: 1, Body: "first"},
		{ID: "c2", ThreadID: "t2", Author: "bob", File: "b.go", Line: 2, Body: "second"},
	})
	remaining := reviewCommentsSignature([]ports.PRCommentObservation{
		{ID: "c2", ThreadID: "t2", Author: "bob", File: "b.go", Line: 2, Body: "second"},
	})
	if !reviewSignatureCovers(delivered, remaining) {
		t.Fatal("v2 delivery proof must cover a shrinking, unchanged backlog")
	}
	edited := reviewCommentsSignature([]ports.PRCommentObservation{
		{ID: "c2", ThreadID: "t2", Author: "bob", File: "b.go", Line: 2, Body: "revised"},
	})
	if reviewSignatureCovers(delivered, edited) {
		t.Fatal("v2 delivery proof must not cover edited actionable content")
	}
}

func TestIdleReviewSnapshot_DeliveredNudgesDeferUntilBudgetExhausted(t *testing.T) {
	st := newFakeStore()
	msg := &fakeMessenger{}
	sink := &fakeNotificationSink{}
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	rec := working("mer-1")
	rec.Activity = domain.Activity{State: domain.ActivityIdle, LastActivityAt: now.Add(-2 * time.Minute)}
	rec.FirstSignalAt = now.Add(-2 * time.Minute)
	st.sessions[rec.ID] = rec
	prURL := "https://github.com/o/r/pull/1"
	obs := idleReviewSnapshot(prURL, false, ports.SCMReviewThreadObservation{
		ID: "t1", Path: "a.go", Line: 9,
		Comments: []ports.SCMReviewCommentObservation{{ID: "c1", Author: "alice", Body: "fix this"}},
	})
	delivered := reactionPayload{Seen: map[string]string{"review:" + prURL: idleReviewDeliverySignature(obs)}}
	raw, err := json.Marshal(delivered)
	if err != nil {
		t.Fatal(err)
	}
	st.signatures[prURL] = string(raw)
	m := New(st, msg, WithNotificationSink(sink))
	m.clock = func() time.Time { return now }

	for i := 0; i < idleReviewMaxNudges; i++ {
		if err := m.ApplyIdleReviewSnapshot(ctx, rec.ID, obs); err != nil {
			t.Fatalf("nudge %d: %v", i+1, err)
		}
		if len(sink.intents) != 0 {
			t.Fatalf("nudge %d notified before budget exhaustion: %+v", i+1, sink.intents)
		}
	}
	if got := len(msg.msgs); got != idleReviewMaxNudges {
		t.Fatalf("agent nudges = %d, want %d", got, idleReviewMaxNudges)
	}
	if err := m.ApplyIdleReviewSnapshot(ctx, rec.ID, obs); err != nil {
		t.Fatalf("exhaustion handoff: %v", err)
	}
	if err := m.ApplyIdleReviewSnapshot(ctx, rec.ID, obs); err != nil {
		t.Fatalf("latched exhaustion handoff: %v", err)
	}
	if len(sink.intents) != 1 {
		t.Fatalf("exhaustion notifications = %d, want exactly 1: %+v", len(sink.intents), sink.intents)
	}
}

func TestIdleReviewSnapshot_JITRecheckSkipsActionsAfterConcurrentRecovery(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	prURL := "https://github.com/o/r/pull/1"
	human := ports.SCMReviewThreadObservation{
		ID: "t1", Comments: []ports.SCMReviewCommentObservation{{ID: "c1", Author: "alice", Body: "fix"}},
	}
	for _, tc := range []struct {
		name      string
		partial   bool
		seedProof bool
		newIdle   bool
	}{
		{name: "nudge after active recovery", seedProof: true},
		{name: "handoff after a new idle episode", partial: true, newIdle: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newFakeStore()
			msg := &fakeMessenger{}
			sink := &fakeNotificationSink{}
			rec := working("mer-1")
			rec.Activity = domain.Activity{State: domain.ActivityIdle, LastActivityAt: now.Add(-2 * time.Minute)}
			rec.FirstSignalAt = rec.Activity.LastActivityAt
			st.sessions[rec.ID] = rec
			obs := idleReviewSnapshot(prURL, tc.partial, human)
			proof := reactionPayload{}
			if tc.seedProof {
				proof.Seen = map[string]string{"review:" + prURL: idleReviewDeliverySignature(obs)}
			}
			if tc.newIdle {
				proof.Attempts = map[string]int{
					idleReviewEpisodeKey(prURL, rec.Activity.LastActivityAt): 1,
					idleReviewEpisodeKey(prURL, now):                         2,
				}
				proof.Handoffs = map[string]humanHandoffOutcome{
					idleReviewHandoffKey(prURL, rec.Activity.LastActivityAt): {Outcome: humanHandoffBlocked, Reason: idleReviewUndeliverable},
					idleReviewHandoffKey(prURL, now):                         {Outcome: humanHandoffNotified, Reason: idleReviewNudgeExhausted},
				}
			}
			if len(proof.Seen) > 0 || len(proof.Attempts) > 0 || len(proof.Handoffs) > 0 {
				raw, err := json.Marshal(proof)
				if err != nil {
					t.Fatal(err)
				}
				st.signatures[prURL] = string(raw)
			}
			// The first eligibility read returns idle, then a concurrent activity
			// reducer commits recovery before this decision reaches its side effect.
			st.afterSessionRead = func(id domain.SessionID, read int) {
				if read != 1 {
					return
				}
				active := st.sessions[id]
				state := domain.ActivityActive
				if tc.newIdle {
					state = domain.ActivityIdle
				}
				active.Activity = domain.Activity{State: state, LastActivityAt: now}
				st.sessions[id] = active
			}
			m := New(st, msg, WithNotificationSink(sink))
			m.clock = func() time.Time { return now }
			if err := m.ApplyIdleReviewSnapshot(ctx, rec.ID, obs); err != nil {
				t.Fatal(err)
			}
			if len(msg.msgs) != 0 || len(sink.intents) != 0 {
				t.Fatalf("recovered episode acted on stale idle evidence: nudges=%d notifications=%d", len(msg.msgs), len(sink.intents))
			}
			if tc.newIdle {
				var got reactionPayload
				if err := json.Unmarshal([]byte(st.signatures[prURL]), &got); err != nil {
					t.Fatal(err)
				}
				oldAttempt := idleReviewEpisodeKey(prURL, rec.Activity.LastActivityAt)
				oldHandoff := idleReviewHandoffKey(prURL, rec.Activity.LastActivityAt)
				newAttempt := idleReviewEpisodeKey(prURL, now)
				newHandoff := idleReviewHandoffKey(prURL, now)
				if _, ok := got.Attempts[oldAttempt]; ok {
					t.Fatalf("stale attempt survived episode cleanup: %+v", got.Attempts)
				}
				if _, ok := got.Handoffs[oldHandoff]; ok {
					t.Fatalf("stale handoff survived episode cleanup: %+v", got.Handoffs)
				}
				if got.Attempts[newAttempt] != 2 || got.Handoffs[newHandoff].Outcome != humanHandoffNotified {
					t.Fatalf("stale cleanup erased newer episode: attempts=%+v handoffs=%+v", got.Attempts, got.Handoffs)
				}
			}
		})
	}
}

func TestIdleReviewSnapshot_FinalGuardRateLimitParksWithoutHandoff(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	prURL := "https://github.com/o/r/pull/1"
	st := newFakeStore()
	msg := &fakeMessenger{}
	sink := &fakeNotificationSink{}
	rec := working("mer-1")
	rec.Activity = domain.Activity{State: domain.ActivityIdle, LastActivityAt: now.Add(-2 * time.Minute)}
	rec.FirstSignalAt = rec.Activity.LastActivityAt
	st.sessions[rec.ID] = rec
	obs := idleReviewSnapshot(prURL, false, ports.SCMReviewThreadObservation{
		ID: "t1", Comments: []ports.SCMReviewCommentObservation{{ID: "c1", Author: "alice", Body: "fix"}},
	})
	proof := reactionPayload{
		Seen:     map[string]string{"review:" + prURL: idleReviewDeliverySignature(obs)},
		Attempts: map[string]int{idleReviewEpisodeKey(prURL, rec.Activity.LastActivityAt): 1},
	}
	raw, err := json.Marshal(proof)
	if err != nil {
		t.Fatal(err)
	}
	st.signatures[prURL] = string(raw)
	// The initial eligibility read returns the old idle episode. The provider
	// limit then lands before the guard's final pre-write read, which must park
	// silently rather than turn suppression into a false review-failure handoff.
	st.afterSessionRead = func(id domain.SessionID, read int) {
		if read != 1 {
			return
		}
		parked := st.sessions[id]
		parked.Activity = domain.Activity{State: domain.ActivityRateLimited, LastActivityAt: now}
		st.sessions[id] = parked
	}
	m := New(st, msg, WithNotificationSink(sink))
	m.clock = func() time.Time { return now }
	if err := m.ApplyIdleReviewSnapshot(ctx, rec.ID, obs); err != nil {
		t.Fatal(err)
	}
	if len(msg.msgs) != 0 || len(sink.intents) != 0 {
		t.Fatalf("rate-limit guard suppression acted as review failure: nudges=%d notifications=%d", len(msg.msgs), len(sink.intents))
	}
	if got := st.sessions[rec.ID]; got.IsTerminated || got.Activity.State != domain.ActivityRateLimited {
		t.Fatalf("rate-limited session did not remain parked: %+v", got)
	}
	var persisted reactionPayload
	if err := json.Unmarshal([]byte(st.signatures[prURL]), &persisted); err != nil {
		t.Fatal(err)
	}
	if _, ok := persisted.Attempts[idleReviewEpisodeKey(prURL, rec.Activity.LastActivityAt)]; ok {
		t.Fatalf("old idle episode survived rate-limit cleanup: %+v", persisted.Attempts)
	}
}

func TestIdleReviewSnapshot_FailsClosedExactlyOnce(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	prURL := "https://github.com/o/r/pull/1"
	human := ports.SCMReviewThreadObservation{
		ID: "t1", Comments: []ports.SCMReviewCommentObservation{{ID: "c1", Author: "alice", Body: "fix"}},
	}
	bot := ports.SCMReviewThreadObservation{
		ID: "bot-1", IsBot: true,
		Comments: []ports.SCMReviewCommentObservation{{ID: "bc1", Author: "review-bot", Body: "context", IsBot: true}},
	}
	for _, tc := range []struct {
		name      string
		obs       ports.SCMObservation
		seedProof bool
		noSignal  bool
		messenger *fakeMessenger
	}{
		{name: "partial snapshot is uncertain", obs: idleReviewSnapshot(prURL, true, bot), seedProof: true, messenger: &fakeMessenger{}},
		{name: "non-actionable backlog", obs: idleReviewSnapshot(prURL, false, bot), seedProof: true, messenger: &fakeMessenger{}},
		{name: "no actual delivery proof", obs: idleReviewSnapshot(prURL, false, human), messenger: &fakeMessenger{}},
		{name: "no positive idle evidence", obs: idleReviewSnapshot(prURL, false, human), seedProof: true, noSignal: true, messenger: &fakeMessenger{}},
		{name: "send failure", obs: idleReviewSnapshot(prURL, false, human), seedProof: true, messenger: &fakeMessenger{err: errors.New("pane unavailable")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newFakeStore()
			sink := &fakeNotificationSink{}
			rec := working("mer-1")
			rec.Activity = domain.Activity{State: domain.ActivityIdle, LastActivityAt: now.Add(-2 * time.Minute)}
			if !tc.noSignal {
				rec.FirstSignalAt = now.Add(-2 * time.Minute)
			}
			st.sessions[rec.ID] = rec
			if tc.seedProof {
				proof := reactionPayload{Seen: map[string]string{"review:" + prURL: idleReviewDeliverySignature(tc.obs)}}
				raw, err := json.Marshal(proof)
				if err != nil {
					t.Fatal(err)
				}
				st.signatures[prURL] = string(raw)
			}
			m := New(st, tc.messenger, WithNotificationSink(sink))
			m.clock = func() time.Time { return now }
			_ = m.ApplyIdleReviewSnapshot(ctx, rec.ID, tc.obs)
			m = New(st, tc.messenger, WithNotificationSink(sink))
			m.clock = func() time.Time { return now.Add(time.Minute) }
			_ = m.ApplyIdleReviewSnapshot(ctx, rec.ID, tc.obs)
			if len(sink.intents) != 1 {
				t.Fatalf("notifications = %d, want exactly 1: %+v", len(sink.intents), sink.intents)
			}
		})
	}
}

func TestIdleReviewSnapshot_CleanOrRecoveredDoesNotAlertAndRearms(t *testing.T) {
	st := newFakeStore()
	msg := &fakeMessenger{}
	sink := &fakeNotificationSink{}
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	prURL := "https://github.com/o/r/pull/1"
	backlog := idleReviewSnapshot(prURL, false, ports.SCMReviewThreadObservation{
		ID: "t1", Comments: []ports.SCMReviewCommentObservation{{ID: "c1", Author: "alice", Body: "fix"}},
	})
	proof := reactionPayload{Seen: map[string]string{"review:" + prURL: idleReviewDeliverySignature(backlog)}}
	raw, err := json.Marshal(proof)
	if err != nil {
		t.Fatal(err)
	}
	st.signatures[prURL] = string(raw)
	rec := working("mer-1")
	rec.Activity = domain.Activity{State: domain.ActivityIdle, LastActivityAt: now.Add(-2 * time.Minute)}
	rec.FirstSignalAt = now.Add(-2 * time.Minute)
	st.sessions[rec.ID] = rec
	st.prs[rec.ID] = []domain.PullRequest{{URL: prURL}}
	m := New(st, msg, WithNotificationSink(sink))
	m.clock = func() time.Time { return now }
	if err := m.ApplyIdleReviewSnapshot(ctx, rec.ID, backlog); err != nil {
		t.Fatal(err)
	}

	if err := m.ApplyActivitySignal(ctx, rec.ID, ports.ActivitySignal{Valid: true, State: domain.ActivityActive, Timestamp: now.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	if err := m.ApplyActivitySignal(ctx, rec.ID, ports.ActivitySignal{Valid: true, State: domain.ActivityIdle, Timestamp: now.Add(2 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	m.clock = func() time.Time { return now.Add(2 * time.Minute) }
	if err := m.ApplyIdleReviewSnapshot(ctx, rec.ID, backlog); err != nil {
		t.Fatal(err)
	}
	if got := len(msg.msgs); got != 2 {
		t.Fatalf("recovered episode did not rearm: nudges=%d want 2", got)
	}

	clean := idleReviewSnapshot(prURL, false)
	if err := m.ApplyIdleReviewSnapshot(ctx, rec.ID, clean); err != nil {
		t.Fatal(err)
	}
	if len(sink.intents) != 0 {
		t.Fatalf("clean PR overlay emitted stuck alert: %+v", sink.intents)
	}
}

func TestActivityRecoveryPromptlyClearsDurableIdleReviewState(t *testing.T) {
	st := newFakeStore()
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	prURL := "https://github.com/o/r/pull/1"
	rec := working("mer-1")
	rec.Activity = domain.Activity{State: domain.ActivityIdle, LastActivityAt: now.Add(-2 * time.Minute)}
	rec.FirstSignalAt = now.Add(-2 * time.Minute)
	st.sessions[rec.ID] = rec
	st.prs[rec.ID] = []domain.PullRequest{{URL: prURL}}
	payload := reactionPayload{
		Attempts: map[string]int{idleReviewEpisodeKey(prURL, rec.Activity.LastActivityAt): 2},
		Handoffs: map[string]humanHandoffOutcome{
			idleReviewHandoffKey(prURL, rec.Activity.LastActivityAt): {Outcome: humanHandoffNotified, Reason: idleReviewNudgeFailed},
			"review-failure:" + prURL + ":old":                       {Outcome: humanHandoffNotified, Reason: reviewFetchFailed},
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	st.signatures[prURL] = string(raw)
	m := New(st, &fakeMessenger{})
	m.clock = func() time.Time { return now }

	if err := m.ApplyActivitySignal(ctx, rec.ID, ports.ActivitySignal{Valid: true, State: domain.ActivityActive, Timestamp: now}); err != nil {
		t.Fatal(err)
	}
	var got reactionPayload
	if err := json.Unmarshal([]byte(st.signatures[prURL]), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Attempts) != 0 || len(got.Handoffs) != 0 {
		t.Fatalf("recovery left stale idle episode state: attempts=%+v handoffs=%+v", got.Attempts, got.Handoffs)
	}
}

func TestPRObservation_NudgesSuppressedWhileBlocked(t *testing.T) {
	// A blocked session must not receive automated CI/review nudges: injected
	// text could interact with the pending permission dialog.
	m, st, msg := newManager()
	rec := working("mer-1")
	rec.Activity.State = domain.ActivityBlocked
	st.sessions["mer-1"] = rec
	o := ports.PRObservation{Fetched: true, URL: "pr1", CI: domain.CIFailing, Checks: []ports.PRCheckObservation{{Name: "build", CommitHash: "c1", Status: domain.PRCheckFailed, LogTail: "boom"}}}
	if err := m.ApplyPRObservation(ctx, "mer-1", o); err != nil {
		t.Fatal(err)
	}
	if len(msg.msgs) != 0 {
		t.Fatalf("blocked session got nudged: %v", msg.msgs)
	}
}

func TestPRObservation_NudgesSuppressedWhileRateLimited(t *testing.T) {
	m, st, msg := newManager()
	rec := working("mer-1")
	rec.Activity.State = domain.ActivityRateLimited
	st.sessions[rec.ID] = rec
	o := ports.PRObservation{Fetched: true, URL: "pr1", CI: domain.CIFailing, Checks: []ports.PRCheckObservation{{Name: "build", CommitHash: "c1", Status: domain.PRCheckFailed, LogTail: "boom"}}}

	if err := m.ApplyPRObservation(ctx, rec.ID, o); err != nil {
		t.Fatal(err)
	}
	if len(msg.msgs) != 0 {
		t.Fatalf("rate-limited session got an automated nudge: %v", msg.msgs)
	}
}

func TestActivity_TerminatedSessionDoesNotEmitNotification(t *testing.T) {
	st := newFakeStore()
	sink := &fakeNotificationSink{}
	m := New(st, nil, WithNotificationSink(sink))
	st.sessions["mer-1"] = domain.SessionRecord{ID: "mer-1", ProjectID: "mer", IsTerminated: true, Activity: domain.Activity{State: domain.ActivityExited}}

	if err := m.ApplyActivitySignal(ctx, "mer-1", ports.ActivitySignal{Valid: true, State: domain.ActivityWaitingInput}); err != nil {
		t.Fatal(err)
	}
	if len(sink.intents) != 0 {
		t.Fatalf("terminated session emitted %+v", sink.intents)
	}
}

func TestSCMObservation_Notifications(t *testing.T) {
	for _, tc := range []struct {
		name string
		obs  ports.SCMObservation
		want domain.NotificationType
	}{
		{
			name: "ready",
			obs: ports.SCMObservation{
				Fetched: true,
				PR:      ports.SCMPRObservation{URL: "https://github.com/o/r/pull/1", Number: 1, Title: "checkout", HeadSHA: "head-1"},
				CI:      ports.SCMCIObservation{Summary: string(domain.CIPassing), HeadSHA: "head-1"},
				Review: ports.SCMReviewObservation{
					Decision: string(domain.ReviewApproved),
					HeadSHA:  "head-1",
					Reviews: []ports.SCMReviewSummaryObservation{{
						Author: "alice", State: string(domain.ReviewApproved), CommitSHA: "head-1",
					}},
				},
				Mergeability: ports.SCMMergeabilityObservation{State: string(domain.MergeMergeable)},
			},
			want: domain.NotificationReadyToMerge,
		},
		{
			name: "merged",
			obs:  ports.SCMObservation{Fetched: true, PR: ports.SCMPRObservation{URL: "https://github.com/o/r/pull/2", Number: 2, Merged: true}},
			want: domain.NotificationPRMerged,
		},
		{
			name: "closed",
			obs:  ports.SCMObservation{Fetched: true, PR: ports.SCMPRObservation{URL: "https://github.com/o/r/pull/3", Number: 3, Closed: true}},
			want: domain.NotificationPRClosedUnmerged,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newFakeStore()
			sink := &fakeNotificationSink{}
			m := New(st, nil, WithNotificationSink(sink))
			st.sessions["mer-1"] = working("mer-1")
			if err := m.ApplySCMObservation(ctx, "mer-1", tc.obs); err != nil {
				t.Fatal(err)
			}
			if len(sink.intents) != 1 {
				t.Fatalf("intents = %d, want 1", len(sink.intents))
			}
			if got := sink.intents[0]; got.Type != tc.want || got.PRURL != tc.obs.PR.URL || got.PRNumber != tc.obs.PR.Number {
				t.Fatalf("intent = %+v, want type %s", got, tc.want)
			}
		})
	}
}

func TestSCMObservation_ReviewDefinitionOfDone(t *testing.T) {
	base := ports.SCMObservation{
		Fetched: true,
		PR:      ports.SCMPRObservation{URL: "https://github.com/o/r/pull/1", Number: 1, HeadSHA: "head-2"},
		CI:      ports.SCMCIObservation{Summary: string(domain.CIPassing), HeadSHA: "head-2"},
		Review: ports.SCMReviewObservation{
			Decision: string(domain.ReviewApproved),
			HeadSHA:  "head-2",
			Reviews: []ports.SCMReviewSummaryObservation{{
				Author: "alice", State: string(domain.ReviewApproved), CommitSHA: "head-2",
			}},
		},
		Mergeability: ports.SCMMergeabilityObservation{State: string(domain.MergeMergeable)},
	}

	tests := []struct {
		name   string
		mutate func(*ports.SCMObservation)
		ready  bool
	}{
		{name: "complete current-head approval", ready: true},
		{name: "no approval required and no P1 findings", mutate: func(o *ports.SCMObservation) {
			o.Review.Decision = string(domain.ReviewNone)
			o.Review.Reviews = nil
		}, ready: true},
		{name: "stale approval", mutate: func(o *ports.SCMObservation) {
			o.Review.Reviews[0].CommitSHA = "head-1"
		}},
		{name: "review snapshot for stale head", mutate: func(o *ports.SCMObservation) {
			o.Review.HeadSHA = "head-1"
		}},
		{name: "CI snapshot for stale head", mutate: func(o *ports.SCMObservation) {
			o.CI.HeadSHA = "head-1"
		}},
		{name: "partial review window", mutate: func(o *ports.SCMObservation) {
			o.Review.Partial = true
		}},
		{name: "unresolved human thread", mutate: func(o *ports.SCMObservation) {
			o.Review.Threads = []ports.SCMReviewThreadObservation{{ID: "human", Comments: []ports.SCMReviewCommentObservation{{Author: "alice", Body: "please fix this"}}}}
		}},
		{name: "unresolved Codex P1 thread", mutate: func(o *ports.SCMObservation) {
			o.Review.Threads = []ports.SCMReviewThreadObservation{{ID: "codex-p1", IsBot: true, Comments: []ports.SCMReviewCommentObservation{{Author: "chatgpt-codex-connector[bot]", IsBot: true, Body: "[P1] Lost update can corrupt state"}}}}
		}},
		{name: "unresolved Codex P2 is non-blocking", mutate: func(o *ports.SCMObservation) {
			o.Review.Threads = []ports.SCMReviewThreadObservation{{ID: "codex-p2", IsBot: true, Comments: []ports.SCMReviewCommentObservation{{Author: "chatgpt-codex-connector[bot]", IsBot: true, Body: "[P2] Consider a clearer name"}}}}
		}, ready: true},
		{name: "unresolved unrelated bot P1 is non-blocking", mutate: func(o *ports.SCMObservation) {
			o.Review.Threads = []ports.SCMReviewThreadObservation{{ID: "other-p1", IsBot: true, Comments: []ports.SCMReviewCommentObservation{{Author: "other-reviewer[bot]", IsBot: true, Body: "[P1] Maybe change this"}}}}
		}, ready: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			obs := base
			obs.Review.Reviews = append([]ports.SCMReviewSummaryObservation(nil), base.Review.Reviews...)
			if tc.mutate != nil {
				tc.mutate(&obs)
			}
			if got := scmObservationIsReadyToMerge(obs); got != tc.ready {
				t.Fatalf("scmObservationIsReadyToMerge() = %v, want %v", got, tc.ready)
			}
		})
	}
}

func TestSCMObservation_NotReadyWhenCIOrReviewBlocks(t *testing.T) {
	for _, obs := range []ports.SCMObservation{
		{Fetched: true, PR: ports.SCMPRObservation{URL: "https://github.com/o/r/pull/1", Number: 1}, CI: ports.SCMCIObservation{Summary: string(domain.CIFailing)}, Mergeability: ports.SCMMergeabilityObservation{State: string(domain.MergeMergeable)}},
		{Fetched: true, PR: ports.SCMPRObservation{URL: "https://github.com/o/r/pull/1", Number: 1}, CI: ports.SCMCIObservation{Summary: string(domain.CIPending)}, Mergeability: ports.SCMMergeabilityObservation{State: string(domain.MergeMergeable)}},
		{Fetched: true, PR: ports.SCMPRObservation{URL: "https://github.com/o/r/pull/1", Number: 1}, CI: ports.SCMCIObservation{Summary: string(domain.CIUnknown)}, Mergeability: ports.SCMMergeabilityObservation{State: string(domain.MergeMergeable)}},
		{Fetched: true, PR: ports.SCMPRObservation{URL: "https://github.com/o/r/pull/1", Number: 1}, Mergeability: ports.SCMMergeabilityObservation{State: string(domain.MergeMergeable)}},
		{Fetched: true, PR: ports.SCMPRObservation{URL: "https://github.com/o/r/pull/1", Number: 1}, CI: ports.SCMCIObservation{Summary: string(domain.CIPassing)}, Review: ports.SCMReviewObservation{Decision: string(domain.ReviewChangesRequest)}, Mergeability: ports.SCMMergeabilityObservation{State: string(domain.MergeMergeable)}},
	} {
		st := newFakeStore()
		sink := &fakeNotificationSink{}
		m := New(st, nil, WithNotificationSink(sink))
		st.sessions["mer-1"] = working("mer-1")
		if err := m.ApplySCMObservation(ctx, "mer-1", obs); err != nil {
			t.Fatal(err)
		}
		if len(sink.intents) != 0 {
			t.Fatalf("blocked PR emitted %+v", sink.intents)
		}
	}
}

func TestSCMObservation_ReadyToMergeSuppressedWhileWaitingInput(t *testing.T) {
	st := newFakeStore()
	sink := &fakeNotificationSink{}
	m := New(st, nil, WithNotificationSink(sink))
	rec := working("mer-1")
	rec.Activity.State = domain.ActivityWaitingInput
	st.sessions["mer-1"] = rec
	obs := ports.SCMObservation{
		Fetched:      true,
		PR:           ports.SCMPRObservation{URL: "https://github.com/o/r/pull/1", Number: 1},
		CI:           ports.SCMCIObservation{Summary: string(domain.CIPassing)},
		Mergeability: ports.SCMMergeabilityObservation{State: string(domain.MergeMergeable)},
	}
	if err := m.ApplySCMObservation(ctx, "mer-1", obs); err != nil {
		t.Fatal(err)
	}
	if len(sink.intents) != 0 {
		t.Fatalf("waiting-input session emitted ready notification: %+v", sink.intents)
	}
}
