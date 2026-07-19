package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/designcontract"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
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
