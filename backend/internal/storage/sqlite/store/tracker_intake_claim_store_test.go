package store_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

func intakeClaim(project, issue, token string, now time.Time) ports.TrackerIntakeClaim {
	return ports.TrackerIntakeClaim{
		ProjectID: domain.ProjectID(project), Provider: domain.TrackerProviderGitHub,
		Repo: "acme/demo", IssueID: issue, OwnerToken: token,
		ClaimedAt: now, LeaseExpiresAt: now.Add(5 * time.Minute),
	}
}

func TestTrackerIntakeClaimHasExactlyOneConcurrentWinner(t *testing.T) {
	dir := t.TempDir()
	first, err := sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	seedProject(t, first, "demo")
	second, err := sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })

	now := time.Now().UTC()
	start := make(chan struct{})
	results := make(chan ports.TrackerIntakeClaimResult, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, candidate := range []struct {
		store *sqlite.Store
		token string
	}{{first, "owner-a"}, {second, "owner-b"}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, err := candidate.store.ClaimTrackerIntakeIssue(context.Background(), intakeClaim("demo", "acme/demo#28", candidate.token, now), 4)
			results <- result
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	winners := 0
	for result := range results {
		if result == ports.TrackerIntakeClaimAcquired {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("claim winners = %d, want exactly 1", winners)
	}
}

func TestTrackerIntakeClaimSurvivesReopenAndExpiredLeaseRetries(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	first, err := sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	seedProject(t, first, "demo")
	if result, err := first.ClaimTrackerIntakeIssue(ctx, intakeClaim("demo", "acme/demo#28", "owner-a", now), 1); err != nil || result != ports.TrackerIntakeClaimAcquired {
		t.Fatalf("initial claim result=%v err=%v", result, err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	beforeExpiry := intakeClaim("demo", "acme/demo#28", "owner-b", now.Add(time.Minute))
	if result, err := second.ClaimTrackerIntakeIssue(ctx, beforeExpiry, 1); err != nil || result != ports.TrackerIntakeClaimBusy {
		t.Fatalf("live durable lease result=%v err=%v", result, err)
	}
	afterExpiry := intakeClaim("demo", "acme/demo#28", "owner-b", now.Add(6*time.Minute))
	if result, err := second.ClaimTrackerIntakeIssue(ctx, afterExpiry, 1); err != nil || result != ports.TrackerIntakeClaimAcquired {
		t.Fatalf("expired durable lease result=%v err=%v", result, err)
	}
}

func TestTrackerIntakeClaimReconcilesCrashSurvivingSession(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	first, err := sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	seedProject(t, first, "demo")
	claim := intakeClaim("demo", "acme/demo#28", "owner-a", now)
	if result, err := first.ClaimTrackerIntakeIssue(ctx, claim, 1); err != nil || result != ports.TrackerIntakeClaimAcquired {
		t.Fatalf("claim result=%v err=%v", result, err)
	}
	record := sampleRecord("demo")
	record.Metadata = domain.SessionMetadata{}
	record.IssueID = "github:acme/demo#28"
	record, err = first.CreateClaimedSession(ctx, record, claim, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if started, err := first.MarkTrackerIntakeSpawnStarted(ctx, claim, record.ID, now.Add(2*time.Second)); err != nil || !started {
		t.Fatalf("spawn started=%v err=%v", started, err)
	}
	record.Metadata = domain.SessionMetadata{WorkspacePath: "/workspace", RuntimeHandleID: "runtime", Prompt: "work"}
	if err := first.UpdateSession(ctx, record); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	retry := intakeClaim("demo", "acme/demo#28", "owner-b", now.Add(10*time.Minute))
	if result, err := second.ClaimTrackerIntakeIssue(ctx, retry, 1); err != nil || result != ports.TrackerIntakeClaimAlreadyProcessed {
		t.Fatalf("post-crash reconciliation result=%v err=%v", result, err)
	}
	if released, err := second.ReleaseTrackerIntakeIssue(ctx, retry, retry.ClaimedAt); err != nil || released {
		t.Fatalf("release with durable session released=%v err=%v", released, err)
	}
}

func TestTrackerIntakeFailedSpawnReleaseIsTokenFencedAndRetryable(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	seedProject(t, store, "demo")
	now := time.Now().UTC()
	old := intakeClaim("demo", "acme/demo#28", "owner-a", now)
	if result, err := store.ClaimTrackerIntakeIssue(ctx, old, 1); err != nil || result != ports.TrackerIntakeClaimAcquired {
		t.Fatalf("old claim result=%v err=%v", result, err)
	}
	newer := intakeClaim("demo", "acme/demo#28", "owner-b", now.Add(6*time.Minute))
	if result, err := store.ClaimTrackerIntakeIssue(ctx, newer, 1); err != nil || result != ports.TrackerIntakeClaimAcquired {
		t.Fatalf("takeover result=%v err=%v", result, err)
	}
	if released, err := store.ReleaseTrackerIntakeIssue(ctx, old, newer.ClaimedAt); err != nil || released {
		t.Fatalf("stale release released=%v err=%v", released, err)
	}
	if result, err := store.ClaimTrackerIntakeIssue(ctx, newer, 1); err != nil || result != ports.TrackerIntakeClaimAcquired {
		t.Fatalf("successor after stale release result=%v err=%v", result, err)
	}
	if released, err := store.ReleaseTrackerIntakeIssue(ctx, newer, newer.ClaimedAt); err != nil || !released {
		t.Fatalf("owner release released=%v err=%v", released, err)
	}
	retry := intakeClaim("demo", "acme/demo#28", "owner-c", newer.ClaimedAt.Add(time.Second))
	if result, err := store.ClaimTrackerIntakeIssue(ctx, retry, 1); err != nil || result != ports.TrackerIntakeClaimAcquired {
		t.Fatalf("retry claim result=%v err=%v", result, err)
	}
}

func TestTrackerIntakeClaimsAtomicallyAccountForCapacity(t *testing.T) {
	dir := t.TempDir()
	first, err := sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	seedProject(t, first, "demo")
	second, err := sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })

	now := time.Now().UTC()
	start := make(chan struct{})
	results := make(chan ports.TrackerIntakeClaimResult, 2)
	var wg sync.WaitGroup
	for _, candidate := range []struct {
		store *sqlite.Store
		issue string
		token string
	}{{first, "acme/demo#28", "a"}, {second, "acme/demo#29", "b"}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, err := candidate.store.ClaimTrackerIntakeIssue(context.Background(), intakeClaim("demo", candidate.issue, candidate.token, now), 1)
			if err != nil {
				t.Errorf("claim %s: %v", candidate.issue, err)
			}
			results <- result
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	counts := map[ports.TrackerIntakeClaimResult]int{}
	for result := range results {
		counts[result]++
	}
	if counts[ports.TrackerIntakeClaimAcquired] != 1 || counts[ports.TrackerIntakeClaimCapacityReached] != 1 {
		t.Fatalf("capacity results = %#v, want one acquired and one full", counts)
	}
}

func TestTrackerIntakeTransientSeedNeverCompletesClaimAndFailureRetriesAfterReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	first, err := sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	seedProject(t, first, "demo")
	owner := intakeClaim("demo", "acme/demo#28", "owner-a", now)
	if result, err := first.ClaimTrackerIntakeIssue(ctx, owner, 1); err != nil || result != ports.TrackerIntakeClaimAcquired {
		t.Fatalf("claim result=%v err=%v", result, err)
	}
	record := sampleRecord("demo")
	record.Metadata = domain.SessionMetadata{}
	record.IssueID = "github:acme/demo#28"
	record, err = first.CreateClaimedSession(ctx, record, owner, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	contender := intakeClaim("demo", "acme/demo#28", "owner-b", now.Add(2*time.Second))
	if result, err := first.ClaimTrackerIntakeIssue(ctx, contender, 1); err != nil || result != ports.TrackerIntakeClaimBusy {
		t.Fatalf("claim against transient seed result=%v err=%v, want busy not completed", result, err)
	}
	if deleted, err := first.DeleteSession(ctx, record.ID); err != nil || !deleted {
		t.Fatalf("delete failed seed deleted=%v err=%v", deleted, err)
	}
	if released, err := first.ReleaseTrackerIntakeIssue(ctx, owner, now.Add(3*time.Second)); err != nil || !released {
		t.Fatalf("release failed spawn released=%v err=%v", released, err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	retry := intakeClaim("demo", "acme/demo#28", "owner-c", now.Add(4*time.Second))
	if result, err := second.ClaimTrackerIntakeIssue(ctx, retry, 1); err != nil || result != ports.TrackerIntakeClaimAcquired {
		t.Fatalf("post-restart retry result=%v err=%v", result, err)
	}
}

func TestTrackerIntakeExpiredAdmissionWithSurvivingPureSeedRecoversAfterReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	first, err := sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	seedProject(t, first, "demo")
	original := intakeClaim("demo", "acme/demo#28", "owner-a", now)
	if result, err := first.ClaimTrackerIntakeIssue(ctx, original, 1); err != nil || result != ports.TrackerIntakeClaimAcquired {
		t.Fatalf("original claim result=%v err=%v", result, err)
	}
	seed := sampleRecord("demo")
	seed.Metadata = domain.SessionMetadata{}
	seed.IssueID = "github:acme/demo#28"
	seed, err = first.CreateClaimedSession(ctx, seed, original, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	// Crash before MarkTrackerIntakeSpawnStarted: no external side effect can
	// have begun, so the exact pure seed is bounded recovery state.
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	successor := intakeClaim("demo", "acme/demo#28", "owner-b", now.Add(6*time.Minute))
	if hasCapacity, err := store.TrackerIntakeHasCapacity(ctx, "demo", 1, successor.ClaimedAt); err != nil || !hasCapacity {
		t.Fatalf("expired recoverable seed capacity=%v err=%v, want retry slot", hasCapacity, err)
	}
	if result, err := store.ClaimTrackerIntakeIssue(ctx, successor, 1); err != nil || result != ports.TrackerIntakeClaimAcquired {
		t.Fatalf("takeover result=%v err=%v, want recovered claim", result, err)
	}
	if _, ok, err := store.GetSession(ctx, seed.ID); err != nil || ok {
		t.Fatalf("recoverable expired seed survived: ok=%v err=%v", ok, err)
	}
	late := sampleRecord("demo")
	late.Metadata = domain.SessionMetadata{}
	late.IssueID = "github:acme/demo#28"
	if _, err := store.CreateClaimedSession(ctx, late, original, successor.ClaimedAt); !errors.Is(err, ports.ErrTrackerIntakeClaimLost) {
		t.Fatalf("late original admission err=%v, want ErrTrackerIntakeClaimLost", err)
	}
	if _, err := store.CreateClaimedSession(ctx, late, successor, successor.ClaimedAt.Add(time.Second)); err != nil {
		t.Fatalf("successor admission: %v", err)
	}
}

func TestTrackerIntakeExpiredSpawningSeedRemainsFencedAfterReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	first, err := sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	seedProject(t, first, "demo")
	original := intakeClaim("demo", "acme/demo#28", "owner-a", now)
	if result, err := first.ClaimTrackerIntakeIssue(ctx, original, 1); err != nil || result != ports.TrackerIntakeClaimAcquired {
		t.Fatalf("original claim result=%v err=%v", result, err)
	}
	seed := sampleRecord("demo")
	seed.Metadata = domain.SessionMetadata{}
	seed.IssueID = "github:acme/demo#28"
	seed, err = first.CreateClaimedSession(ctx, seed, original, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if started, err := first.MarkTrackerIntakeSpawnStarted(ctx, original, seed.ID, now.Add(2*time.Second)); err != nil || !started {
		t.Fatalf("spawn started=%v err=%v", started, err)
	}
	// Crash after the side-effect fence but before handles reach the session row.
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	successor := intakeClaim("demo", "acme/demo#28", "owner-b", now.Add(6*time.Minute))
	if hasCapacity, err := second.TrackerIntakeHasCapacity(ctx, "demo", 1, successor.ClaimedAt); err != nil || hasCapacity {
		t.Fatalf("unsafe spawning seed capacity=%v err=%v, want fenced slot", hasCapacity, err)
	}
	if result, err := second.ClaimTrackerIntakeIssue(ctx, successor, 1); err != nil || result != ports.TrackerIntakeClaimBusy {
		t.Fatalf("unsafe takeover result=%v err=%v, want busy", result, err)
	}
	if _, ok, err := second.GetSession(ctx, seed.ID); err != nil || !ok {
		t.Fatalf("unsafe seed was deleted: ok=%v err=%v", ok, err)
	}
}

func TestTrackerIntakeExpiredTakeoverAllowsAlreadyDeletedBoundSeedAfterReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	first, err := sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	seedProject(t, first, "demo")
	original := intakeClaim("demo", "acme/demo#28", "owner-a", now)
	if result, err := first.ClaimTrackerIntakeIssue(ctx, original, 1); err != nil || result != ports.TrackerIntakeClaimAcquired {
		t.Fatalf("original claim result=%v err=%v", result, err)
	}
	seed := sampleRecord("demo")
	seed.Metadata = domain.SessionMetadata{}
	seed.IssueID = "github:acme/demo#28"
	seed, err = first.CreateClaimedSession(ctx, seed, original, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if deleted, err := first.DeleteSession(ctx, seed.ID); err != nil || !deleted {
		t.Fatalf("rollback seed deleted=%v err=%v", deleted, err)
	}
	// Deliberately omit ReleaseTrackerIntakeIssue, modeling a crash in the gap.
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	successor := intakeClaim("demo", "acme/demo#28", "owner-b", now.Add(6*time.Minute))
	if result, err := second.ClaimTrackerIntakeIssue(ctx, successor, 1); err != nil || result != ports.TrackerIntakeClaimAcquired {
		t.Fatalf("takeover after rollback/crash result=%v err=%v", result, err)
	}
}

func TestTrackerIntakeExpiredTakeoverStillRejectsExistingBoundNonSeed(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	seedProject(t, store, "demo")
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	original := intakeClaim("demo", "acme/demo#28", "owner-a", now)
	if result, err := store.ClaimTrackerIntakeIssue(ctx, original, 1); err != nil || result != ports.TrackerIntakeClaimAcquired {
		t.Fatalf("original claim result=%v err=%v", result, err)
	}
	bound := sampleRecord("demo")
	bound.Metadata = domain.SessionMetadata{}
	bound.IssueID = "github:acme/demo#28"
	bound, err := store.CreateClaimedSession(ctx, bound, original, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	// Move the exact bound row out of the claim identity and out of seed state.
	// It must neither reconcile as this issue nor be deleted by takeover.
	bound.IssueID = "github:other/repo#99"
	bound.Metadata.WorkspacePath = "/workspace"
	bound.Metadata.Prompt = "running"
	if err := store.UpdateSession(ctx, bound); err != nil {
		t.Fatal(err)
	}
	successor := intakeClaim("demo", "acme/demo#28", "owner-b", now.Add(6*time.Minute))
	if _, err := store.ClaimTrackerIntakeIssue(ctx, successor, 1); err == nil || !strings.Contains(err.Error(), "references non-seed session") {
		t.Fatalf("takeover err=%v, want existing non-seed rejection", err)
	}
	if _, ok, err := store.GetSession(ctx, bound.ID); err != nil || !ok {
		t.Fatalf("bound non-seed was removed: ok=%v err=%v", ok, err)
	}
}

func TestTrackerIntakeRecognizesLegacyMixedCaseGitHubSessionOnly(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	seedProject(t, store, "demo")
	now := time.Now().UTC().Truncate(time.Second)
	legacy := sampleRecord("demo")
	legacy.IssueID = "github:Acme/Demo#12"
	legacy.IsTerminated = true
	if _, err := store.CreateSession(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	githubClaim := intakeClaim("demo", "acme/demo#12", "github-owner", now)
	if result, err := store.ClaimTrackerIntakeIssue(ctx, githubClaim, 2); err != nil || result != ports.TrackerIntakeClaimAlreadyProcessed {
		t.Fatalf("legacy GitHub reconciliation result=%v err=%v", result, err)
	}

	caseSensitive := intakeClaim("demo", "group/repo#12", "gitlab-owner", now)
	caseSensitive.Provider = domain.TrackerProvider("gitlab")
	caseSensitive.Repo = "group/repo"
	legacyOther := sampleRecord("demo")
	legacyOther.IssueID = "gitlab:Group/Repo#12"
	legacyOther.IsTerminated = true // avoid consuming capacity; only identity matching is under test
	if _, err := store.CreateSession(ctx, legacyOther); err != nil {
		t.Fatal(err)
	}
	if result, err := store.ClaimTrackerIntakeIssue(ctx, caseSensitive, 2); err != nil || result != ports.TrackerIntakeClaimAcquired {
		t.Fatalf("case-sensitive provider result=%v err=%v, want distinct acquired claim", result, err)
	}
}

func TestTrackerIntakeTerminatedFailedAdmissionCanBeReleasedAndRetried(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	seedProject(t, store, "demo")
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	original := intakeClaim("demo", "acme/demo#28", "owner-a", now)
	if result, err := store.ClaimTrackerIntakeIssue(ctx, original, 1); err != nil || result != ports.TrackerIntakeClaimAcquired {
		t.Fatalf("original claim result=%v err=%v", result, err)
	}
	failed := sampleRecord("demo")
	failed.Metadata = domain.SessionMetadata{}
	failed.IssueID = "github:acme/demo#28"
	failed, err := store.CreateClaimedSession(ctx, failed, original, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	// Prompt delivery after runtime start can leave admission metadata behind,
	// but termination proves this worker was not successfully admitted.
	failed.IsTerminated = true
	failed.Metadata.Prompt = "failed prompt delivery"
	if err := store.UpdateSession(ctx, failed); err != nil {
		t.Fatal(err)
	}
	if released, err := store.ReleaseTrackerIntakeIssue(ctx, original, now.Add(2*time.Second)); err != nil || !released {
		t.Fatalf("failed admission released=%v err=%v", released, err)
	}
	retry := intakeClaim("demo", "acme/demo#28", "owner-b", now.Add(3*time.Second))
	if result, err := store.ClaimTrackerIntakeIssue(ctx, retry, 1); err != nil || result != ports.TrackerIntakeClaimAcquired {
		t.Fatalf("retry claim result=%v err=%v", result, err)
	}
}

func TestTrackerIntakeCompletionAcceptsExactSessionThatTerminatedAfterSpawn(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	seedProject(t, store, "demo")
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	claim := intakeClaim("demo", "acme/demo#28", "owner-a", now)
	if result, err := store.ClaimTrackerIntakeIssue(ctx, claim, 1); err != nil || result != ports.TrackerIntakeClaimAcquired {
		t.Fatalf("claim result=%v err=%v", result, err)
	}
	record := sampleRecord("demo")
	record.Metadata = domain.SessionMetadata{}
	record.IssueID = "github:acme/demo#28"
	record, err := store.CreateClaimedSession(ctx, record, claim, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if started, err := store.MarkTrackerIntakeSpawnStarted(ctx, claim, record.ID, now.Add(2*time.Second)); err != nil || !started {
		t.Fatalf("spawn started=%v err=%v", started, err)
	}
	record.Metadata = domain.SessionMetadata{WorkspacePath: "/workspace", RuntimeHandleID: "runtime", Prompt: "work"}
	record.IsTerminated = true
	if err := store.UpdateSession(ctx, record); err != nil {
		t.Fatal(err)
	}
	if completed, err := store.CompleteTrackerIntakeIssue(ctx, claim, record.ID, now.Add(3*time.Second)); err != nil || !completed {
		t.Fatalf("completion after termination completed=%v err=%v", completed, err)
	}
	retry := intakeClaim("demo", "acme/demo#28", "owner-b", now.Add(4*time.Second))
	if result, err := store.ClaimTrackerIntakeIssue(ctx, retry, 1); err != nil || result != ports.TrackerIntakeClaimAlreadyProcessed {
		t.Fatalf("completed claim result=%v err=%v", result, err)
	}
}

func TestTrackerIntakeExpiredAdmissionNeverDeletesAnySeed(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	seedProject(t, store, "demo")
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	original := intakeClaim("demo", "acme/demo#28", "owner-a", now)
	if result, err := store.ClaimTrackerIntakeIssue(ctx, original, 3); err != nil || result != ports.TrackerIntakeClaimAcquired {
		t.Fatalf("original claim result=%v err=%v", result, err)
	}
	otherRepo := intakeClaim("demo", "acme/demo#28", "other-owner", now)
	otherRepo.Repo = "mirror/demo"
	if result, err := store.ClaimTrackerIntakeIssue(ctx, otherRepo, 3); err != nil || result != ports.TrackerIntakeClaimAcquired {
		t.Fatalf("other repo claim result=%v err=%v", result, err)
	}
	claimedSeed := sampleRecord("demo")
	claimedSeed.Metadata = domain.SessionMetadata{}
	claimedSeed.IssueID = "github:acme/demo#28"
	claimedSeed, err := store.CreateClaimedSession(ctx, claimedSeed, original, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if started, err := store.MarkTrackerIntakeSpawnStarted(ctx, original, claimedSeed.ID, now.Add(2*time.Second)); err != nil || !started {
		t.Fatalf("spawn started=%v err=%v", started, err)
	}
	manualSeed := sampleRecord("demo")
	manualSeed.Metadata = domain.SessionMetadata{}
	manualSeed.IssueID = "github:acme/demo#28"
	manualSeed, err = store.CreateSession(ctx, manualSeed)
	if err != nil {
		t.Fatal(err)
	}
	otherSeed := sampleRecord("demo")
	otherSeed.Metadata = domain.SessionMetadata{}
	otherSeed.IssueID = "github:acme/demo#28"
	otherSeed, err = store.CreateClaimedSession(ctx, otherSeed, otherRepo, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}

	successor := intakeClaim("demo", "acme/demo#28", "owner-b", now.Add(6*time.Minute))
	if result, err := store.ClaimTrackerIntakeIssue(ctx, successor, 3); err != nil || result != ports.TrackerIntakeClaimBusy {
		t.Fatalf("takeover result=%v err=%v, want fail-closed busy", result, err)
	}
	for _, id := range []domain.SessionID{claimedSeed.ID, manualSeed.ID, otherSeed.ID} {
		if _, ok, err := store.GetSession(ctx, id); err != nil || !ok {
			t.Fatalf("seed %s was deleted: ok=%v err=%v", id, ok, err)
		}
	}
}

func TestTrackerIntakeRenewalPreventsTakeoverAndCapacityDoesNotDoubleCountSeed(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	seedProject(t, store, "demo")
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	owner := intakeClaim("demo", "acme/demo#28", "owner-a", now)
	if result, err := store.ClaimTrackerIntakeIssue(ctx, owner, 1); err != nil || result != ports.TrackerIntakeClaimAcquired {
		t.Fatalf("claim result=%v err=%v", result, err)
	}
	seed := sampleRecord("demo")
	seed.Metadata = domain.SessionMetadata{}
	seed.IssueID = "github:acme/demo#28"
	if _, err := store.CreateClaimedSession(ctx, seed, owner, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if hasCapacity, err := store.TrackerIntakeHasCapacity(ctx, "demo", 1, now.Add(2*time.Second)); err != nil || hasCapacity {
		t.Fatalf("capacity with live claim+seed=%v err=%v, want one slot used (not zero or double)", hasCapacity, err)
	}
	renewAt := now.Add(4 * time.Minute)
	if renewed, err := store.RenewTrackerIntakeIssue(ctx, owner, renewAt, renewAt.Add(5*time.Minute)); err != nil || !renewed {
		t.Fatalf("renewed=%v err=%v", renewed, err)
	}
	contender := intakeClaim("demo", "acme/demo#28", "owner-b", now.Add(6*time.Minute))
	if result, err := store.ClaimTrackerIntakeIssue(ctx, contender, 1); err != nil || result != ports.TrackerIntakeClaimBusy {
		t.Fatalf("post-original-lease contender result=%v err=%v, want busy after renewal", result, err)
	}
}
