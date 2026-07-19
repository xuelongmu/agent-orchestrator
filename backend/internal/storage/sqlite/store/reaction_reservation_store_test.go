package store_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

func seedReactionPR(t *testing.T, s *sqlite.Store) (string, domain.SessionRecord) {
	t.Helper()
	ctx := context.Background()
	seedProject(t, s, "mer")
	session, err := s.CreateSession(ctx, sampleRecord("mer"))
	if err != nil {
		t.Fatal(err)
	}
	prURL := "https://github.com/o/r/pull/28"
	if err := s.WriteSCMObservation(ctx, domain.PullRequest{URL: prURL, SessionID: session.ID, Number: 28, HeadSHA: "head-a", UpdatedAt: time.Now().UTC()}, nil, nil, nil, nil, ports.ReviewWritePreserve); err != nil {
		t.Fatal(err)
	}
	return prURL, session
}

func TestLegacyWritePRPersistsExactHeadForReactionFence(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	session, err := s.CreateSession(ctx, sampleRecord("mer"))
	if err != nil {
		t.Fatal(err)
	}
	prURL := "https://github.com/o/r/pull/legacy"
	if err := s.WritePR(ctx, domain.PullRequest{URL: prURL, SessionID: session.ID, Number: 7, HeadSHA: "legacy-head", UpdatedAt: time.Now().UTC()}, nil, nil); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.GetPR(ctx, prURL)
	if err != nil || !ok || got.HeadSHA != "legacy-head" {
		t.Fatalf("legacy PR = %+v, ok=%v err=%v", got, ok, err)
	}
}

func reactionPayloadAt(t *testing.T, s *sqlite.Store, prURL string) struct {
	Seen     map[string]string `json:"seen"`
	Attempts map[string]int    `json:"attempts"`
	Handoffs map[string]any    `json:"handoffs"`
} {
	t.Helper()
	raw, err := s.GetPRLastNudgeSignature(context.Background(), prURL)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Seen     map[string]string `json:"seen"`
		Attempts map[string]int    `json:"attempts"`
		Handoffs map[string]any    `json:"handoffs"`
	}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &payload); err != nil {
			t.Fatalf("decode reaction payload %q: %v", raw, err)
		}
	}
	return payload
}

