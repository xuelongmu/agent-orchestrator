package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

func newTestStore(t *testing.T) *sqlite.Store {
	t.Helper()
	s, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func seedProject(t *testing.T, s *sqlite.Store, id string) {
	t.Helper()
	if err := s.UpsertProject(context.Background(), domain.ProjectRecord{
		ID: id, Path: "/tmp/" + id, RegisteredAt: time.Now().UTC().Truncate(time.Second),
	}); err != nil {
		t.Fatalf("seed project %s: %v", id, err)
	}
}

func sampleRecord(project string) domain.SessionRecord {
	now := time.Now().UTC().Truncate(time.Second)
	return domain.SessionRecord{
		ProjectID: domain.ProjectID(project),
		Kind:      domain.KindWorker,
		Harness:   domain.HarnessClaudeCode,
		Activity:  domain.Activity{State: domain.ActivityActive, LastActivityAt: now},
		Metadata:  domain.SessionMetadata{Branch: "feat/x", WorkspacePath: "/ws"},
		CreatedAt: now,
		UpdatedAt: now,
	}
}

func decodeDependencyIDs(t *testing.T, encoded string) []domain.SessionID {
	t.Helper()
	ids, err := domain.DecodeSessionDependencyIDs(encoded)
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

func TestProjectCRUDAndArchive(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")

	got, ok, err := s.GetProject(ctx, "mer")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.ID != "mer" || got.Path != "/tmp/mer" {
		t.Fatalf("project = %+v", got)
	}
	if list, _ := s.ListProjects(ctx); len(list) != 1 {
		t.Fatalf("active list = %d, want 1", len(list))
	}
	// archive hides from the active list but still resolves by id.
	if ok, err := s.ArchiveProject(ctx, "mer", time.Now().UTC()); err != nil || !ok {
		t.Fatalf("archive: ok=%v err=%v", ok, err)
	}
	if list, _ := s.ListProjects(ctx); len(list) != 0 {
		t.Fatalf("after archive, active list = %d, want 0", len(list))
	}
	if _, ok, _ := s.GetProject(ctx, "mer"); !ok {
		t.Fatal("archived project must still resolve by id")
	}
}

func TestCIRerunAttemptIsDurableAndUniquePerPRHeadCheck(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	session, err := s.CreateSession(ctx, sampleRecord("mer"))
	if err != nil {
		t.Fatal(err)
	}
	prURL := "https://github.com/o/r/pull/1"
	if err := s.WritePR(ctx, domain.PullRequest{URL: prURL, SessionID: session.ID, Number: 1, UpdatedAt: time.Now().UTC()}, nil, nil); err != nil {
		t.Fatal(err)
	}
	attempt := ports.SCMCIRerunAttempt{
		PRURL: prURL, HeadSHA: "abc", CheckName: "test", ProviderID: "101",
		Status: ports.SCMCIRerunReserved, RequestedAt: time.Now().UTC().Truncate(time.Second),
	}
	reserved, err := s.ReserveCIRerunAttempt(ctx, attempt)
	if err != nil || !reserved {
		t.Fatalf("first reserve = %v, err=%v", reserved, err)
	}
	reserved, err = s.ReserveCIRerunAttempt(ctx, attempt)
	if err != nil || reserved {
		t.Fatalf("duplicate reserve = %v, err=%v", reserved, err)
	}
	attempt.Status = ports.SCMCIRerunRequested
	if err := s.UpdateCIRerunAttempt(ctx, attempt); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.GetCIRerunAttempt(ctx, prURL, "abc", "test")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.Status != ports.SCMCIRerunRequested || got.ProviderID != "101" || !got.RequestedAt.Equal(attempt.RequestedAt) {
		t.Fatalf("attempt = %#v, want %#v", got, attempt)
	}
}

func TestProjectConfigRoundTrips(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	// A config with mixed field kinds (scalar, map, list, nested) survives the
	// JSON round trip.
	cfg := domain.ProjectConfig{
		WorkspaceKind:     domain.WorkspaceKindScratch,
		DefaultBranch:     "develop",
		Env:               map[string]string{"FOO": "bar"},
		Symlinks:          []string{".env"},
		PostCreate:        []string{"echo hi"},
		AgentRules:        "Run focused tests.",
		AgentRulesFile:    "docs/agent-rules.md",
		OrchestratorRules: "Keep workers unblocked.",
		AgentConfig:       domain.AgentConfig{Model: "claude-opus-4-5", Permissions: domain.PermissionModeAcceptEdits},
		Worker:            domain.RoleOverride{Harness: domain.HarnessCodex},
	}
	if err := s.UpsertProject(ctx, domain.ProjectRecord{
		ID: "cfg", Path: "/tmp/cfg", RegisteredAt: now, Config: cfg,
	}); err != nil {
		t.Fatalf("upsert with config: %v", err)
	}
	got, ok, err := s.GetProject(ctx, "cfg")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if !reflect.DeepEqual(got.Config, cfg) {
		t.Fatalf("config = %#v, want %#v", got.Config, cfg)
	}

	// An unset config round-trips back to a zero value rather than an empty object.
	seedProject(t, s, "nocfg")
	got, _, _ = s.GetProject(ctx, "nocfg")
	if !got.Config.IsZero() {
		t.Fatalf("unset config = %#v, want zero", got.Config)
	}

	// Clearing replaces a previously-set config with a zero value.
	if err := s.UpsertProject(ctx, domain.ProjectRecord{
		ID: "cfg", Path: "/tmp/cfg", RegisteredAt: now, Config: domain.ProjectConfig{},
	}); err != nil {
		t.Fatalf("clear config: %v", err)
	}
	if got, _, _ := s.GetProject(ctx, "cfg"); !got.Config.IsZero() {
		t.Fatalf("cleared config = %#v, want zero", got.Config)
	}
}

func TestSessionCreateAssignsPerProjectID(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	seedProject(t, s, "ao")

	r1, err := s.CreateSession(ctx, sampleRecord("mer"))
	if err != nil {
		t.Fatal(err)
	}
	r2, _ := s.CreateSession(ctx, sampleRecord("mer"))
	r3, _ := s.CreateSession(ctx, sampleRecord("ao"))
	if r1.ID != "mer-1" || r2.ID != "mer-2" || r3.ID != "ao-1" {
		t.Fatalf("ids = %s, %s, %s; want mer-1, mer-2, ao-1", r1.ID, r2.ID, r3.ID)
	}
	got, ok, err := s.GetSession(ctx, "mer-1")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.Activity.State != domain.ActivityActive || got.IsTerminated ||
		got.Harness != domain.HarnessClaudeCode || got.Metadata.Branch != "feat/x" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if list, _ := s.ListSessions(ctx, "mer"); len(list) != 2 {
		t.Fatalf("list mer = %d, want 2", len(list))
	}
	if all, _ := s.ListAllSessions(ctx); len(all) != 3 {
		t.Fatalf("list all = %d, want 3", len(all))
	}
}

func TestSessionDependenciesRoundTripDeduped(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "ao")
	parent, err := s.CreateSession(ctx, sampleRecord("ao"))
	if err != nil {
		t.Fatal(err)
	}
	childRecord := sampleRecord("ao")
	childRecord.DependencyIDs = domain.EncodeSessionDependencyIDs([]domain.SessionID{parent.ID, parent.ID})
	child, err := s.CreateSession(ctx, childRecord)
	if err != nil {
		t.Fatal(err)
	}

	got, ok, err := s.GetSession(ctx, child.ID)
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if gotIDs, want := decodeDependencyIDs(t, got.DependencyIDs), []domain.SessionID{parent.ID}; !reflect.DeepEqual(gotIDs, want) {
		t.Fatalf("dependsOn = %#v, want %#v", gotIDs, want)
	}
	listed, err := s.ListSessions(ctx, "ao")
	if err != nil {
		t.Fatal(err)
	}
	if gotIDs := decodeDependencyIDs(t, listed[1].DependencyIDs); !reflect.DeepEqual(gotIDs, []domain.SessionID{parent.ID}) {
		t.Fatalf("listed dependsOn = %#v", gotIDs)
	}
}

func TestDependencyPromotionRequiresCompletionAndClaimsOnceWithActivityEvent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "ao")
	parent, err := s.CreateSession(ctx, sampleRecord("ao"))
	if err != nil {
		t.Fatal(err)
	}
	childRecord := sampleRecord("ao")
	childRecord.DependencyIDs = domain.EncodeSessionDependencyIDs([]domain.SessionID{parent.ID})
	childRecord.DependencyPreparedAt = childRecord.CreatedAt
	childRecord.DependencyBasePrompt = "build child"
	child, err := s.CreateSession(ctx, childRecord)
	if err != nil {
		t.Fatal(err)
	}
	if ready, err := s.ListReadyDependencySessions(ctx); err != nil || len(ready) != 0 {
		t.Fatalf("ready before completion = %#v, err=%v", ready, err)
	}
	handoff := domain.AgentHandoff{ChangedFiles: []string{"parent.go"}, VerificationCommands: []string{"go test ./parent"}, ResidualRisk: "none"}
	if created, err := s.PutSessionHandoff(ctx, parent.ID, handoff, time.Now().UTC()); err != nil || !created {
		t.Fatalf("put handoff = %v, %v", created, err)
	}
	ready, err := s.ListReadyDependencySessions(ctx)
	if err != nil || !reflect.DeepEqual(ready, []domain.SessionID{child.ID}) {
		t.Fatalf("ready after completion = %#v, err=%v", ready, err)
	}
	parents, err := s.ListDependencyHandoffs(ctx, child.ID)
	if err != nil || len(parents) != 1 || parents[0].Handoff == nil || !parents[0].Handoff.Equal(handoff) {
		t.Fatalf("parent handoffs = %#v, err=%v", parents, err)
	}
	promotedAt := time.Now().UTC().Add(time.Second).Truncate(time.Millisecond)
	if claimed, err := s.ReserveDependencyPromotion(ctx, child.ID, "owner-a", promotedAt); err != nil || !claimed {
		t.Fatalf("first claim = %v, %v", claimed, err)
	}
	if claimed, err := s.ReserveDependencyPromotion(ctx, child.ID, "owner-b", promotedAt.Add(time.Second)); err != nil || claimed {
		t.Fatalf("replay claim = %v, %v", claimed, err)
	}
	if completed, err := s.CompleteDependencyPromotion(ctx, child.ID, "owner-b", promotedAt); err != nil || completed {
		t.Fatalf("stale token completion = %v, %v", completed, err)
	}
	spawnedMetadata := child.Metadata
	spawnedMetadata.RuntimeHandleID = "runtime-child"
	spawnedMetadata.Prompt = "build child with handoffs"
	if marked, err := s.MarkReservedDependencySpawned(ctx, child.ID, "owner-a", spawnedMetadata, promotedAt); err != nil || !marked {
		t.Fatalf("mark spawned = %v, %v", marked, err)
	}
	if marked, err := s.MarkReservedDependencyLaunchSucceeded(ctx, child.ID, "owner-a", promotedAt); err != nil || !marked {
		t.Fatalf("mark launch succeeded = %v, %v", marked, err)
	}
	if completed, err := s.CompleteDependencyPromotion(ctx, child.ID, "owner-a", promotedAt); err != nil || !completed {
		t.Fatalf("owner completion = %v, %v", completed, err)
	}
	got, ok, err := s.GetSession(ctx, child.ID)
	if err != nil || !ok || !got.DependencyPromotedAt.Equal(promotedAt) {
		t.Fatalf("promoted child = %#v ok=%v err=%v", got, ok, err)
	}
	events, err := s.EventsAfter(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	promotions := 0
	for _, event := range events {
		if event.SessionID == string(child.ID) && strings.Contains(string(event.Payload), `"dependencyPromoted":true`) {
			promotions++
		}
	}
	if promotions != 1 {
		t.Fatalf("promotion activity events = %d; events=%#v", promotions, events)
	}
}

