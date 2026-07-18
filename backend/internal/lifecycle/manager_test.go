package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

var ctx = context.Background()

type fakeStore struct {
	sessions   map[domain.SessionID]domain.SessionRecord
	prs        map[domain.SessionID][]domain.PullRequest
	signatures map[string]string

	signatureWriteErr error
	signatureWrites   int
}

func newFakeStore() *fakeStore {
	return &fakeStore{sessions: map[domain.SessionID]domain.SessionRecord{}, prs: map[domain.SessionID][]domain.PullRequest{}, signatures: map[string]string{}}
}

func (f *fakeStore) GetSession(_ context.Context, id domain.SessionID) (domain.SessionRecord, bool, error) {
	r, ok := f.sessions[id]
	return r, ok, nil
}

func (f *fakeStore) ListPRsBySession(_ context.Context, id domain.SessionID) ([]domain.PullRequest, error) {
	return f.prs[id], nil
}

func (f *fakeStore) UpdateSession(_ context.Context, rec domain.SessionRecord) error {
	f.sessions[rec.ID] = rec
	return nil
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

type fakeMessenger struct {
	msgs []string
	err  error
}

type retryMergedCleaner struct {
	lcm   *Manager
	err   error
	calls int
}

func (c *retryMergedCleaner) CleanupMergedSession(ctx context.Context, id domain.SessionID) error {
	c.calls++
	if c.err != nil {
		return c.err
	}
	return c.lcm.MarkTerminated(ctx, id)
}

type telemetrySink struct {
	events []ports.TelemetryEvent
}

func (s *telemetrySink) Emit(_ context.Context, ev ports.TelemetryEvent) {
	s.events = append(s.events, ev)
}

func (*telemetrySink) Close(context.Context) error { return nil }

func (f *fakeMessenger) Send(_ context.Context, _ domain.SessionID, msg string) error {
	if f.err != nil {
		return f.err
	}
	f.msgs = append(f.msgs, msg)
	return nil
}

func newManager() (*Manager, *fakeStore, *fakeMessenger) {
	st := newFakeStore()
	msg := &fakeMessenger{}
	return New(st, msg), st, msg
}

func working(id domain.SessionID) domain.SessionRecord {
	return domain.SessionRecord{ID: id, ProjectID: "mer", Activity: domain.Activity{State: domain.ActivityActive, LastActivityAt: time.Now()}}
}

func TestRuntimeObservation_InferredDeathSetsTerminated(t *testing.T) {
	m, st, _ := newManager()
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
	cleaner := &retryMergedCleaner{lcm: m}
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
	cleaner := &retryMergedCleaner{lcm: m}
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
	o := ports.PRObservation{Fetched: true, URL: "pr1", CI: domain.CIFailing, Checks: []ports.PRCheckObservation{
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

func TestReviewCommentsSignatureUsesStableIDs(t *testing.T) {
	original := []ports.PRCommentObservation{
		{ID: "c1", ThreadID: "t1", Author: "alice", File: "old.go", Line: 10, Body: "old", URL: "https://old"},
		{ID: "c2", ThreadID: "t2", Author: "bob", File: "old.go", Line: 20, Body: "old", URL: "https://old"},
	}
	editedAndReordered := []ports.PRCommentObservation{
		{ID: "c2", ThreadID: "t2", Author: "bob", File: "new.go", Line: 99, Body: "edited", URL: "https://new"},
		{ID: "c1", ThreadID: "t1", Author: "alice", File: "new.go", Line: 42, Body: "edited", URL: "https://new"},
	}
	if got, want := reviewCommentsSignature(editedAndReordered), reviewCommentsSignature(original); got != want {
		t.Fatalf("signature changed after edit/reorder\n got %q\nwant %q", got, want)
	}

	withNewComment := append([]ports.PRCommentObservation(nil), original...)
	withNewComment = append(withNewComment, ports.PRCommentObservation{ID: "c3", ThreadID: "t2", Body: "new comment in same thread"})
	if got, old := reviewCommentsSignature(withNewComment), reviewCommentsSignature(original); got == old {
		t.Fatalf("new comment id should change signature, got %q", got)
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
	o := ports.PRObservation{Fetched: true, URL: "pr1", Review: domain.ReviewChangesRequest, Comments: []ports.PRCommentObservation{
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
	o := ports.PRObservation{Fetched: true, URL: "pr1", CI: domain.CIFailing, Checks: []ports.PRCheckObservation{{Name: "build", CommitHash: "c1", Status: domain.PRCheckFailed, LogTail: "line1\x1b[2Jline2\x00\ttabbed"}}}
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
	o := ports.PRObservation{Fetched: true, URL: "pr1", Review: domain.ReviewChangesRequest, Comments: []ports.PRCommentObservation{{ID: "1", Body: "please\x1b]0;pwned\afix this"}}}
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
		PR:      ports.SCMPRObservation{URL: "pr1", Number: 1},
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
	o := ports.PRObservation{Fetched: true, URL: "pr1", Mergeability: domain.MergeConflicting}
	if err := m.ApplyPRObservation(ctx, "mer-1", o); err != nil {
		t.Fatal(err)
	}
	if len(msg.msgs) != 1 || !strings.Contains(msg.msgs[0], "merge conflicts") {
		t.Fatalf("want merge-conflict nudge, got %v", msg.msgs)
	}
}

func TestPRObservation_NudgeIncludesPRIdentity(t *testing.T) {
	m, st, msg := newManager()
	st.sessions["mer-1"] = working("mer-1")
	o := ports.PRObservation{
		Fetched:      true,
		URL:          "https://github.com/o/r/pull/7",
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

func TestPRObservation_MergedCleanupFailureRetriesFromDurableLatch(t *testing.T) {
	m, st, _ := newManager()
	st.sessions["mer-1"] = working("mer-1")
	st.prs["mer-1"] = []domain.PullRequest{{URL: "pr1", Merged: true}}
	cleaner := &retryMergedCleaner{lcm: m, err: errors.New("runtime busy")}
	m.SetMergedSessionCleaner(cleaner)

	err := m.ApplyPRObservation(ctx, "mer-1", ports.PRObservation{Fetched: true, URL: "pr1", Merged: true})
	if err == nil || !strings.Contains(err.Error(), "runtime busy") {
		t.Fatalf("ApplyPRObservation error = %v, want runtime busy", err)
	}
	failed := st.sessions["mer-1"]
	if failed.IsTerminated || !failed.Metadata.MergedCleanupPending {
		t.Fatalf("failed cleanup must stay live and durably pending: %+v", failed)
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
			cleaner := &retryMergedCleaner{lcm: m, err: errors.New("workspace busy")}
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
	o := ports.PRObservation{Fetched: true, URL: "child", Mergeability: domain.MergeConflicting}
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
	o := ports.PRObservation{Fetched: true, URL: "parent", Mergeability: domain.MergeConflicting}
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

func TestPRObservation_DedupPersistsAcrossPRs(t *testing.T) {
	st := newFakeStore()
	st.sessions["mer-1"] = working("mer-1")
	msg := &fakeMessenger{}
	m := New(st, msg)

	for _, url := range []string{"https://github.com/o/r/pull/1", "https://github.com/o/r/pull/2"} {
		o := ports.PRObservation{
			Fetched: true, URL: url, CI: domain.CIFailing,
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
	o := ports.PRObservation{Fetched: true, URL: "pr1", Mergeability: domain.MergeConflicting}
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
		PR:       ports.SCMPRObservation{URL: "https://github.com/o/r/pull/1", Number: 1, Title: "fix pending input"},
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