func reserveReaction(t *testing.T, s *sqlite.Store, prURL, key, sig, owner string, sessionID domain.SessionID, head string, now time.Time) ports.PRReactionReservation {
	t.Helper()
	fences := []ports.PRReactionFence{{PRURL: prURL, SessionID: sessionID, HeadSHA: head}}
	got, err := s.ReservePRReaction(context.Background(), prURL, key, sig, 3, owner, fences, now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestPRReactionStartPersistsBeforePaneBoundaryAndCrashIsUncertain(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	first, err := sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	prURL, session := seedReactionPR(t, first)
	key := "review:" + prURL
	now := time.Now().UTC()
	reserved := reserveReaction(t, first, prURL, key, "comments-a", "owner-a", session.ID, "head-a", now)
	if reserved.Status != ports.PRReactionReserved || reserved.Attempts != 1 {
		t.Fatalf("reserve = %+v", reserved)
	}
	if got := reactionPayloadAt(t, first, prURL); len(got.Seen) != 0 || len(got.Attempts) != 0 {
		t.Fatalf("reserve consumed budget before send boundary: %+v", got)
	}
	started, err := first.StartPRReaction(ctx, prURL, key, "owner-a", now, now.Add(time.Minute))
	if err != nil || started.Status != ports.PRReactionReserved {
		t.Fatalf("start = %+v, err=%v", started, err)
	}
	if got := reactionPayloadAt(t, first, prURL); got.Seen[key] != "comments-a" || got.Attempts[key] != 1 {
		t.Fatalf("pre-pane payload = %+v", got)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	// Whether the process crashed immediately before the pane call or after the
	// pane call but before Commit is unknowable. Both fail closed: no re-send.
	duplicate := reserveReaction(t, reopened, prURL, key, "comments-a", "owner-b", session.ID, "head-a", now.Add(2*time.Minute))
	if duplicate.Status != ports.PRReactionUncertain || duplicate.Attempts != 1 {
		t.Fatalf("unknown delivery = %+v, want uncertain", duplicate)
	}
}

func TestPRReactionCrashBeforeStartCanBeTakenOverAfterLease(t *testing.T) {
	s := newTestStore(t)
	prURL, session := seedReactionPR(t, s)
	key := "ci:" + prURL
	now := time.Now().UTC()
	fences := []ports.PRReactionFence{{PRURL: prURL, SessionID: session.ID, HeadSHA: "head-a"}}
	first, err := s.ReservePRReaction(context.Background(), prURL, key, "head-a", 3, "owner-a", fences, now, now.Add(time.Second))
	if err != nil || first.Status != ports.PRReactionReserved {
		t.Fatalf("first reserve = %+v, err=%v", first, err)
	}
	live, err := s.ReservePRReaction(context.Background(), prURL, key, "head-a", 3, "owner-b", fences, now.Add(time.Second/2), now.Add(time.Minute))
	if err != nil || live.Status != ports.PRReactionBusy {
		t.Fatalf("live contender = %+v, err=%v", live, err)
	}
	takeover, err := s.ReservePRReaction(context.Background(), prURL, key, "head-a", 3, "owner-b", fences, now.Add(2*time.Second), now.Add(time.Minute))
	if err != nil || takeover.Status != ports.PRReactionReserved {
		t.Fatalf("expired takeover = %+v, err=%v", takeover, err)
	}
	if released, err := s.ReleasePRReaction(context.Background(), prURL, key, "owner-a"); err != nil || released {
		t.Fatalf("stale owner rollback = %v, err=%v", released, err)
	}
}

func TestPRReactionConcurrentInFlightHasOneSenderAndFailedSendCanRetry(t *testing.T) {
	dir := t.TempDir()
	first, err := sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	prURL, session := seedReactionPR(t, first)
	second, err := sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	key := "review:" + prURL
	now := time.Now().UTC()

	type outcome struct {
		owner  string
		status ports.PRReactionReservationStatus
		err    error
	}
	start := make(chan struct{})
	results := make(chan outcome, 2)
	var wg sync.WaitGroup
	for _, candidate := range []struct {
		store *sqlite.Store
		owner string
	}{{first, "owner-a"}, {second, "owner-b"}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			fences := []ports.PRReactionFence{{PRURL: prURL, SessionID: session.ID, HeadSHA: "head-a"}}
			got, err := candidate.store.ReservePRReaction(context.Background(), prURL, key, "comments-a", 3, candidate.owner, fences, now, now.Add(time.Minute))
			results <- outcome{owner: candidate.owner, status: got.Status, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	winner := ""
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.status == ports.PRReactionReserved {
			if winner != "" {
				t.Fatalf("multiple winners: %s and %s", winner, result.owner)
			}
			winner = result.owner
		} else if result.status != ports.PRReactionBusy {
			t.Fatalf("loser status = %q, want busy", result.status)
		}
	}
	if winner == "" {
		t.Fatal("no reservation winner")
	}
	started, err := first.StartPRReaction(context.Background(), prURL, key, winner, now, now.Add(time.Minute))
	if err != nil || started.Status != ports.PRReactionReserved {
		t.Fatalf("winner start = %+v, err=%v", started, err)
	}
	contender := reserveReaction(t, second, prURL, key, "comments-a", "owner-c", session.ID, "head-a", now.Add(2*time.Minute))
	if contender.Status != ports.PRReactionUncertain {
		t.Fatalf("in-flight contender = %+v, want uncertain", contender)
	}
	// A confirmed no-write/error releases and restores the budget.
	if released, err := first.ReleasePRReaction(context.Background(), prURL, key, winner); err != nil || !released {
		t.Fatalf("failed-send release = %v, err=%v", released, err)
	}
	retry := reserveReaction(t, second, prURL, key, "comments-a", "retry-owner", session.ID, "head-a", now.Add(3*time.Minute))
	if retry.Status != ports.PRReactionReserved || retry.Attempts != 1 {
		t.Fatalf("retry after confirmed no-write = %+v", retry)
	}
}

func TestPRReactionStartFencesHeadAndSessionTransitions(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	prURL, session := seedReactionPR(t, s)
	key := "review:" + prURL
	now := time.Now().UTC()
	reserved := reserveReaction(t, s, prURL, key, "comments-a", "owner-a", session.ID, "head-a", now)
	if reserved.Status != ports.PRReactionReserved {
		t.Fatalf("reserve = %+v", reserved)
	}
	if err := s.WriteSCMObservation(ctx, domain.PullRequest{URL: prURL, SessionID: session.ID, Number: 28, HeadSHA: "head-b", UpdatedAt: now.Add(time.Second)}, nil, nil, nil, nil, ports.ReviewWritePreserve); err != nil {
		t.Fatal(err)
	}
	started, err := s.StartPRReaction(ctx, prURL, key, "owner-a", now.Add(time.Second), now.Add(time.Minute))
	if err != nil || started.Status != ports.PRReactionStale {
		t.Fatalf("start after head transition = %+v, err=%v", started, err)
	}
	newHead := reserveReaction(t, s, prURL, key, "comments-a", "owner-b", session.ID, "head-b", now.Add(2*time.Second))
	if newHead.Status != ports.PRReactionReserved {
		t.Fatalf("new head reserve = %+v", newHead)
	}
	wrongFence := []ports.PRReactionFence{{PRURL: prURL, SessionID: "other-session", HeadSHA: "head-b"}}
	wrongSession, err := s.ReservePRReaction(ctx, prURL, "ci:"+prURL, "head-b", 3, "owner-c", wrongFence, now, now.Add(time.Minute))
	if err != nil || wrongSession.Status != ports.PRReactionStale {
		t.Fatalf("wrong session fence = %+v, err=%v", wrongSession, err)
	}
}

func TestPRReactionReleasePreservesHandoffState(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	prURL, session := seedReactionPR(t, s)
	key := "review:" + prURL
	initial := `{"seen":{"` + key + `":"old"},"attempts":{"` + key + `":2},"handoffs":{"keep":{"outcome":"notified","reason":"test"}}}`
	if err := s.UpdatePRLastNudgeSignature(ctx, prURL, initial); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	reserved := reserveReaction(t, s, prURL, key, "new", "owner-a", session.ID, "head-a", now)
	if reserved.Status != ports.PRReactionReserved {
		t.Fatalf("reserve = %+v", reserved)
	}
	if _, err := s.StartPRReaction(ctx, prURL, key, "owner-a", now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if released, err := s.ReleasePRReaction(ctx, prURL, key, "owner-a"); err != nil || !released {
		t.Fatalf("release = %v, err=%v", released, err)
	}
	got := reactionPayloadAt(t, s, prURL)
	if got.Seen[key] != "old" || got.Attempts[key] != 2 || got.Handoffs["keep"] == nil {
		t.Fatalf("rolled-back payload = %+v", got)
	}
}