func TestDependencyReadyFromLifecycleTerminalMergedPRWithoutHandoff(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "ao")
	parent, err := s.CreateSession(ctx, sampleRecord("ao"))
	if err != nil {
		t.Fatal(err)
	}
	childRecord := sampleRecord("ao")
	childRecord.DependencyIDs = domain.EncodeSessionDependencyIDs([]domain.SessionID{parent.ID})
	childRecord.DependencyPreparedAt = childRecord.CreatedAt
	childRecord.DependencyBasePrompt = "base"
	childRecord.Metadata = domain.SessionMetadata{WorkspaceKind: domain.WorkspaceKindWorktree, Branch: "ao/child/root", Prompt: "base"}
	child, err := s.CreateSession(ctx, childRecord)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.WritePR(ctx, domain.PullRequest{URL: "https://example.test/pr/1", SessionID: parent.ID, Number: 1, Merged: true, UpdatedAt: time.Now().UTC()}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if ready, err := s.ListReadyDependencySessions(ctx); err != nil || len(ready) != 0 {
		t.Fatalf("merged PR without lifecycle terminal became ready: %v, %v", ready, err)
	}
	parent.IsTerminated = true
	parent.Activity.State = domain.ActivityExited
	if err := s.UpdateSession(ctx, parent); err != nil {
		t.Fatal(err)
	}
	if ready, err := s.ListReadyDependencySessions(ctx); err != nil || !reflect.DeepEqual(ready, []domain.SessionID{child.ID}) {
		t.Fatalf("terminal merged parent did not satisfy dependency: %v, %v", ready, err)
	}
	handoffs, err := s.ListDependencyHandoffs(ctx, child.ID)
	if err != nil || len(handoffs) != 1 || handoffs[0].Handoff != nil {
		t.Fatalf("missing handoff was not explicit nil context: %#v, %v", handoffs, err)
	}
	// Model completion facts changing after the scheduler's read but before its
	// reservation write. The reservation CAS must re-check every parent rather
	// than trusting the stale ready list.
	if err := s.WritePR(ctx, domain.PullRequest{URL: "https://example.test/pr/2", SessionID: parent.ID, Number: 2, UpdatedAt: time.Now().UTC()}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if claimed, err := s.ReserveDependencyPromotion(ctx, child.ID, "stale-ready", time.Now().UTC()); err != nil || claimed {
		t.Fatalf("reservation accepted stale parent completion: %v, %v", claimed, err)
	}
}

func TestDependencyChildCreationAtomicallyPersistsPreparedLaunchInputs(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "ao")
	parent, err := s.CreateSession(ctx, sampleRecord("ao"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	childRecord := sampleRecord("ao")
	childRecord.DependencyIDs = domain.EncodeSessionDependencyIDs([]domain.SessionID{parent.ID})
	childRecord.DependencyPreparedAt = now
	childRecord.DependencyBasePrompt = "immutable base"
	childRecord.DependencyBranchPrefix = "ao/"
	childRecord.DependencyBranchSuffix = "/root"
	childRecord.Metadata = domain.SessionMetadata{WorkspaceKind: domain.WorkspaceKindWorktree, Prompt: "immutable base"}
	child, err := s.CreateSession(ctx, childRecord)
	if err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.GetSession(ctx, child.ID)
	if err != nil || !ok {
		t.Fatalf("get child: ok=%v err=%v", ok, err)
	}
	if got.DependencyPreparedAt.IsZero() || got.DependencyBasePrompt != "immutable base" || got.Metadata.Prompt != "immutable base" || got.Metadata.Branch != "ao/"+string(child.ID)+"/root" {
		t.Fatalf("committed child exposed partial launch inputs: %#v", got)
	}
	if got.Metadata.RuntimeHandleID != "" || !got.DependencyPromotedAt.IsZero() {
		t.Fatalf("prepared child launched during create: %#v", got)
	}
}

func TestDependencyLaunchCASLosesToConcurrentTermination(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "ao")
	parent, err := s.CreateSession(ctx, sampleRecord("ao"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutSessionHandoff(ctx, parent.ID, domain.AgentHandoff{ChangedFiles: []string{}, VerificationCommands: []string{}}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	childRecord := sampleRecord("ao")
	childRecord.DependencyIDs = domain.EncodeSessionDependencyIDs([]domain.SessionID{parent.ID})
	childRecord.DependencyPreparedAt = childRecord.CreatedAt
	childRecord.DependencyBasePrompt = "base"
	childRecord.Metadata = domain.SessionMetadata{WorkspaceKind: domain.WorkspaceKindWorktree, Branch: "ao/child/root", Prompt: "base"}
	child, err := s.CreateSession(ctx, childRecord)
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := s.ReserveDependencyPromotion(ctx, child.ID, "owner", time.Now().UTC()); err != nil || !claimed {
		t.Fatalf("reserve = %v, %v", claimed, err)
	}
	terminated, ok, err := s.GetSession(ctx, child.ID)
	if err != nil || !ok {
		t.Fatal(err)
	}
	terminated.IsTerminated = true
	terminated.Activity.State = domain.ActivityExited
	if err := s.UpdateSession(ctx, terminated); err != nil {
		t.Fatal(err)
	}
	metadata := child.Metadata
	metadata.RuntimeHandleID = "new-runtime"
	metadata.WorkspacePath = "/owned-workspace"
	metadata.Prompt = "rendered"
	if marked, err := s.MarkReservedDependencySpawned(ctx, child.ID, "owner", metadata, time.Now().UTC()); err != nil || marked {
		t.Fatalf("terminal-losing CAS = %v, %v", marked, err)
	}
	if reset, err := s.ResetReservedDependencyLaunch(ctx, child.ID, "owner", false, time.Now().UTC()); err != nil || !reset {
		t.Fatalf("terminal cleanup reset = %v, %v", reset, err)
	}
	got, _, err := s.GetSession(ctx, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsTerminated || got.Activity.State != domain.ActivityExited || got.Metadata.RuntimeHandleID != "" || got.Metadata.WorkspacePath != "" || got.Metadata.Prompt != "base" {
		t.Fatalf("launch CAS resurrected or overwrote terminal child: %#v", got)
	}
}

func TestDependencyWorkspaceInventoryFollowsPromotionFence(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "ao")
	parent, err := s.CreateSession(ctx, sampleRecord("ao"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutSessionHandoff(ctx, parent.ID, domain.AgentHandoff{ChangedFiles: []string{}, VerificationCommands: []string{}}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	childRecord := sampleRecord("ao")
	childRecord.DependencyIDs = domain.EncodeSessionDependencyIDs([]domain.SessionID{parent.ID})
	childRecord.DependencyPreparedAt = childRecord.CreatedAt
	childRecord.DependencyBasePrompt = "base"
	childRecord.Metadata = domain.SessionMetadata{WorkspaceKind: domain.WorkspaceKindWorktree, Branch: "ao/child/root", Prompt: "base"}
	child, err := s.CreateSession(ctx, childRecord)
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := s.ReserveDependencyPromotion(ctx, child.ID, "owner", time.Now().UTC()); err != nil || !claimed {
		t.Fatalf("reserve = %v, %v", claimed, err)
	}
	metadata := child.Metadata
	metadata.WorkspacePath = "/ws/child"
	metadata.RuntimeHandleID = "runtime"
	root := domain.SessionWorktreeRecord{
		SessionID: child.ID, RepoName: domain.RootWorkspaceRepoName,
		RepoPath: ptrTo("/repos/project"), RelativePath: ptrTo(""),
		Branch: metadata.Branch, WorktreePath: metadata.WorkspacePath, State: "active",
	}

	if prepared, err := s.PrepareReservedDependencyWorkspace(ctx, child.ID, "stale", metadata, []domain.SessionWorktreeRecord{root}, time.Now().UTC()); err != nil || prepared {
		t.Fatalf("stale prepare = %v, %v", prepared, err)
	}
	if rows, err := s.ListSessionWorktrees(ctx, child.ID); err != nil || len(rows) != 0 {
		t.Fatalf("stale prepare wrote inventory: rows=%+v err=%v", rows, err)
	}
	if prepared, err := s.PrepareReservedDependencyWorkspace(ctx, child.ID, "owner", metadata, []domain.SessionWorktreeRecord{root}, time.Now().UTC()); err != nil || !prepared {
		t.Fatalf("owned prepare = %v, %v", prepared, err)
	}
	if rows, err := s.ListSessionWorktrees(ctx, child.ID); err != nil || len(rows) != 1 || !reflect.DeepEqual(rows[0], root) {
		t.Fatalf("owned prepare inventory = %+v err=%v, want %+v", rows, err, root)
	}
	if reset, err := s.ResetReservedDependencyLaunch(ctx, child.ID, "stale", false, time.Now().UTC()); err != nil || reset {
		t.Fatalf("stale reset = %v, %v", reset, err)
	}
	if rows, err := s.ListSessionWorktrees(ctx, child.ID); err != nil || len(rows) != 1 {
		t.Fatalf("stale reset consumed owned inventory: rows=%+v err=%v", rows, err)
	}
	if reset, err := s.ResetReservedDependencyLaunch(ctx, child.ID, "owner", true, time.Now().UTC()); err != nil || !reset {
		t.Fatalf("dirty owned reset = %v, %v", reset, err)
	}
	if rows, err := s.ListSessionWorktrees(ctx, child.ID); err != nil || len(rows) != 1 {
		t.Fatalf("dirty owned reset lost inventory: rows=%+v err=%v", rows, err)
	}
	if prepared, err := s.PrepareReservedDependencyWorkspace(ctx, child.ID, "owner", metadata, []domain.SessionWorktreeRecord{root}, time.Now().UTC()); err != nil || !prepared {
		t.Fatalf("owned reprepare = %v, %v", prepared, err)
	}
	if reset, err := s.ResetReservedDependencyLaunch(ctx, child.ID, "owner", false, time.Now().UTC()); err != nil || !reset {
		t.Fatalf("clean owned reset = %v, %v", reset, err)
	}
	if rows, err := s.ListSessionWorktrees(ctx, child.ID); err != nil || len(rows) != 0 {
		t.Fatalf("clean owned reset retained inventory: rows=%+v err=%v", rows, err)
	}
}

func TestStaleDependencyReservationRecoveryNeverClearsRuntimeBackedFence(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "ao")
	parent, err := s.CreateSession(ctx, sampleRecord("ao"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutSessionHandoff(ctx, parent.ID, domain.AgentHandoff{ChangedFiles: []string{}, VerificationCommands: []string{}}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	childRecord := sampleRecord("ao")
	childRecord.DependencyIDs = domain.EncodeSessionDependencyIDs([]domain.SessionID{parent.ID})
	childRecord.DependencyPreparedAt = childRecord.CreatedAt
	childRecord.DependencyBasePrompt = "base"
	childRecord.Metadata = domain.SessionMetadata{WorkspaceKind: domain.WorkspaceKindWorktree, Branch: "ao/child/root", Prompt: "base"}
	child, err := s.CreateSession(ctx, childRecord)
	if err != nil {
		t.Fatal(err)
	}
	claimedAt := time.Now().UTC().Add(-time.Hour)
	if claimed, err := s.ReserveDependencyPromotion(ctx, child.ID, "stale", claimedAt); err != nil || !claimed {
		t.Fatalf("reserve stale = %v, %v", claimed, err)
	}
	if recovered, err := s.RecoverStaleDependencyPromotions(ctx, time.Now().UTC(), time.Now().UTC().Add(-30*time.Minute)); err != nil || recovered != 1 {
		t.Fatalf("recover handleless reservation = %d, %v", recovered, err)
	}
	if ready, err := s.ListReadyDependencySessions(ctx); err != nil || !reflect.DeepEqual(ready, []domain.SessionID{child.ID}) {
		t.Fatalf("recovered child not retryable: ready=%v err=%v", ready, err)
	}
	if claimed, err := s.ReserveDependencyPromotion(ctx, child.ID, "runtime-owner", claimedAt); err != nil || !claimed {
		t.Fatalf("reserve runtime owner = %v, %v", claimed, err)
	}
	metadata := child.Metadata
	metadata.RuntimeHandleID = "runtime-boundary"
	metadata.WorkspacePath = "/owned"
	if marked, err := s.MarkReservedDependencySpawned(ctx, child.ID, "runtime-owner", metadata, claimedAt); err != nil || !marked {
		t.Fatalf("mark runtime boundary = %v, %v", marked, err)
	}
	if recovered, err := s.RecoverStaleDependencyPromotions(ctx, time.Now().UTC(), time.Now().UTC().Add(-30*time.Minute)); err != nil || recovered != 0 {
		t.Fatalf("runtime-backed fence recovered without probe = %d, %v", recovered, err)
	}
	got, _, err := s.GetSession(ctx, child.ID)
	if err != nil || got.DependencyPromotionToken != "runtime-owner" {
		t.Fatalf("runtime-backed token = %q err=%v", got.DependencyPromotionToken, err)
	}
}

func TestSessionDependenciesRejectInvalidEdges(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "ao")
	seedProject(t, s, "other")
	_, err := s.CreateSession(ctx, sampleRecord("ao"))
	if err != nil {
		t.Fatal(err)
	}
	selfRecord := sampleRecord("ao")
	selfRecord.DependencyIDs = domain.EncodeSessionDependencyIDs([]domain.SessionID{"ao-2"})
	if _, err := s.CreateSession(ctx, selfRecord); !errors.Is(err, ports.ErrDependencySelf) {
		t.Fatalf("spawn self edge error = %v, want ErrDependencySelf", err)
	}
	b, err := s.CreateSession(ctx, sampleRecord("ao"))
	if err != nil {
		t.Fatal(err)
	}
	if b.ID != "ao-2" {
		t.Fatalf("rejected dependency consumed session id: got %s", b.ID)
	}

	missing := sampleRecord("ao")
	missing.DependencyIDs = domain.EncodeSessionDependencyIDs([]domain.SessionID{"ao-999"})
	if _, err := s.CreateSession(ctx, missing); !errors.Is(err, ports.ErrDependencyNotFound) {
		t.Fatalf("missing edge error = %v, want ErrDependencyNotFound", err)
	}
	other, err := s.CreateSession(ctx, sampleRecord("other"))
	if err != nil {
		t.Fatal(err)
	}
	crossProject := sampleRecord("ao")
	crossProject.DependencyIDs = domain.EncodeSessionDependencyIDs([]domain.SessionID{other.ID})
	if _, err := s.CreateSession(ctx, crossProject); !errors.Is(err, ports.ErrDependencyProject) {
		t.Fatalf("cross-project edge error = %v, want ErrDependencyProject", err)
	}
	invalid := sampleRecord("ao")
	invalid.DependencyIDs = domain.EncodeSessionDependencyIDs([]domain.SessionID{"ao 1"})
	if _, err := s.CreateSession(ctx, invalid); !errors.Is(err, ports.ErrDependencyInvalid) {
		t.Fatalf("invalid id error = %v, want ErrDependencyInvalid", err)
	}
	embeddedNUL := sampleRecord("ao")
	embeddedNUL.DependencyIDs = domain.EncodeSessionDependencyIDs([]domain.SessionID{"ao-1\x00ao-2"})
	if _, err := s.CreateSession(ctx, embeddedNUL); !errors.Is(err, ports.ErrDependencyInvalid) {
		t.Fatalf("embedded NUL id error = %v, want ErrDependencyInvalid", err)
	}
	tooMany := sampleRecord("ao")
	tooMany.DependencyIDs = domain.EncodeSessionDependencyIDs(make([]domain.SessionID, domain.MaxSessionDependencies+1))
	if _, err := s.CreateSession(ctx, tooMany); !errors.Is(err, ports.ErrDependencyLimit) {
		t.Fatalf("dependency limit error = %v, want ErrDependencyLimit", err)
	}
	sessions, err := s.ListSessions(ctx, "ao")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 {
		t.Fatalf("sessions after rejected edges = %d, want 2", len(sessions))
	}
	for _, session := range sessions {
		if ids := decodeDependencyIDs(t, session.DependencyIDs); len(ids) != 0 {
			t.Fatalf("rejected embedded NUL created edges for %s: %#v", session.ID, ids)
		}
	}
}

func TestReferencedSeedParentIsRestrictedAndEdgesSurviveRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	seedProject(t, s, "ao")
	parentRecord := sampleRecord("ao")
	parentRecord.Metadata = domain.SessionMetadata{}
	parent, err := s.CreateSession(ctx, parentRecord)
	if err != nil {
		t.Fatal(err)
	}
	childRecord := sampleRecord("ao")
	childRecord.DependencyIDs = domain.EncodeSessionDependencyIDs([]domain.SessionID{parent.ID})
	child, err := s.CreateSession(ctx, childRecord)
	if err != nil {
		t.Fatal(err)
	}
	if deleted, err := s.DeleteSession(ctx, parent.ID); err == nil || deleted {
		t.Fatalf("referenced seed delete = deleted:%v err:%v, want FK restriction", deleted, err)
	}
	parent, ok, err := s.GetSession(ctx, parent.ID)
	if err != nil || !ok {
		t.Fatalf("parent after restricted delete: ok=%v err=%v", ok, err)
	}
	parent.IsTerminated = true // manager rollback fallback parks the failed seed.
	if err := s.UpdateSession(ctx, parent); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	parent, ok, err = s.GetSession(ctx, parent.ID)
	if err != nil || !ok || !parent.IsTerminated {
		t.Fatalf("parked parent after reopen = %#v ok=%v err=%v", parent, ok, err)
	}
	gotChild, ok, err := s.GetSession(ctx, child.ID)
	if err != nil || !ok {
		t.Fatalf("child after reopen: ok=%v err=%v", ok, err)
	}
	if gotIDs := decodeDependencyIDs(t, gotChild.DependencyIDs); !reflect.DeepEqual(gotIDs, []domain.SessionID{parent.ID}) {
		t.Fatalf("child edge after parent rollback/reopen = %#v", gotIDs)
	}
}

func TestDeletingSeedChildCascadesOutgoingDependencyEdge(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "ao")

	parentRecord := sampleRecord("ao")
	parentRecord.Metadata = domain.SessionMetadata{}
	parent, err := s.CreateSession(ctx, parentRecord)
	if err != nil {
		t.Fatal(err)
	}
	childRecord := sampleRecord("ao")
	childRecord.Metadata = domain.SessionMetadata{}
	childRecord.DependencyIDs = domain.EncodeSessionDependencyIDs([]domain.SessionID{parent.ID})
	child, err := s.CreateSession(ctx, childRecord)
	if err != nil {
		t.Fatal(err)
	}

	if deleted, err := s.DeleteSession(ctx, child.ID); err != nil || !deleted {
		t.Fatalf("delete child = %v, %v; want true, nil", deleted, err)
	}
	if deleted, err := s.DeleteSession(ctx, parent.ID); err != nil || !deleted {
		t.Fatalf("delete former parent after child cascade = %v, %v; want true, nil", deleted, err)
	}
}

func TestSessionWorkspaceKindRoundTrips(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	rec := sampleRecord("mer")
	rec.Metadata.WorkspaceKind = domain.WorkspaceKindScratch
	rec.Metadata.Branch = ""
	created, err := s.CreateSession(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.GetSession(ctx, created.ID)
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.Metadata.WorkspaceKind != domain.WorkspaceKindScratch || got.Metadata.Branch != "" {
		t.Fatalf("metadata = %#v, want branchless scratch", got.Metadata)
	}
}

func TestSessionDiagnosticSurvivesStoreRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)

	s, err := sqlite.Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	seedProject(t, s, "mer")
	rec := sampleRecord("mer")
	created, err := s.CreateSession(ctx, rec)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	created.Diagnostic = &domain.LifecycleDiagnostic{
		Trigger:       domain.DiagnosticStopFailure,
		TerminalTail:  "agent stopped after validation failed",
		HookErrorType: "validation_failed",
		CapturedAt:    now,
	}
	created.UpdatedAt = now.Add(time.Second)
	if err := s.UpdateSession(ctx, created); err != nil {
		t.Fatalf("persist diagnostic: %v", err)
	}
	events, err := s.EventsAfter(ctx, 0, 10)
	if err != nil {
		t.Fatalf("read diagnostic CDC: %v", err)
	}
	if len(events) != 2 || string(events[1].Type) != "session_updated" {
		t.Fatalf("diagnostic CDC events = %#v, want create then session_updated", events)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := sqlite.Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	got, ok, err := reopened.GetSession(ctx, created.ID)
	if err != nil || !ok {
		t.Fatalf("get after restart: ok=%v err=%v", ok, err)
	}
	if !reflect.DeepEqual(got.Diagnostic, created.Diagnostic) {
		t.Fatalf("diagnostic after restart = %#v, want %#v", got.Diagnostic, created.Diagnostic)
	}
}

// TestDeleteSessionOnlyRemovesSeedRows covers Bug 4's storage-layer guarantee:
// DeleteSession removes a session row only when the row is still in seed state
// (no workspace, no runtime handle, no agent session id, no prompt, not
// terminated). Rows that already carry spawn output are immutable so the
// no-resurrection guarantee for live sessions still holds.
func TestDeleteSessionOnlyRemovesSeedRows(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")

	// Seed row: just CreateSession output, no metadata yet.
	now := time.Now().UTC().Truncate(time.Second)
	seed := domain.SessionRecord{
		ProjectID: "mer",
		Kind:      domain.KindWorker,
		Harness:   domain.HarnessClaudeCode,
		Activity:  domain.Activity{State: domain.ActivityIdle, LastActivityAt: now},
		CreatedAt: now,
		UpdatedAt: now,
	}
	r1, err := s.CreateSession(ctx, seed)
	if err != nil {
		t.Fatalf("create seed: %v", err)
	}

	deleted, err := s.DeleteSession(ctx, r1.ID)
	if err != nil || !deleted {
		t.Fatalf("delete seed = %v %v, want true nil", deleted, err)
	}
	if _, ok, _ := s.GetSession(ctx, r1.ID); ok {
		t.Fatal("seed row still present after DeleteSession")
	}

	// A row with workspace_path populated must NOT be deleted — even if
	// !is_terminated. This is the no-resurrection guarantee for live work.
	r2, err := s.CreateSession(ctx, sampleRecord("mer"))
	if err != nil {
		t.Fatalf("create live: %v", err)
	}
	deleted, err = s.DeleteSession(ctx, r2.ID)
	if err != nil {
		t.Fatalf("delete live err = %v", err)
	}
	if deleted {
		t.Fatal("DeleteSession must be a no-op for rows with spawn output")
	}
	if _, ok, _ := s.GetSession(ctx, r2.ID); !ok {
		t.Fatal("live row was removed by DeleteSession")
	}

	// A terminated row is also out of scope: terminal-state rows hold cleanup
	// metadata users may still inspect, so the gate refuses them too.
	r3, err := s.CreateSession(ctx, seed)
	if err != nil {
		t.Fatalf("create extra seed: %v", err)
	}
	terminated := r3
	terminated.IsTerminated = true
	if err := s.UpdateSession(ctx, terminated); err != nil {
		t.Fatalf("mark terminated: %v", err)
	}
	deleted, err = s.DeleteSession(ctx, r3.ID)
	if err != nil {
		t.Fatalf("delete terminated err = %v", err)
	}
	if deleted {
		t.Fatal("DeleteSession must be a no-op for terminated rows")
	}
}

func TestSessionRenameUpdatesDisplayName(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	r, _ := s.CreateSession(ctx, sampleRecord("mer"))

	renamedAt := r.UpdatedAt.Add(time.Minute)
	ok, err := s.RenameSession(ctx, r.ID, "Fix flaky tests", renamedAt)
	if err != nil || !ok {
		t.Fatalf("rename: ok=%v err=%v", ok, err)
	}
	got, _, _ := s.GetSession(ctx, r.ID)
	if got.DisplayName != "Fix flaky tests" || !got.UpdatedAt.Equal(renamedAt) {
		t.Fatalf("rename not persisted: %+v", got)
	}

	ok, err = s.RenameSession(ctx, "mer-missing", "Missing", renamedAt)
	if err != nil {
		t.Fatalf("rename missing: %v", err)
	}
	if ok {
		t.Fatal("rename missing ok=true, want false")
	}
}

func TestSessionUpdateActivityAndTermination(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	r, _ := s.CreateSession(ctx, sampleRecord("mer"))

	r.Activity = domain.Activity{State: domain.ActivityWaitingInput, LastActivityAt: r.CreatedAt}
	r.IsTerminated = true
	if err := s.UpdateSession(ctx, r); err != nil {
		t.Fatal(err)
	}
	got, _, _ := s.GetSession(ctx, r.ID)
	if got.Activity.State != domain.ActivityWaitingInput || !got.IsTerminated {
		t.Fatalf("update not persisted: %+v", got)
	}

	got.IsTerminated = false
	got.Activity.State = domain.ActivityActive
	_ = s.UpdateSession(ctx, got)
	again, _, _ := s.GetSession(ctx, r.ID)
	if again.IsTerminated || again.Activity.State != domain.ActivityActive {
		t.Fatalf("activity/termination should update, got %+v", again)
	}
}

func TestSessionFirstSignalRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	r, _ := s.CreateSession(ctx, sampleRecord("mer"))

	// Fresh sessions have no signal receipt: NULL round-trips as zero time.
	got, _, _ := s.GetSession(ctx, r.ID)
	if !got.FirstSignalAt.IsZero() {
		t.Fatalf("fresh session has receipt: %v", got.FirstSignalAt)
	}

	stamp := time.Now().UTC().Truncate(time.Second)
	got.FirstSignalAt = stamp
	if err := s.UpdateSession(ctx, got); err != nil {
		t.Fatal(err)
	}
	again, _, _ := s.GetSession(ctx, r.ID)
	if !again.FirstSignalAt.Equal(stamp) {
		t.Fatalf("receipt not persisted: got %v want %v", again.FirstSignalAt, stamp)
	}

	// Clearing it (spawn/restore re-proves the hook pipeline) round-trips too.
	again.FirstSignalAt = time.Time{}
	if err := s.UpdateSession(ctx, again); err != nil {
		t.Fatal(err)
	}
	final, _, _ := s.GetSession(ctx, r.ID)
	if !final.FirstSignalAt.IsZero() {
		t.Fatalf("receipt not cleared: %v", final.FirstSignalAt)
	}
}

func TestSessionMergedCleanupPendingRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	r, _ := s.CreateSession(ctx, sampleRecord("mer"))

	r.Metadata.MergedCleanupPending = true
	r.Metadata.MergedCleanupPRURL = "https://github.com/o/r/pull/57"
	if err := s.UpdateSession(ctx, r); err != nil {
		t.Fatal(err)
	}
	got, _, _ := s.GetSession(ctx, r.ID)
	if !got.Metadata.MergedCleanupPending || got.Metadata.MergedCleanupPRURL != r.Metadata.MergedCleanupPRURL {
		t.Fatalf("merged cleanup replay state was not persisted: %+v", got.Metadata)
	}

	got.Metadata.MergedCleanupPending = false
	got.Metadata.MergedCleanupPRURL = ""
	if err := s.UpdateSession(ctx, got); err != nil {
		t.Fatal(err)
	}
	cleared, _, _ := s.GetSession(ctx, r.ID)
	if cleared.Metadata.MergedCleanupPending || cleared.Metadata.MergedCleanupPRURL != "" {
		t.Fatalf("merged cleanup replay state was not cleared: %+v", cleared.Metadata)
	}
}

func TestSessionPendingSubmitLatchRoundTripAndAtomicClaim(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	r, err := s.CreateSession(ctx, sampleRecord("mer"))
	if err != nil {
		t.Fatal(err)
	}

	fingerprint := "sha256-prompt"
	stamp := r.UpdatedAt.Add(time.Second)
	if ok, err := s.SetPendingSubmit(ctx, r.ID, fingerprint, stamp); err != nil || !ok {
		t.Fatalf("SetPendingSubmit = %v, %v; want true, nil", ok, err)
	}
	got, _, _ := s.GetSession(ctx, r.ID)
	if got.Metadata.PendingSubmitFingerprint != fingerprint || got.Metadata.PendingSubmitRecoveryAttempted {
		t.Fatalf("latched metadata = %+v", got.Metadata)
	}

	if ok, err := s.ClaimPendingSubmitRecovery(ctx, r.ID, fingerprint, stamp.Add(time.Second)); err != nil || !ok {
		t.Fatalf("first ClaimPendingSubmitRecovery = %v, %v; want true, nil", ok, err)
	}
	if ok, err := s.ClaimPendingSubmitRecovery(ctx, r.ID, fingerprint, stamp.Add(2*time.Second)); err != nil || ok {
		t.Fatalf("second ClaimPendingSubmitRecovery = %v, %v; want false, nil", ok, err)
	}
	got, _, _ = s.GetSession(ctx, r.ID)
	if !got.Metadata.PendingSubmitRecoveryAttempted {
		t.Fatal("recovery claim was not persisted")
	}

	// A stale confirmation cannot clear a different prompt's newer latch.
	if ok, err := s.ClearPendingSubmit(ctx, r.ID, "wrong", stamp.Add(3*time.Second)); err != nil || ok {
		t.Fatalf("stale ClearPendingSubmit = %v, %v; want false, nil", ok, err)
	}
	if ok, err := s.ClearPendingSubmit(ctx, r.ID, fingerprint, stamp.Add(4*time.Second)); err != nil || !ok {
		t.Fatalf("ClearPendingSubmit = %v, %v; want true, nil", ok, err)
	}
	got, _, _ = s.GetSession(ctx, r.ID)
	if got.Metadata.PendingSubmitFingerprint != "" || got.Metadata.PendingSubmitRecoveryAttempted {
		t.Fatalf("cleared metadata = %+v", got.Metadata)
	}
}

func TestSessionTerminalOrAutomationBlockedUpdateClearsPendingSubmitLatch(t *testing.T) {
	for _, tc := range []struct {
		name       string
		activity   domain.ActivityState
		terminated bool
	}{
		{name: "blocked", activity: domain.ActivityBlocked},
		{name: "rate limited", activity: domain.ActivityRateLimited},
		{name: "terminated", activity: domain.ActivityExited, terminated: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			ctx := context.Background()
			seedProject(t, s, "mer")
			r, _ := s.CreateSession(ctx, sampleRecord("mer"))
			_, _ = s.SetPendingSubmit(ctx, r.ID, "sha256-prompt", r.UpdatedAt.Add(time.Second))

			got, _, _ := s.GetSession(ctx, r.ID)
			got.Activity.State = tc.activity
			got.IsTerminated = tc.terminated
			got.UpdatedAt = got.UpdatedAt.Add(2 * time.Second)
			if err := s.UpdateSession(ctx, got); err != nil {
				t.Fatal(err)
			}
			got, _, _ = s.GetSession(ctx, r.ID)
			if got.Metadata.PendingSubmitFingerprint != "" || got.Metadata.PendingSubmitRecoveryAttempted {
				t.Fatalf("terminal metadata = %+v, want latch cleared", got.Metadata)
			}
		})
	}
}

func TestPRCRUD(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	r, _ := s.CreateSession(ctx, sampleRecord("mer"))
	now := time.Now().UTC().Truncate(time.Second)

	pr := domain.PullRequest{
		URL: "https://gh/pr/1", SessionID: r.ID, Number: 1,
		Review: domain.ReviewRequired, CI: domain.CIFailing, Mergeability: domain.MergeBlocked, UpdatedAt: now,
	}
	if err := s.WritePR(ctx, pr, nil, nil); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.GetPR(ctx, pr.URL)
	if err != nil || !ok || got != pr {
		t.Fatalf("get pr: ok=%v err=%v got=%+v", ok, err, got)
	}
	if list, _ := s.ListPRsBySession(ctx, r.ID); len(list) != 1 {
		t.Fatalf("list prs = %d, want 1", len(list))
	}
}

func TestWritePRRejectsSessionReassignment(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	first, _ := s.CreateSession(ctx, sampleRecord("mer"))
	second, _ := s.CreateSession(ctx, sampleRecord("mer"))
	now := time.Now().UTC().Truncate(time.Second)

	pr := domain.PullRequest{URL: "https://gh/pr/1", SessionID: first.ID, Number: 1, UpdatedAt: now}
	if err := s.WritePR(ctx, pr, nil, nil); err != nil {
		t.Fatal(err)
	}
	pr.SessionID = second.ID
	if err := s.WritePR(ctx, pr, nil, nil); err == nil {
		t.Fatal("expected reassignment to fail")
	}
	got, ok, err := s.GetPR(ctx, pr.URL)
	if err != nil || !ok {
		t.Fatalf("get pr: ok=%v err=%v", ok, err)
	}
	if got.SessionID != first.ID {
		t.Fatalf("pr moved to %s, want %s", got.SessionID, first.ID)
	}
}

func TestDisplayPRFactsPrefersActivePR(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	r, _ := s.CreateSession(ctx, sampleRecord("mer"))
	now := time.Now().UTC().Truncate(time.Second)

	if err := s.WritePR(ctx, domain.PullRequest{URL: "closed", SessionID: r.ID, Number: 1, Closed: true, UpdatedAt: now.Add(time.Minute)}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.WritePR(ctx, domain.PullRequest{URL: "open", SessionID: r.ID, Number: 2, CI: domain.CIFailing, UpdatedAt: now}, nil, nil); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.GetDisplayPRFactsForSession(ctx, r.ID)
	if err != nil || !ok {
		t.Fatalf("display pr: ok=%v err=%v", ok, err)
	}
	if got.URL != "open" || got.CI != domain.CIFailing {
		t.Fatalf("display pr = %+v", got)
	}
}

func TestPRCommentsReplace(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	r, _ := s.CreateSession(ctx, sampleRecord("mer"))
	now := time.Now().UTC().Truncate(time.Second)
	_ = s.WritePR(ctx, domain.PullRequest{URL: "pr1", SessionID: r.ID, UpdatedAt: now}, nil, []domain.PullRequestComment{
		{ID: "c1", Author: "a", File: "a.go", Line: 1, Body: "nit", CreatedAt: now},
		{ID: "c2", Author: "b", File: "b.go", Line: 2, Body: "bug", Resolved: true, CreatedAt: now.Add(time.Second)},
	})
	if list, _ := s.ListPRComments(ctx, "pr1"); len(list) != 2 {
		t.Fatalf("comments = %d, want 2", len(list))
	}
	// replace with a smaller set drops the rest.
	_ = s.WritePR(ctx, domain.PullRequest{URL: "pr1", SessionID: r.ID, UpdatedAt: now}, nil, []domain.PullRequestComment{{ID: "c1", Body: "x", CreatedAt: now}})
	if list, _ := s.ListPRComments(ctx, "pr1"); len(list) != 1 {
		t.Fatalf("after replace, comments = %d, want 1", len(list))
	}
}

func TestWriteSCMObservationPersistsMetadataChecksReviewsAndComments(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	r, _ := s.CreateSession(ctx, sampleRecord("mer"))
	now := time.Now().UTC().Truncate(time.Second)

	pr := domain.PullRequest{
		URL: "https://github.com/o/r/pull/1", SessionID: r.ID, Number: 1,
		Provider: "github", Host: "github.com", Repo: "o/r",
		SourceBranch: "feat/75", TargetBranch: "main", HeadSHA: "h1",
		Title: "SCM observer", Additions: 10, Deletions: 2, ChangedFiles: 3,
		Author: "dev", BaseSHA: "b1", MergeCommitSHA: "m1",
		ProviderState: "OPEN", ProviderMergeable: "MERGEABLE", ProviderMergeStateStatus: "CLEAN",
		HTMLURL: "https://github.com/o/r/pull/1",
		CI:      domain.CIFailing, Review: domain.ReviewChangesRequest, Mergeability: domain.MergeBlocked,
		MetadataHash: "mh", CIHash: "ch", ReviewHash: "rh",
		UpdatedAt: now, ObservedAt: now, CIObservedAt: now, ReviewObservedAt: now,
	}
	checks := []domain.PullRequestCheck{{Name: "build", CommitHash: "h1", Status: domain.PRCheckFailed, Conclusion: "failure", URL: "ci", Details: "99", LogTail: "boom", CreatedAt: now}}
	reviews := []domain.PullRequestReview{{ID: "review-1", Author: "reviewer", State: domain.ReviewChangesRequest, URL: "https://github.com/o/r/pull/1#pullrequestreview-1", SubmittedAt: now}}
	threads := []domain.PullRequestReviewThread{{ThreadID: "t1", Path: "main.go", Line: 7, SemanticHash: "th", UpdatedAt: now}}
	comments := []domain.PullRequestComment{{ThreadID: "t1", ID: "c1", Author: "reviewer", File: "main.go", Line: 7, Body: "fix", URL: "comment", CreatedAt: now}}

	if err := s.WriteSCMObservation(ctx, pr, checks, reviews, threads, comments, ports.ReviewWriteReplace); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.GetPR(ctx, pr.URL)
	if err != nil || !ok {
		t.Fatalf("get pr: ok=%v err=%v", ok, err)
	}
	if got.Provider != "github" || got.HeadSHA != "h1" || got.MetadataHash != "mh" || got.CIHash != "ch" || got.ReviewHash != "rh" {
		t.Fatalf("SCM metadata not persisted: %+v", got)
	}
	gotChecks, _ := s.ListChecks(ctx, pr.URL)
	if len(gotChecks) != 1 || gotChecks[0].Conclusion != "failure" || gotChecks[0].Details != "99" || gotChecks[0].LogTail != "boom" {
		t.Fatalf("checks not persisted: %+v", gotChecks)
	}
	gotThreads, _ := s.ListPRReviewThreads(ctx, pr.URL)
	if len(gotThreads) != 1 || gotThreads[0].ThreadID != "t1" || gotThreads[0].SemanticHash != "th" {
		t.Fatalf("threads not persisted: %+v", gotThreads)
	}
	gotReviews, _ := s.ListPRReviews(ctx, pr.URL)
	if len(gotReviews) != 1 || gotReviews[0].ID != "review-1" || gotReviews[0].URL != "https://github.com/o/r/pull/1#pullrequestreview-1" {
		t.Fatalf("reviews not persisted: %+v", gotReviews)
	}
	gotComments, _ := s.ListPRComments(ctx, pr.URL)
	if len(gotComments) != 1 || gotComments[0].ThreadID != "t1" || gotComments[0].URL != "comment" {
		t.Fatalf("comments not persisted: %+v", gotComments)
	}
}

func TestWriteSCMObservationMergeUpdatesFetchedReviewWindow(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	r, _ := s.CreateSession(ctx, sampleRecord("mer"))
	now := time.Now().UTC().Truncate(time.Second)
	pr := domain.PullRequest{URL: "https://github.com/o/r/pull/1", SessionID: r.ID, Number: 1, UpdatedAt: now}

	initialThreads := []domain.PullRequestReviewThread{
		{ThreadID: "older", Path: "old.go", Line: 1, Resolved: false, SemanticHash: "old", UpdatedAt: now},
		{ThreadID: "latest", Path: "main.go", Line: 7, Resolved: false, SemanticHash: "latest-v1", UpdatedAt: now},
	}
	initialComments := []domain.PullRequestComment{
		{ThreadID: "older", ID: "older-c1", Author: "ann", Body: "old", CreatedAt: now},
		{ThreadID: "latest", ID: "latest-c1", Author: "bob", Body: "before", CreatedAt: now},
	}
	if err := s.WriteSCMObservation(ctx, pr, nil, nil, initialThreads, initialComments, ports.ReviewWriteReplace); err != nil {
		t.Fatal(err)
	}

	mergedThreads := []domain.PullRequestReviewThread{
		{ThreadID: "latest", Path: "main.go", Line: 8, Resolved: true, SemanticHash: "latest-v2", UpdatedAt: now.Add(time.Second)},
		{ThreadID: "new", Path: "new.go", Line: 2, Resolved: false, SemanticHash: "new", UpdatedAt: now.Add(time.Second)},
	}
	mergedComments := []domain.PullRequestComment{
		{ThreadID: "latest", ID: "latest-c2", Author: "bob", Body: "after", CreatedAt: now.Add(time.Second)},
		{ThreadID: "new", ID: "new-c1", Author: "cat", Body: "new", CreatedAt: now.Add(time.Second)},
	}
	if err := s.WriteSCMObservation(ctx, pr, nil, nil, mergedThreads, mergedComments, ports.ReviewWriteMerge); err != nil {
		t.Fatal(err)
	}

	gotThreads, err := s.ListPRReviewThreads(ctx, pr.URL)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotThreads) != 3 {
		t.Fatalf("threads = %#v, want older preserved plus latest/new", gotThreads)
	}
	byThread := map[string]domain.PullRequestReviewThread{}
	for _, th := range gotThreads {
		byThread[th.ThreadID] = th
	}
	if byThread["older"].SemanticHash != "old" {
		t.Fatalf("older thread not preserved: %#v", byThread["older"])
	}
	if byThread["latest"].SemanticHash != "latest-v2" || !byThread["latest"].Resolved || byThread["latest"].Line != 8 {
		t.Fatalf("latest thread not updated: %#v", byThread["latest"])
	}
	if byThread["new"].SemanticHash != "new" {
		t.Fatalf("new thread not inserted: %#v", byThread["new"])
	}

	gotComments, err := s.ListPRComments(ctx, pr.URL)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, c := range gotComments {
		ids[c.ID] = true
	}
	if !ids["older-c1"] || !ids["latest-c2"] || !ids["new-c1"] {
		t.Fatalf("comments after merge = %#v, want older preserved and fetched threads replaced", gotComments)
	}
	if ids["latest-c1"] {
		t.Fatalf("stale fetched-thread comment was preserved: %#v", gotComments)
	}
}

func TestWritePRPreservesSCMReviewThreads(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	r, _ := s.CreateSession(ctx, sampleRecord("mer"))
	now := time.Now().UTC().Truncate(time.Second)
	observedAt := now.Add(-time.Minute)
	pr := domain.PullRequest{
		URL: "https://github.com/o/r/pull/1", SessionID: r.ID, Number: 1, UpdatedAt: now,
		Provider: "github", Host: "github.com", Repo: "o/r", HeadSHA: "head-1",
		MetadataHash: "metadata-v1", CIHash: "ci-v1", ReviewHash: "review-v1",
		ObservedAt: observedAt, CIObservedAt: observedAt, ReviewObservedAt: observedAt,
	}
	threads := []domain.PullRequestReviewThread{{ThreadID: "t1", Path: "main.go", Line: 7, SemanticHash: "thread-v1", UpdatedAt: now}}
	comments := []domain.PullRequestComment{{ThreadID: "t1", ID: "c1", Author: "reviewer", Body: "scm", URL: "https://example/comment/c1", CreatedAt: now}}

	if err := s.WriteSCMObservation(ctx, pr, nil, nil, threads, comments, ports.ReviewWriteReplace); err != nil {
		t.Fatal(err)
	}
	legacyComments := []domain.PullRequestComment{
		{ID: "c1", Author: "legacy", Body: "duplicate legacy row must not clear thread metadata", CreatedAt: now.Add(time.Second)},
		{ID: "legacy-only", Author: "legacy", Body: "legacy", CreatedAt: now.Add(time.Second)},
	}
	if err := s.WritePR(ctx, domain.PullRequest{URL: pr.URL, SessionID: r.ID, Number: 1, CI: domain.CIPassing, UpdatedAt: now.Add(time.Second)}, nil, legacyComments); err != nil {
		t.Fatal(err)
	}

	gotPR, ok, err := s.GetPR(ctx, pr.URL)
	if err != nil || !ok {
		t.Fatalf("get pr: ok=%v err=%v", ok, err)
	}
	if gotPR.Provider != "github" || gotPR.Host != "github.com" || gotPR.Repo != "o/r" || gotPR.HeadSHA != "head-1" ||
		gotPR.MetadataHash != "metadata-v1" || gotPR.CIHash != "ci-v1" || gotPR.ReviewHash != "review-v1" {
		t.Fatalf("legacy WritePR must preserve SCM-owned metadata and hashes, got %+v", gotPR)
	}
	if !gotPR.ObservedAt.Equal(observedAt) || !gotPR.CIObservedAt.Equal(observedAt) || !gotPR.ReviewObservedAt.Equal(observedAt) {
		t.Fatalf("legacy WritePR must preserve SCM observation timestamps, got observed=%s ci=%s review=%s", gotPR.ObservedAt, gotPR.CIObservedAt, gotPR.ReviewObservedAt)
	}
	if gotPR.CI != domain.CIPassing {
		t.Fatalf("legacy WritePR should still update legacy scalar CI state, got %s", gotPR.CI)
	}
	gotThreads, err := s.ListPRReviewThreads(ctx, pr.URL)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotThreads) != 1 || gotThreads[0].ThreadID != "t1" || gotThreads[0].SemanticHash != "thread-v1" {
		t.Fatalf("legacy WritePR must preserve SCM-owned review threads, got %+v", gotThreads)
	}
	gotComments, err := s.ListPRComments(ctx, pr.URL)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]domain.PullRequestComment{}
	for _, c := range gotComments {
		byID[c.ID] = c
	}
	scmComment, ok := byID["c1"]
	if !ok || scmComment.ThreadID != "t1" || scmComment.URL != "https://example/comment/c1" || scmComment.Body != "scm" {
		t.Fatalf("legacy WritePR must not clear SCM comment metadata, got %+v", scmComment)
	}
	legacyOnly, ok := byID["legacy-only"]
	if !ok || legacyOnly.ThreadID != "" {
		t.Fatalf("legacy-only comment should remain unthreaded, got %+v", legacyOnly)
	}

	mergedThreads := []domain.PullRequestReviewThread{{ThreadID: "t1", Path: "main.go", Line: 8, Resolved: true, SemanticHash: "thread-v2", UpdatedAt: now.Add(2 * time.Second)}}
	mergedComments := []domain.PullRequestComment{{ThreadID: "t1", ID: "c2", Author: "reviewer", Body: "updated", URL: "https://example/comment/c2", CreatedAt: now.Add(2 * time.Second)}}
	if err := s.WriteSCMObservation(ctx, pr, nil, nil, mergedThreads, mergedComments, ports.ReviewWriteMerge); err != nil {
		t.Fatal(err)
	}
	gotComments, err = s.ListPRComments(ctx, pr.URL)
	if err != nil {
		t.Fatal(err)
	}
	byID = map[string]domain.PullRequestComment{}
	for _, c := range gotComments {
		byID[c.ID] = c
	}
	if _, ok := byID["c1"]; ok {
		t.Fatalf("SCM merge should delete stale fetched-thread comment c1, comments=%+v", gotComments)
	}
	replacement, ok := byID["c2"]
	if !ok || replacement.ThreadID != "t1" {
		t.Fatalf("SCM merge did not insert replacement threaded comment, comments=%+v", gotComments)
	}
}

func TestWritePRReplacesLegacyCommentBodies(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	r, _ := s.CreateSession(ctx, sampleRecord("mer"))
	now := time.Now().UTC().Truncate(time.Second)
	pr := domain.PullRequest{URL: "https://github.com/o/r/pull/2", SessionID: r.ID, Number: 2, UpdatedAt: now}

	if err := s.WritePR(ctx, pr, nil, []domain.PullRequestComment{{ID: "legacy", Author: "reviewer", Body: "before", CreatedAt: now}}); err != nil {
		t.Fatal(err)
	}
	if err := s.WritePR(ctx, pr, nil, []domain.PullRequestComment{{ID: "legacy", Author: "reviewer", Body: "after edit", CreatedAt: now.Add(time.Second)}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListPRComments(ctx, pr.URL)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Body != "after edit" || !got[0].CreatedAt.Equal(now.Add(time.Second)) {
		t.Fatalf("legacy comment replacement did not persist edited row: %+v", got)
	}
}

func TestCDCTriggersPopulateChangeLog(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")

	r, _ := s.CreateSession(ctx, sampleRecord("mer"))
	// a real state change logs; a metadata-only change does not (WHEN guard).
	r.Activity.State = domain.ActivityIdle
	_ = s.UpdateSession(ctx, r)
	r.Metadata.Prompt = "only metadata changed"
	_ = s.UpdateSession(ctx, r)
	// a PR insert logs too.
	_ = s.WritePR(ctx, domain.PullRequest{URL: "pr1", SessionID: r.ID, UpdatedAt: r.UpdatedAt}, nil, nil)

	evs, err := s.EventsAfter(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, e := range evs {
		if e.ProjectID != "mer" {
			t.Fatalf("event project = %s, want mer", e.ProjectID)
		}
		types = append(types, string(e.Type))
	}
	want := []string{"session_created", "session_updated", "pr_created"}
	if len(types) != 3 || types[0] != want[0] || types[1] != want[1] || types[2] != want[2] {
		t.Fatalf("change_log event types = %v, want %v (metadata-only update suppressed)", types, want)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(evs[0].Payload), &payload); err != nil {
		t.Fatalf("session payload JSON: %v", err)
	}
	if _, ok := payload["isTerminated"].(bool); !ok {
		t.Fatalf("isTerminated payload type = %T, want bool", payload["isTerminated"])
	}
	maxSeq, _ := s.LatestSeq(ctx)
	if maxSeq != int64(len(evs)) {
		t.Fatalf("max seq = %d, want %d", maxSeq, len(evs))
	}
}

func TestSetSessionPreviewURLBumpsRevisionAndFiresCDCOnSameURL(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	r, _ := s.CreateSession(ctx, sampleRecord("mer"))

	base, _ := s.LatestSeq(ctx)
	now := time.Now().UTC()
	for i := 0; i < 2; i++ {
		ok, err := s.SetSessionPreviewURL(ctx, r.ID, "http://localhost:5173/", now.Add(time.Duration(i)*time.Second))
		if err != nil || !ok {
			t.Fatalf("set preview url (call %d): ok=%v err=%v", i, ok, err)
		}
	}

	got, found, err := s.GetSession(ctx, r.ID)
	if err != nil || !found {
		t.Fatalf("get session: found=%v err=%v", found, err)
	}
	if got.Metadata.PreviewURL != "http://localhost:5173/" {
		t.Fatalf("preview url = %q, want persisted target", got.Metadata.PreviewURL)
	}
	if got.Metadata.PreviewRevision != 2 {
		t.Fatalf("preview revision = %d, want 2 after two sets", got.Metadata.PreviewRevision)
	}

	// Both sets fire session_updated even though the URL never changed — the
	// revision bump is what trips the trigger, so a same-URL `ao preview` re-run
	// still reaches the browser panel.
	evs, err := s.EventsAfter(ctx, base, 100)
	if err != nil {
		t.Fatal(err)
	}
	updates := 0
	for _, e := range evs {
		if string(e.Type) == "session_updated" {
			updates++
		}
	}
	if updates != 2 {
		t.Fatalf("session_updated events = %d, want 2 (one per same-URL set)", updates)
	}
}

func TestConcurrentSessionCreateAssignsUniqueNums(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")

	const n = 20
	var wg sync.WaitGroup
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := s.CreateSession(ctx, sampleRecord("mer"))
			if err != nil {
				t.Errorf("create: %v", err)
				return
			}
			ids[i] = string(r.ID)
		}(i)
	}
	wg.Wait()

	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" || seen[id] {
			t.Fatalf("duplicate or empty id: %q in %v", id, ids)
		}
		seen[id] = true
	}
	if all, _ := s.ListAllSessions(ctx); len(all) != n {
		t.Fatalf("created %d sessions, want %d", len(all), n)
	}
}

func TestSessionWorktreesRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "ws")
	rec, err := s.CreateSession(ctx, sampleRecord("ws"))
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	rows := []domain.SessionWorktreeRecord{
		{SessionID: rec.ID, RepoName: domain.RootWorkspaceRepoName, RepoPath: ptrTo("/repos/ws"), RelativePath: ptrTo(""), Branch: "ao/ws-1", BaseSHA: "root-base", WorktreePath: "/managed/ws/ws-1", State: "active"},
		{SessionID: rec.ID, RepoName: "api", RepoPath: ptrTo("/repos/ws/api"), RelativePath: ptrTo("services/api"), Branch: "ao/ws-1", BaseSHA: "api-base", WorktreePath: "/managed/ws/ws-1/api", PreservedRef: "refs/ao/preserved/ws-1", State: "removed"},
	}
	for _, row := range rows {
		if err := s.UpsertSessionWorktree(ctx, row); err != nil {
			t.Fatalf("upsert worktree %s: %v", row.RepoName, err)
		}
	}
	got, err := s.ListSessionWorktrees(ctx, rec.ID)
	if err != nil {
		t.Fatalf("list worktrees: %v", err)
	}
	if !reflect.DeepEqual(got, rows) {
		t.Fatalf("worktrees = %#v, want %#v", got, rows)
	}
	one, ok, err := s.GetSessionWorktree(ctx, rec.ID, "api")
	if err != nil || !ok || one.PreservedRef != "refs/ao/preserved/ws-1" {
		t.Fatalf("get api = %#v ok=%v err=%v", one, ok, err)
	}
	rows[1].State = "active"
	rows[1].PreservedRef = ""
	rows[1].RepoPath = ptrTo("/repos/re-registered/api")
	rows[1].RelativePath = ptrTo("api-now")
	if err := s.UpsertSessionWorktree(ctx, rows[1]); err != nil {
		t.Fatalf("update api worktree: %v", err)
	}
	one, ok, err = s.GetSessionWorktree(ctx, rec.ID, "api")
	if err != nil || !ok || one.State != "active" || one.PreservedRef != "" || one.RepoPath == nil || *one.RepoPath != "/repos/ws/api" || one.RelativePath == nil || *one.RelativePath != "services/api" {
		t.Fatalf("updated api = %#v ok=%v err=%v", one, ok, err)
	}
	if err := s.DeleteSessionWorktrees(ctx, rec.ID); err != nil {
		t.Fatalf("delete worktrees: %v", err)
	}
	got, err = s.ListSessionWorktrees(ctx, rec.ID)
	if err != nil || len(got) != 0 {
		t.Fatalf("after delete = %#v err=%v", got, err)
	}
}

