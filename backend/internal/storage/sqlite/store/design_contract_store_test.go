package store_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/designcontract"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	sqlitedb "github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	sqlite "github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

func TestPRDesignContractSurvivesTerminationAndReplacementWithoutWorkspaceState(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	owner := createContractSession(t, s, "mer")
	replacement := createContractSession(t, s, "mer")
	largeInvariant := "Durable knowledge: " + strings.Repeat("x", 32*1024)
	want := designcontract.BuildSeed("61", "## Invariants\n- "+largeInvariant)
	if err := s.SaveSessionDesignContractSeed(ctx, owner.ID, want, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	claimContractPR(t, s, owner.ID, "https://github.com/acme/repo/pull/1", 1)

	// Model normal kill/retirement: the session row remains terminal while its
	// disposable worktree state is absent. Canonical bytes must not depend on it.
	owner.IsTerminated = true
	owner.Metadata.WorkspacePath = ""
	if err := s.UpdateSession(ctx, owner); err != nil {
		t.Fatal(err)
	}
	outcome := claimContractPR(t, s, replacement.ID, "https://github.com/acme/repo/pull/1", 1)
	if outcome.PreviousOwner != owner.ID || outcome.DesignContract != want {
		t.Fatalf("replacement outcome owner=%s contract bytes=%d, want owner=%s bytes=%d", outcome.PreviousOwner, len(outcome.DesignContract), owner.ID, len(want))
	}
	got, ok, err := s.GetPRDesignContract(ctx, "https://github.com/acme/repo/pull/1")
	if err != nil || !ok || got != want {
		t.Fatalf("durable contract after replacement = ok=%v bytes=%d err=%v", ok, len(got), err)
	}
}

func TestOwnedPRDesignContractReadFencesTakeoverAndPreservesInheritance(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	owner := createContractSession(t, s, "mer")
	replacement := createContractSession(t, s, "mer")
	prURL := "https://github.com/acme/repo/pull/19"
	want := designcontract.BuildSeed("61", "## Invariants\n- Ownership is checked with the canonical read.")
	if err := s.SaveSessionDesignContractSeed(ctx, owner.ID, want, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	claimContractPR(t, s, owner.ID, prURL, 19)
	if got, found, err := s.GetOwnedPRDesignContract(ctx, owner.ID, prURL); err != nil || !found || got != want {
		t.Fatalf("owner read = %q found=%v err=%v", got, found, err)
	}
	claimContractPR(t, s, replacement.ID, prURL, 19)
	if _, _, err := s.GetOwnedPRDesignContract(ctx, owner.ID, prURL); !errors.Is(err, designcontract.ErrPRNotOwned) {
		t.Fatalf("predecessor read after takeover error = %v, want ErrPRNotOwned", err)
	}
	if got, found, err := s.GetOwnedPRDesignContract(ctx, replacement.ID, prURL); err != nil || !found || got != want {
		t.Fatalf("replacement inherited read = %q found=%v err=%v", got, found, err)
	}
}

func reviewFixMessage(t *testing.T, pr, mode, invariant string) string {
	t.Helper()
	value, err := json.Marshal(designcontract.ReviewFixInvariantDeclaration{PR: pr, Mode: mode, Invariant: invariant})
	if err != nil {
		t.Fatal(err)
	}
	return "fix: address review\n\n" + designcontract.ReviewFixInvariantTrailer + ": " + string(value)
}

func setupPendingReviewFix(t *testing.T) (*sqlite.Store, domain.SessionRecord, domain.SessionRecord, string, string, string) {
	t.Helper()
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	owner := createContractSession(t, s, "mer")
	replacement := createContractSession(t, s, "mer")
	pr1 := "https://github.com/acme/repo/pull/31"
	pr2 := "https://github.com/acme/repo/pull/32"
	seed := designcontract.BuildSeed("148", "## Invariants\n- Exact provenance is checked atomically.")
	if err := s.SaveSessionDesignContractSeed(ctx, owner.ID, seed, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	for i, pr := range []string{pr1, pr2} {
		if err := s.WriteSCMObservation(ctx, domain.PullRequest{URL: pr, SessionID: owner.ID, Number: 31 + i, HeadSHA: fmt.Sprintf("head-%d", 31+i), UpdatedAt: time.Now().UTC()}, nil, nil, nil, nil, ports.ReviewWritePreserve); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	if err := s.UpsertReview(ctx, domain.Review{ID: "review-31", SessionID: owner.ID, ProjectID: owner.ProjectID, Harness: domain.ReviewerCodex, PRURL: pr1, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertReviewRun(ctx, domain.ReviewRun{ID: "run-31", ReviewID: "review-31", SessionID: owner.ID, Harness: domain.ReviewerCodex, PRURL: pr1, TargetSHA: "reviewed-head", Status: domain.ReviewRunRunning, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	finding := domain.ReviewFinding{ID: "run-31:1", RunID: "run-31", SessionID: owner.ID, PRURL: pr1, Round: 1, ClassTag: "ownership", RootCauseNote: "fix ownership", CreatedAt: now}
	if ok, err := s.CompleteReviewRunWithFindings(ctx, "run-31", domain.VerdictChangesRequested, "[P1] fix ownership", "", "", []domain.ReviewFinding{finding}); err != nil || !ok {
		t.Fatalf("complete review = %v, %v", ok, err)
	}
	return s, owner, replacement, pr1, pr2, seed
}

func TestAcceptReviewFixCommitFencesExactPRHeadOwnerAndUpdatesOnlyTarget(t *testing.T) {
	t.Run("no pending actionable findings bypass declaration", func(t *testing.T) {
		s, owner, _, _, pr2, seed := setupPendingReviewFix(t)
		required, bound, err := s.AcceptReviewFixCommit(context.Background(), owner.ID, pr2, "head-32", "no trailer", false, time.Now().UTC())
		if err != nil || required || bound != 0 {
			t.Fatalf("bypass = required %v bound %d err %v", required, bound, err)
		}
		if got, _, _ := s.GetPRDesignContract(context.Background(), pr2); got != seed {
			t.Fatal("bypass changed sibling contract")
		}
	})

	t.Run("historical blocking run can force declaration without finding rows", func(t *testing.T) {
		s, owner, _, _, pr2, seed := setupPendingReviewFix(t)
		if _, _, err := s.AcceptReviewFixCommit(context.Background(), owner.ID, pr2, "head-32", "missing trailer", true, time.Now().UTC()); !errors.Is(err, designcontract.ErrReviewFixDeclarationMissing) {
			t.Fatalf("forced missing trailer error = %v", err)
		}
		if got, _, _ := s.GetPRDesignContract(context.Background(), pr2); got != seed {
			t.Fatal("forced validation failure changed contract")
		}
		added := "Historical blocking reviews still require invariant declarations."
		message := reviewFixMessage(t, pr2, "add", added)
		required, bound, err := s.AcceptReviewFixCommit(context.Background(), owner.ID, pr2, "head-32", message, true, time.Now().UTC())
		if err != nil || !required || bound != 0 {
			t.Fatalf("forced declaration = required %v bound %d err %v", required, bound, err)
		}
		after, _, _ := s.GetPRDesignContract(context.Background(), pr2)
		if !designcontract.HasExactInvariant(after, added) {
			t.Fatalf("forced add missing: %q", after)
		}
		if _, _, err := s.AcceptReviewFixCommit(context.Background(), owner.ID, pr2, "head-32", message, true, time.Now().UTC()); err != nil {
			t.Fatalf("forced restart replay: %v", err)
		}
		if replay, _, _ := s.GetPRDesignContract(context.Background(), pr2); replay != after {
			t.Fatal("forced restart replay duplicated invariant")
		}
	})

	t.Run("exact preserve binds pending finding", func(t *testing.T) {
		s, owner, _, pr1, _, seed := setupPendingReviewFix(t)
		required, bound, err := s.AcceptReviewFixCommit(context.Background(), owner.ID, pr1, "head-31", reviewFixMessage(t, pr1, "preserve", "Exact provenance is checked atomically."), false, time.Now().UTC())
		if err != nil || !required || bound != 1 {
			t.Fatalf("accept = required %v bound %d err %v", required, bound, err)
		}
		if got, _, _ := s.GetPRDesignContract(context.Background(), pr1); got != seed {
			t.Fatal("preserve changed canonical contract")
		}
		findings, _ := s.ListReviewFindingsByRun(context.Background(), "run-31")
		if len(findings) != 1 || findings[0].FixCommit != "head-31" {
			t.Fatalf("finding binding = %+v", findings)
		}
	})

	t.Run("manual current-head run cannot bypass and its own finding is excluded", func(t *testing.T) {
		s, owner, _, pr1, _, seed := setupPendingReviewFix(t)
		now := time.Now().UTC().Add(time.Second)
		if err := s.InsertReviewRun(context.Background(), domain.ReviewRun{ID: "run-current", ReviewID: "review-31", SessionID: owner.ID, Harness: domain.ReviewerCodex, PRURL: pr1, TargetSHA: "head-31", Status: domain.ReviewRunRunning, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
		current := domain.ReviewFinding{ID: "run-current:1", RunID: "run-current", SessionID: owner.ID, PRURL: pr1, Round: 2, ClassTag: "current", CreatedAt: now}
		if ok, err := s.CompleteReviewRunWithFindings(context.Background(), "run-current", domain.VerdictChangesRequested, "[P1] current finding", "", "", []domain.ReviewFinding{current}); err != nil || !ok {
			t.Fatalf("complete current run = %v, %v", ok, err)
		}
		if _, _, err := s.AcceptReviewFixCommit(context.Background(), owner.ID, pr1, "head-31", "missing trailer", false, now); !errors.Is(err, designcontract.ErrReviewFixDeclarationMissing) {
			t.Fatalf("manual-trigger bypass error = %v", err)
		}
		if got, _, _ := s.GetPRDesignContract(context.Background(), pr1); got != seed {
			t.Fatal("failed manual-trigger gate changed contract")
		}
		required, bound, err := s.AcceptReviewFixCommit(context.Background(), owner.ID, pr1, "head-31", reviewFixMessage(t, pr1, "preserve", "Exact provenance is checked atomically."), false, now)
		if err != nil || !required || bound != 1 {
			t.Fatalf("valid manual-trigger gate = required %v bound %d err %v", required, bound, err)
		}
		oldFindings, _ := s.ListReviewFindingsByRun(context.Background(), "run-31")
		currentFindings, _ := s.ListReviewFindingsByRun(context.Background(), "run-current")
		if oldFindings[0].FixCommit != "head-31" || currentFindings[0].FixCommit != "" {
			t.Fatalf("head-scoped bindings old=%+v current=%+v", oldFindings, currentFindings)
		}
	})

	badCases := []struct {
		name     string
		message  func(*testing.T, string, string) string
		session  func(domain.SessionRecord, domain.SessionRecord) domain.SessionID
		expected string
		want     error
	}{
		{"missing", func(*testing.T, string, string) string { return "fix without trailer" }, func(o, _ domain.SessionRecord) domain.SessionID { return o.ID }, "head-31", designcontract.ErrReviewFixDeclarationMissing},
		{"malformed", func(*testing.T, string, string) string { return "x\n\nAO-Review-Fix-Invariant: {}" }, func(o, _ domain.SessionRecord) domain.SessionID { return o.ID }, "head-31", designcontract.ErrReviewFixDeclarationMalformed},
		{"normalized identity is exact", func(t *testing.T, pr, _ string) string {
			return reviewFixMessage(t, pr+"/", "preserve", "Exact provenance is checked atomically.")
		}, func(o, _ domain.SessionRecord) domain.SessionID { return o.ID }, "head-31", designcontract.ErrReviewFixDeclarationStale},
		{"sibling declaration", func(t *testing.T, _, sibling string) string {
			return reviewFixMessage(t, sibling, "preserve", "Exact provenance is checked atomically.")
		}, func(o, _ domain.SessionRecord) domain.SessionID { return o.ID }, "head-31", designcontract.ErrReviewFixDeclarationStale},
		{"stale observed head", func(t *testing.T, pr, _ string) string {
			return reviewFixMessage(t, pr, "preserve", "Exact provenance is checked atomically.")
		}, func(o, _ domain.SessionRecord) domain.SessionID { return o.ID }, "old-head", designcontract.ErrReviewFixDeclarationStale},
		{"near invariant", func(t *testing.T, pr, _ string) string {
			return reviewFixMessage(t, pr, "preserve", "exact provenance is checked atomically.")
		}, func(o, _ domain.SessionRecord) domain.SessionID { return o.ID }, "head-31", designcontract.ErrReviewFixInvariantUnknown},
		{"unowned PR", func(t *testing.T, pr, _ string) string {
			return reviewFixMessage(t, pr, "preserve", "Exact provenance is checked atomically.")
		}, func(_, r domain.SessionRecord) domain.SessionID { return r.ID }, "head-31", designcontract.ErrPRNotOwned},
	}
	for _, tc := range badCases {
		t.Run(tc.name, func(t *testing.T) {
			s, owner, replacement, pr1, pr2, seed := setupPendingReviewFix(t)
			_, _, err := s.AcceptReviewFixCommit(context.Background(), tc.session(owner, replacement), pr1, tc.expected, tc.message(t, pr1, pr2), false, time.Now().UTC())
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if got, _, _ := s.GetPRDesignContract(context.Background(), pr1); got != seed {
				t.Fatal("failed declaration changed target contract")
			}
			findings, _ := s.ListReviewFindingsByRun(context.Background(), "run-31")
			if len(findings) != 1 || findings[0].FixCommit != "" {
				t.Fatalf("failed declaration bound finding: %+v", findings)
			}
		})
	}

	t.Run("add is target scoped and restart replay is idempotent", func(t *testing.T) {
		s, owner, _, pr1, pr2, siblingSeed := setupPendingReviewFix(t)
		added := "Each review fix binds its declaration to one observed head."
		message := reviewFixMessage(t, pr1, "add", added)
		required, bound, err := s.AcceptReviewFixCommit(context.Background(), owner.ID, pr1, "head-31", message, false, time.Now().UTC())
		if err != nil || !required || bound != 1 {
			t.Fatalf("add = required %v bound %d err %v", required, bound, err)
		}
		after, _, _ := s.GetPRDesignContract(context.Background(), pr1)
		if !designcontract.HasExactInvariant(after, added) {
			t.Fatalf("added invariant missing: %q", after)
		}
		if sibling, _, _ := s.GetPRDesignContract(context.Background(), pr2); sibling != siblingSeed {
			t.Fatal("add leaked to sibling PR")
		}
		required, bound, err = s.AcceptReviewFixCommit(context.Background(), owner.ID, pr1, "head-31", message, false, time.Now().UTC())
		if err != nil || required || bound != 0 {
			t.Fatalf("restart replay = required %v bound %d err %v", required, bound, err)
		}
		if replay, _, _ := s.GetPRDesignContract(context.Background(), pr1); replay != after {
			t.Fatal("restart replay duplicated invariant")
		}
	})

	t.Run("replacement is the only accepted owner", func(t *testing.T) {
		s, owner, replacement, pr1, _, _ := setupPendingReviewFix(t)
		if _, err := s.ClaimPR(context.Background(), domain.PullRequest{URL: pr1, SessionID: replacement.ID, Number: 31, HeadSHA: "head-31", UpdatedAt: time.Now().UTC()}, nil, nil, nil, nil, ports.ReviewWritePreserve, true, "", ""); err != nil {
			t.Fatal(err)
		}
		message := reviewFixMessage(t, pr1, "preserve", "Exact provenance is checked atomically.")
		if _, _, err := s.AcceptReviewFixCommit(context.Background(), owner.ID, pr1, "head-31", message, false, time.Now().UTC()); !errors.Is(err, designcontract.ErrPRNotOwned) {
			t.Fatalf("predecessor error = %v", err)
		}
		if required, bound, err := s.AcceptReviewFixCommit(context.Background(), replacement.ID, pr1, "head-31", message, false, time.Now().UTC()); err != nil || !required || bound != 1 {
			t.Fatalf("replacement = required %v bound %d err %v", required, bound, err)
		}
	})
}

func TestAcceptReviewFixCommitRollsBackContractWhenFindingBindingFails(t *testing.T) {
	dir := t.TempDir()
	s, err := sqlitedb.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	seedProject(t, s, "mer")
	owner := createContractSession(t, s, "mer")
	prURL := "https://github.com/acme/repo/pull/41"
	seed := designcontract.BuildSeed("148", "## Invariants\n- Existing invariant.")
	if err := s.SaveSessionDesignContractSeed(ctx, owner.ID, seed, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteSCMObservation(ctx, domain.PullRequest{URL: prURL, SessionID: owner.ID, Number: 41, HeadSHA: "head-41", UpdatedAt: time.Now().UTC()}, nil, nil, nil, nil, ports.ReviewWritePreserve); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := s.UpsertReview(ctx, domain.Review{ID: "review-41", SessionID: owner.ID, ProjectID: owner.ProjectID, Harness: domain.ReviewerCodex, PRURL: prURL, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertReviewRun(ctx, domain.ReviewRun{ID: "run-41", ReviewID: "review-41", SessionID: owner.ID, Harness: domain.ReviewerCodex, PRURL: prURL, TargetSHA: "reviewed", Status: domain.ReviewRunRunning, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	finding := domain.ReviewFinding{ID: "run-41:1", RunID: "run-41", SessionID: owner.ID, PRURL: prURL, Round: 1, ClassTag: "atomicity", CreatedAt: now}
	if ok, err := s.CompleteReviewRunWithFindings(ctx, "run-41", domain.VerdictChangesRequested, "[P1] fix", "", "", []domain.ReviewFinding{finding}); err != nil || !ok {
		t.Fatalf("complete review = %v, %v", ok, err)
	}

	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "ao.db")+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	if _, err := raw.ExecContext(ctx, `CREATE TRIGGER reject_fix_binding BEFORE UPDATE OF fix_commit ON review_finding WHEN NEW.fix_commit != '' BEGIN SELECT RAISE(ABORT, 'binding rejected'); END;`); err != nil {
		t.Fatal(err)
	}
	added := "The contract append and finding binding commit together."
	if _, _, err := s.AcceptReviewFixCommit(ctx, owner.ID, prURL, "head-41", reviewFixMessage(t, prURL, "add", added), false, time.Now().UTC()); err == nil {
		t.Fatal("binding failure unexpectedly accepted")
	}
	contract, _, _ := s.GetPRDesignContract(ctx, prURL)
	if contract != seed || strings.Contains(contract, added) {
		t.Fatalf("binding failure committed contract mutation: %q", contract)
	}
	findings, _ := s.ListReviewFindingsByRun(ctx, "run-41")
	if len(findings) != 1 || findings[0].FixCommit != "" {
		t.Fatalf("binding failure changed finding: %+v", findings)
	}
}

func TestAddPRDesignContractInvariantReturnsStableOwnershipAndCapacityErrors(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	owner := createContractSession(t, s, "mer")
	other := createContractSession(t, s, "mer")
	prURL := "https://github.com/acme/repo/pull/21"
	seed := strings.Repeat("x", designcontract.MaxCanonicalBytes-80)
	if err := s.SaveSessionDesignContractSeed(ctx, owner.ID, seed, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	claimContractPR(t, s, owner.ID, prURL, 21)
	if _, err := s.AddPRDesignContractInvariant(ctx, other.ID, prURL, "Every append rechecks exact ownership.", time.Now().UTC()); !errors.Is(err, designcontract.ErrPRNotOwned) {
		t.Fatalf("ownership mismatch error = %v, want ErrPRNotOwned", err)
	}
	if _, err := s.AddPRDesignContractInvariant(ctx, owner.ID, prURL, strings.Repeat("i", 128), time.Now().UTC()); !errors.Is(err, designcontract.ErrContractCapacityExceeded) {
		t.Fatalf("capacity error = %v, want ErrContractCapacityExceeded", err)
	}
}

func TestPRDesignContractCanonicalCapCountsUTF8Bytes(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	owner := createContractSession(t, s, "mer")
	// Fewer than one million Unicode characters, but more than one MiB once
	// encoded as UTF-8. SQLite length(TEXT) would incorrectly admit this.
	tooLarge := strings.Repeat("é", designcontract.MaxCanonicalBytes/2+1)
	if len([]rune(tooLarge)) >= designcontract.MaxCanonicalBytes || len(tooLarge) <= designcontract.MaxCanonicalBytes {
		t.Fatal("invalid multibyte test fixture")
	}
	if err := s.SaveSessionDesignContractSeed(ctx, owner.ID, tooLarge, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	_, err := s.ClaimPR(ctx, domain.PullRequest{URL: "https://github.com/acme/repo/pull/20", SessionID: owner.ID, Number: 20, UpdatedAt: time.Now().UTC()}, nil, nil, nil, nil, ports.ReviewWritePreserve, true, "", "")
	if err == nil {
		t.Fatalf("multibyte contract over byte cap error = %v", err)
	}
}

func TestClaimPRPersistsExactSessionContractDeliveryBarrier(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	owner := createContractSession(t, s, "mer")
	other := createContractSession(t, s, "mer")
	prURL := "https://gitlab.example.com/group/repo/-/merge_requests/17"
	taskPrompt := "Fix the exact claimed merge request."
	outcome, err := s.ClaimPR(ctx, domain.PullRequest{URL: prURL, SessionID: owner.ID, Number: 17, UpdatedAt: time.Now().UTC()}, nil, nil, nil, nil, ports.ReviewWritePreserve, true, "", taskPrompt)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.ContractDeliveryPending {
		t.Fatal("ownership transaction omitted delivery barrier")
	}
	delivery, pending, err := s.GetPendingPRDesignContractDelivery(ctx, owner.ID, prURL)
	if err != nil || !pending || !strings.Contains(delivery.Contract, "Trust boundary") || delivery.TaskPrompt != taskPrompt || delivery.Token == "" || delivery.Token != outcome.ContractDeliveryToken {
		t.Fatalf("pending delivery = pending %v contract %q err %v", pending, delivery.Contract, err)
	}
	if completed, err := s.CompletePRDesignContractDelivery(ctx, other.ID, prURL, delivery.Token, delivery.Revision); err != nil || completed {
		t.Fatalf("sibling session cleared barrier: completed=%v err=%v", completed, err)
	}
	if completed, err := s.CompletePRDesignContractDelivery(ctx, owner.ID, prURL, delivery.Token, delivery.Revision); err != nil || !completed {
		t.Fatalf("owner could not clear barrier: completed=%v err=%v", completed, err)
	}
	if _, pending, err := s.GetPendingPRDesignContractDelivery(ctx, owner.ID, prURL); err != nil || pending {
		t.Fatalf("completed barrier still pending: %v, %v", pending, err)
	}
}

func TestClaimPRDeliveryGenerationRejectsSameSessionReclaimAndTakeoverABA(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	owner := createContractSession(t, s, "mer")
	replacement := createContractSession(t, s, "mer")
	prURL := "https://github.com/acme/repo/pull/17"
	claim := func(sessionID domain.SessionID, task string) ports.ClaimOutcome {
		outcome, err := s.ClaimPR(ctx, domain.PullRequest{URL: prURL, SessionID: sessionID, Number: 17, UpdatedAt: time.Now().UTC()}, nil, nil, nil, nil, ports.ReviewWritePreserve, true, "", task)
		if err != nil {
			t.Fatal(err)
		}
		return outcome
	}
	first := claim(owner.ID, "first task")
	firstDelivery, pending, err := s.GetPendingPRDesignContractDelivery(ctx, owner.ID, prURL)
	if err != nil || !pending || firstDelivery.Token != first.ContractDeliveryToken {
		t.Fatalf("first delivery = %+v pending=%v err=%v", firstDelivery, pending, err)
	}
	second := claim(owner.ID, "reclaimed task")
	if second.ContractDeliveryToken == first.ContractDeliveryToken {
		t.Fatal("same-session reclaim reused delivery generation")
	}
	if completed, err := s.CompletePRDesignContractDelivery(ctx, owner.ID, prURL, firstDelivery.Token, firstDelivery.Revision); err != nil || completed {
		t.Fatalf("stale same-session generation cleared reclaim: completed=%v err=%v", completed, err)
	}
	secondDelivery, pending, err := s.GetPendingPRDesignContractDelivery(ctx, owner.ID, prURL)
	if err != nil || !pending || secondDelivery.TaskPrompt != "reclaimed task" || secondDelivery.Token != second.ContractDeliveryToken {
		t.Fatalf("reclaim delivery = %+v pending=%v err=%v", secondDelivery, pending, err)
	}
	third := claim(replacement.ID, "replacement task")
	if _, pending, err := s.GetPendingPRDesignContractDelivery(ctx, owner.ID, prURL); err != nil || pending {
		t.Fatalf("previous owner can still read takeover delivery: pending=%v err=%v", pending, err)
	}
	if completed, err := s.CompletePRDesignContractDelivery(ctx, owner.ID, prURL, secondDelivery.Token, secondDelivery.Revision); err != nil || completed {
		t.Fatalf("previous owner generation cleared takeover: completed=%v err=%v", completed, err)
	}
	thirdDelivery, pending, err := s.GetPendingPRDesignContractDelivery(ctx, replacement.ID, prURL)
	if err != nil || !pending || thirdDelivery.Token != third.ContractDeliveryToken || thirdDelivery.TaskPrompt != "replacement task" {
		t.Fatalf("takeover delivery = %+v pending=%v err=%v", thirdDelivery, pending, err)
	}
}

func TestPendingDeliveryRevisionRetriesAfterInvariantAppend(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	owner := createContractSession(t, s, "mer")
	prURL := "https://github.com/acme/repo/pull/19"
	if _, err := s.ClaimPR(ctx, domain.PullRequest{URL: prURL, SessionID: owner.ID, Number: 19, UpdatedAt: time.Now().UTC()}, nil, nil, nil, nil, ports.ReviewWritePreserve, true, "", "fix review"); err != nil {
		t.Fatal(err)
	}
	stale, pending, err := s.GetPendingPRDesignContractDelivery(ctx, owner.ID, prURL)
	if err != nil || !pending {
		t.Fatalf("initial pending delivery = %+v pending=%v err=%v", stale, pending, err)
	}
	invariant := "Every claim-ready acknowledgement fences the canonical contract revision."
	if _, err := s.AddPRDesignContractInvariant(ctx, owner.ID, prURL, invariant, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if completed, err := s.CompletePRDesignContractDelivery(ctx, owner.ID, prURL, stale.Token, stale.Revision); err != nil || completed {
		t.Fatalf("stale pre-append delivery cleared barrier: completed=%v err=%v", completed, err)
	}
	latest, pending, err := s.GetPendingPRDesignContractDelivery(ctx, owner.ID, prURL)
	if err != nil || !pending || latest.Revision <= stale.Revision || !strings.Contains(latest.Contract, invariant) {
		t.Fatalf("retry payload did not include appended invariant: stale=%+v latest=%+v pending=%v err=%v", stale, latest, pending, err)
	}
	if completed, err := s.CompletePRDesignContractDelivery(ctx, owner.ID, prURL, latest.Token, latest.Revision); err != nil || !completed {
		t.Fatalf("latest delivery could not clear barrier: completed=%v err=%v", completed, err)
	}
}

func TestClaimPRTakeoverWaitsAcrossFinalDeliveryBoundary(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	owner := createContractSession(t, s, "mer")
	replacement := createContractSession(t, s, "mer")
	prURL := "https://github.com/acme/repo/pull/18"
	first, err := s.ClaimPR(ctx, domain.PullRequest{URL: prURL, SessionID: owner.ID, Number: 18, UpdatedAt: time.Now().UTC()}, nil, nil, nil, nil, ports.ReviewWritePreserve, true, "", "owner task")
	if err != nil {
		t.Fatal(err)
	}

	// Model the exact final delivery boundary used by session/lifecycle: the
	// generation is re-read while the per-PR delivery lock is held. A concurrent
	// takeover cannot commit between that read and pane delivery/ack.
	unlock := designcontract.LockDelivery(prURL)
	started := make(chan struct{})
	done := make(chan ports.ClaimOutcome, 1)
	errCh := make(chan error, 1)
	go func() {
		close(started)
		outcome, claimErr := s.ClaimPR(ctx, domain.PullRequest{URL: prURL, SessionID: replacement.ID, Number: 18, UpdatedAt: time.Now().UTC()}, nil, nil, nil, nil, ports.ReviewWritePreserve, true, "", "replacement task")
		if claimErr != nil {
			errCh <- claimErr
			return
		}
		done <- outcome
	}()
	<-started
	select {
	case <-done:
		unlock()
		t.Fatal("takeover crossed in-flight delivery boundary")
	case err := <-errCh:
		unlock()
		t.Fatalf("concurrent takeover: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	delivery, pending, err := s.GetPendingPRDesignContractDelivery(ctx, owner.ID, prURL)
	if err != nil || !pending || delivery.Token != first.ContractDeliveryToken {
		unlock()
		t.Fatalf("owner delivery changed inside boundary: %+v pending=%v err=%v", delivery, pending, err)
	}
	if completed, err := s.CompletePRDesignContractDelivery(ctx, owner.ID, prURL, delivery.Token, delivery.Revision); err != nil || !completed {
		unlock()
		t.Fatalf("owner delivery ack = %v, %v", completed, err)
	}
	unlock()

	var takeover ports.ClaimOutcome
	select {
	case takeover = <-done:
	case err := <-errCh:
		t.Fatal(err)
	case <-time.After(2 * time.Second):
		t.Fatal("takeover did not resume after delivery boundary")
	}
	latest, pending, err := s.GetPendingPRDesignContractDelivery(ctx, replacement.ID, prURL)
	if err != nil || !pending || latest.Token != takeover.ContractDeliveryToken || latest.Token == delivery.Token {
		t.Fatalf("replacement delivery = %+v outcome=%+v pending=%v err=%v", latest, takeover, pending, err)
	}
}

func TestReviewFindingUpdatesExactPRContractAcrossTakeover(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	owner := createContractSession(t, s, "mer")
	replacement := createContractSession(t, s, "mer")
	seed := designcontract.BuildSeed("61", "## Invariants\n- Shared initial invariant.")
	if err := s.SaveSessionDesignContractSeed(ctx, owner.ID, seed, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	pr1, pr2 := "https://github.com/acme/repo/pull/1", "https://github.com/acme/repo/pull/2"
	if err := s.WriteSCMObservation(ctx, domain.PullRequest{URL: pr1, SessionID: owner.ID, Number: 1, UpdatedAt: time.Now().UTC()}, nil, nil, nil, nil, ports.ReviewWritePreserve); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteSCMObservation(ctx, domain.PullRequest{URL: pr2, SessionID: owner.ID, Number: 2, UpdatedAt: time.Now().UTC()}, nil, nil, nil, nil, ports.ReviewWritePreserve); err != nil {
		t.Fatal(err)
	}
	if observed, ok, err := s.GetPRDesignContract(ctx, pr1); err != nil || !ok || observed != seed {
		t.Fatalf("observer-created PR contract = ok=%v contract=%q err=%v", ok, observed, err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	if err := s.UpsertReview(ctx, domain.Review{ID: "review-1", SessionID: owner.ID, ProjectID: owner.ProjectID, Harness: domain.ReviewerCodex, PRURL: pr1, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertReviewRun(ctx, domain.ReviewRun{ID: "run-1", ReviewID: "review-1", SessionID: owner.ID, Harness: domain.ReviewerCodex, PRURL: pr1, TargetSHA: "sha1", Status: domain.ReviewRunRunning, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	invariant := "Every ownership transition delivers the final atomic PR contract."
	findings := []domain.ReviewFinding{
		{ID: "run-1:1", RunID: "run-1", SessionID: owner.ID, PRURL: pr1, Round: 1, ClassTag: "contract-delivery", RootCauseNote: "root cause", ProposedInvariant: invariant, CreatedAt: now},
		{ID: "run-1:2", RunID: "run-1", SessionID: owner.ID, PRURL: pr1, Round: 1, ClassTag: "site-symptom", RootCauseNote: "arbitrary site symptom must not promote", Body: "P1 whole line must not promote", CreatedAt: now},
		{ID: "run-1:3", RunID: "run-1", SessionID: owner.ID, PRURL: pr1, Round: 1, ClassTag: "out-of-scope", ProposedInvariant: "out-of-scope proposal must not promote", OutOfScope: true, CreatedAt: now},
		{ID: "run-1:4", RunID: "run-1", SessionID: owner.ID, PRURL: pr1, Round: 1, ClassTag: "multiline", ProposedInvariant: "line one\nline two", CreatedAt: now},
		{ID: "run-1:5", RunID: "run-1", SessionID: owner.ID, PRURL: pr1, Round: 1, ClassTag: "control", ProposedInvariant: "escape\x1b[31m", CreatedAt: now},
		{ID: "run-1:6", RunID: "run-1", SessionID: owner.ID, PRURL: pr1, Round: 1, ClassTag: "oversized", ProposedInvariant: strings.Repeat("z", 513), CreatedAt: now},
	}
	if ok, err := s.CompleteReviewRunWithFindings(ctx, "run-1", domain.VerdictChangesRequested, "fix", "", "", findings); err != nil || !ok {
		t.Fatalf("complete review finding = %v, %v", ok, err)
	}

	contract1, _, _ := s.GetPRDesignContract(ctx, pr1)
	contract2, _, _ := s.GetPRDesignContract(ctx, pr2)
	if !strings.Contains(contract1, invariant) || strings.Contains(contract2, invariant) || strings.Contains(contract1, "arbitrary site symptom") || strings.Contains(contract1, "whole line") || strings.Contains(contract1, "out-of-scope proposal") || strings.Contains(contract1, "line two") || strings.Contains(contract1, "escape") || strings.Contains(contract1, strings.Repeat("z", 100)) {
		t.Fatalf("per-PR contracts leaked: pr1=%q pr2=%q", contract1, contract2)
	}
	persisted, err := s.ListReviewFindingsByRun(ctx, "run-1")
	if err != nil || len(persisted) != len(findings) {
		t.Fatalf("review findings = %+v, %v", persisted, err)
	}
	for _, finding := range persisted[2:] {
		if finding.ProposedInvariant != "" || !strings.Contains(finding.RootCauseNote, "Invariant proposal rejected") {
			t.Fatalf("invalid proposal disposition = %+v", finding)
		}
	}
	fixerInvariant := "Every human-review fix declares its exact PR invariant through AO."
	contract1, err = s.AddPRDesignContractInvariant(ctx, owner.ID, pr1, fixerInvariant, now.Add(time.Second))
	if err != nil || !strings.Contains(contract1, fixerInvariant) {
		t.Fatalf("fixer invariant write = %q, %v", contract1, err)
	}
	contract2, _, _ = s.GetPRDesignContract(ctx, pr2)
	if strings.Contains(contract2, fixerInvariant) {
		t.Fatalf("fixer invariant leaked to sibling: %q", contract2)
	}
	partial := "human-review fix declares"
	contract1, err = s.AddPRDesignContractInvariant(ctx, owner.ID, pr1, partial, now.Add(2*time.Second))
	if err != nil || !designcontract.HasInvariant(contract1, partial) {
		t.Fatalf("substring proposal was incorrectly deduplicated: %q, %v", contract1, err)
	}
	differentCase := "Every human-review fix DECLARES its exact PR invariant through AO."
	contract1, err = s.AddPRDesignContractInvariant(ctx, owner.ID, pr1, differentCase, now.Add(3*time.Second))
	if err != nil || !designcontract.HasInvariant(contract1, differentCase) {
		t.Fatalf("case-distinct proposal was incorrectly deduplicated: %q, %v", contract1, err)
	}
	unchanged, err := s.AddPRDesignContractInvariant(ctx, owner.ID, pr1, "  "+fixerInvariant+"  ", now.Add(4*time.Second))
	if err != nil || unchanged != contract1 {
		t.Fatalf("normalized exact duplicate changed contract: equal=%v err=%v", unchanged == contract1, err)
	}
	if _, err := s.AddPRDesignContractInvariant(ctx, owner.ID, pr1, "control\x1b[31m", now.Add(5*time.Second)); err == nil {
		t.Fatal("control-character invariant was persisted")
	}
	outcome := claimContractPR(t, s, replacement.ID, pr1, 1)
	if !strings.Contains(outcome.DesignContract, invariant) || !strings.Contains(outcome.DesignContract, fixerInvariant) {
		t.Fatalf("takeover lost review-discovered invariant: %q", outcome.DesignContract)
	}
	prs, err := s.ListPRsBySession(ctx, owner.ID)
	if err != nil || len(prs) != 1 || prs[0].URL != pr2 {
		t.Fatalf("sibling PR ownership changed: %+v, %v", prs, err)
	}

	// A finding cannot use one run as authority to mutate a sibling PR.
	if err := s.InsertReviewRun(ctx, domain.ReviewRun{ID: "run-2", ReviewID: "review-1", SessionID: owner.ID, Harness: domain.ReviewerCodex, PRURL: pr2, TargetSHA: "sha2", Status: domain.ReviewRunRunning, CreatedAt: now.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	bad := domain.ReviewFinding{ID: "run-2:1", RunID: "run-2", SessionID: owner.ID, PRURL: pr1, Round: 1, ClassTag: "bad", ProposedInvariant: "must not leak", CreatedAt: now.Add(time.Second)}
	if ok, err := s.CompleteReviewRunWithFindings(ctx, "run-2", domain.VerdictChangesRequested, "bad", "", "", []domain.ReviewFinding{bad}); err == nil || ok {
		t.Fatalf("mismatched finding provenance = %v, %v", ok, err)
	}
}

func TestReviewInvariantCapacityRejectionDoesNotRollbackFinding(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	owner := createContractSession(t, s, "mer")
	seed := strings.Repeat("x", designcontract.MaxCanonicalBytes-80)
	if err := s.SaveSessionDesignContractSeed(ctx, owner.ID, seed, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	prURL := "https://github.com/acme/repo/pull/8"
	claimContractPR(t, s, owner.ID, prURL, 8)
	now := time.Now().UTC().Truncate(time.Second)
	if err := s.UpsertReview(ctx, domain.Review{ID: "review-cap", SessionID: owner.ID, ProjectID: owner.ProjectID, Harness: domain.ReviewerCodex, PRURL: prURL, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertReviewRun(ctx, domain.ReviewRun{ID: "run-cap", ReviewID: "review-cap", SessionID: owner.ID, Harness: domain.ReviewerCodex, PRURL: prURL, TargetSHA: "sha-cap", Status: domain.ReviewRunRunning, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	finding := domain.ReviewFinding{ID: "run-cap:1", RunID: "run-cap", SessionID: owner.ID, PRURL: prURL, Round: 1, ClassTag: "capacity", ProposedInvariant: strings.Repeat("i", 128), CreatedAt: now}
	if ok, err := s.CompleteReviewRunWithFindings(ctx, "run-cap", domain.VerdictChangesRequested, "fix", "", "", []domain.ReviewFinding{finding}); err != nil || !ok {
		t.Fatalf("capacity review completion = %v, %v", ok, err)
	}
	findings, err := s.ListReviewFindingsByRun(ctx, "run-cap")
	if err != nil || len(findings) != 1 || findings[0].ProposedInvariant != "" || !strings.Contains(findings[0].RootCauseNote, "capacity exceeded") {
		t.Fatalf("capacity finding disposition = %+v, %v", findings, err)
	}
	contract, _, _ := s.GetPRDesignContract(ctx, prURL)
	if contract != seed {
		t.Fatalf("capacity rejection changed canonical bytes: %d/%d", len(contract), len(seed))
	}
}

func TestClaimPRRollsBackOwnershipWhenCanonicalContractWriteFails(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	owner := createContractSession(t, s, "mer")
	// Canonical PR contracts have an explicit 1 MiB durability bound. The
	// session seed can be staged, but finalization must reject it atomically.
	if err := s.SaveSessionDesignContractSeed(ctx, owner.ID, strings.Repeat("x", 1024*1024+1), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	prURL := "https://github.com/acme/repo/pull/99"
	pr := domain.PullRequest{URL: prURL, SessionID: owner.ID, Number: 99, UpdatedAt: time.Now().UTC()}
	if _, err := s.ClaimPR(ctx, pr, nil, nil, nil, nil, ports.ReviewWritePreserve, true, "", ""); err == nil {
		t.Fatal("oversized canonical contract claim unexpectedly succeeded")
	}
	prs, err := s.ListPRsBySession(ctx, owner.ID)
	if err != nil || len(prs) != 0 {
		t.Fatalf("failed contract finalization left PR ownership: %+v, %v", prs, err)
	}
	if _, ok, err := s.GetPRDesignContract(ctx, prURL); err != nil || ok {
		t.Fatalf("failed contract finalization left canonical row: ok=%v err=%v", ok, err)
	}
}

func createContractSession(t *testing.T, s *sqlite.Store, project string) domain.SessionRecord {
	t.Helper()
	rec, err := s.CreateSession(context.Background(), sampleRecord(project))
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func claimContractPR(t *testing.T, s *sqlite.Store, sessionID domain.SessionID, url string, number int) ports.ClaimOutcome {
	t.Helper()
	outcome, err := s.ClaimPR(context.Background(), domain.PullRequest{URL: url, SessionID: sessionID, Number: number, UpdatedAt: time.Now().UTC()}, nil, nil, nil, nil, ports.ReviewWritePreserve, true, "", "")
	if err != nil {
		t.Fatalf("claim %s: %v", url, err)
	}
	return outcome
}