// TestUpsertSessionWorktreeEmptyStateDefaultsToActive exercises the guard in
// UpsertSessionWorktree: when State is left at its zero value "", the store
// must default it to "active" so the SQLite CHECK constraint is satisfied.
// Without the guard, the generated upsert would insert "" and the CHECK would
// reject it. This test catches any regression that removes that guard.
func TestUpsertSessionWorktreeEmptyStateDefaultsToActive(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "sw")
	rec, err := s.CreateSession(ctx, sampleRecord("sw"))
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// State is intentionally left at zero value "" to exercise the guard.
	row := domain.SessionWorktreeRecord{
		SessionID:    rec.ID,
		RepoName:     domain.RootWorkspaceRepoName,
		Branch:       "ao/sw-1",
		BaseSHA:      "abc123",
		WorktreePath: "/managed/sw/sw-1",
	}
	if err := s.UpsertSessionWorktree(ctx, row); err != nil {
		t.Fatalf("upsert with empty State: %v", err)
	}

	got, ok, err := s.GetSessionWorktree(ctx, rec.ID, domain.RootWorkspaceRepoName)
	if err != nil {
		t.Fatalf("get worktree: %v", err)
	}
	if !ok {
		t.Fatal("worktree row not found after upsert")
	}
	if got.State != "active" {
		t.Fatalf("State = %q, want %q", got.State, "active")
	}
	if got.RepoPath != nil || got.RelativePath != nil {
		t.Fatalf("legacy metadata = %v/%v, want nil", got.RepoPath, got.RelativePath)
	}
}

func ptrTo[T any](value T) *T { return &value }
