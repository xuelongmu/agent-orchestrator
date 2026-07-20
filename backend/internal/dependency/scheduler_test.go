package dependency

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

type schedulerStore struct {
	mu              sync.Mutex
	ready           []domain.SessionID
	tokens          map[domain.SessionID]string
	promoted        map[domain.SessionID]bool
	handoffs        map[domain.SessionID][]domain.DependencyHandoff
	reserveClaimed  chan struct{}
	reserveContinue <-chan struct{}
	recoverCalls    int
	readyCalls      int
	staleCalls      int
}

func (s *schedulerStore) ListReadyDependencySessions(context.Context) ([]domain.SessionID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.readyCalls++
	var ready []domain.SessionID
	for _, id := range s.ready {
		if !s.promoted[id] && s.tokens[id] == "" {
			ready = append(ready, id)
		}
	}
	return ready, nil
}
func (s *schedulerStore) ListDependencyHandoffs(_ context.Context, id domain.SessionID) ([]domain.DependencyHandoff, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]domain.DependencyHandoff(nil), s.handoffs[id]...), nil
}
func (s *schedulerStore) ReserveDependencyPromotion(_ context.Context, id domain.SessionID, token string, _ time.Time) (bool, error) {
	s.mu.Lock()
	if s.promoted[id] || s.tokens[id] != "" {
		s.mu.Unlock()
		return false, nil
	}
	s.tokens[id] = token
	s.mu.Unlock()
	if s.reserveClaimed != nil {
		close(s.reserveClaimed)
		<-s.reserveContinue
	}
	return true, nil
}
func (s *schedulerStore) CompleteDependencyPromotion(_ context.Context, id domain.SessionID, token string, _ time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tokens[id] != token || s.promoted[id] {
		return false, nil
	}
	s.tokens[id] = ""
	s.promoted[id] = true
	return true, nil
}
func (s *schedulerStore) ReleaseDependencyPromotion(ctx context.Context, id domain.SessionID, token string, _ time.Time) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tokens[id] != token || s.promoted[id] {
		return false, nil
	}
	s.tokens[id] = ""
	return true, nil
}
func (s *schedulerStore) RecoverDependencyPromotions(context.Context, time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recoverCalls++
	var recovered int64
	for id, token := range s.tokens {
		if token != "" && !s.promoted[id] {
			s.tokens[id] = ""
			recovered++
		}
	}
	return recovered, nil
}

type recoveringSchedulerLauncher struct {
	*schedulerLauncher
	recoveryErr error
}

func (l *recoveringSchedulerLauncher) RecoverPromotedDependencyLaunches(context.Context) error {
	return l.recoveryErr
}

type blockingRecoveryLauncher struct {
	mu             sync.Mutex
	recoverCalls   int
	launchCalls    int
	launchStarted  chan struct{}
	launchContinue <-chan struct{}
}

func (l *blockingRecoveryLauncher) RecoverPromotedDependencyLaunches(context.Context) error {
	l.mu.Lock()
	l.recoverCalls++
	l.mu.Unlock()
	return nil
}

func (l *blockingRecoveryLauncher) LaunchPromoted(_ context.Context, id domain.SessionID, _ string, _ []domain.DependencyHandoff) (domain.SessionRecord, error) {
	l.mu.Lock()
	l.launchCalls++
	l.mu.Unlock()
	close(l.launchStarted)
	<-l.launchContinue
	return domain.SessionRecord{ID: id}, nil
}
func (s *schedulerStore) RecoverStaleDependencyPromotions(context.Context, time.Time, time.Time) (int64, error) {
	s.mu.Lock()
	s.staleCalls++
	s.mu.Unlock()
	return 0, nil
}

type schedulerLauncher struct {
	mu       sync.Mutex
	ids      []domain.SessionID
	handoffs [][]domain.DependencyHandoff
}

func (l *schedulerLauncher) LaunchPromoted(_ context.Context, id domain.SessionID, _ string, handoffs []domain.DependencyHandoff) (domain.SessionRecord, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ids = append(l.ids, id)
	l.handoffs = append(l.handoffs, handoffs)
	return domain.SessionRecord{ID: id}, nil
}

type cancellingLauncher struct{ cancel context.CancelFunc }

func (l cancellingLauncher) LaunchPromoted(ctx context.Context, _ domain.SessionID, _ string, _ []domain.DependencyHandoff) (domain.SessionRecord, error) {
	l.cancel()
	<-ctx.Done()
	return domain.SessionRecord{}, ctx.Err()
}

type retainedReservationError struct{}

func (retainedReservationError) Error() string                     { return "cleanup incomplete" }
func (retainedReservationError) RetainDependencyReservation() bool { return true }

type retainedFailureLauncher struct{}

func (retainedFailureLauncher) LaunchPromoted(context.Context, domain.SessionID, string, []domain.DependencyHandoff) (domain.SessionRecord, error) {
	return domain.SessionRecord{}, retainedReservationError{}
}

var _ error = retainedReservationError{}

type failNLauncher struct {
	mu        sync.Mutex
	remaining int
	attempts  int
}

func (l *failNLauncher) LaunchPromoted(_ context.Context, id domain.SessionID, _ string, _ []domain.DependencyHandoff) (domain.SessionRecord, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.attempts++
	if l.remaining > 0 {
		l.remaining--
		return domain.SessionRecord{}, errors.New("transient launch failure")
	}
	return domain.SessionRecord{ID: id}, nil
}

func TestReconcilePromotesReadyChildExactlyOnceWithParentHandoffs(t *testing.T) {
	parent := domain.AgentHandoff{ChangedFiles: []string{"parent.go"}, VerificationCommands: []string{"go test ./parent"}, ResidualRisk: "none"}
	store := &schedulerStore{
		ready:    []domain.SessionID{"ao-2"},
		tokens:   make(map[domain.SessionID]string),
		promoted: make(map[domain.SessionID]bool),
		handoffs: map[domain.SessionID][]domain.DependencyHandoff{
			"ao-2": {{SessionID: "ao-1", Handoff: &parent}},
		},
	}
	launcher := &schedulerLauncher{}
	scheduler := New(store, launcher, func() time.Time { return time.Unix(1, 0).UTC() }, nil)
	if err := scheduler.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := scheduler.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(launcher.ids, []domain.SessionID{"ao-2"}) {
		t.Fatalf("launched ids = %#v", launcher.ids)
	}
	if !reflect.DeepEqual(launcher.handoffs[0], store.handoffs["ao-2"]) {
		t.Fatalf("handoffs = %#v", launcher.handoffs[0])
	}
}

func TestReconcileSnapshotsHandoffAfterPromotionReservation(t *testing.T) {
	claimed := make(chan struct{})
	resume := make(chan struct{})
	store := &schedulerStore{
		ready: []domain.SessionID{"ao-2"}, tokens: make(map[domain.SessionID]string), promoted: make(map[domain.SessionID]bool),
		handoffs: make(map[domain.SessionID][]domain.DependencyHandoff), reserveClaimed: claimed, reserveContinue: resume,
	}
	launcher := &schedulerLauncher{}
	done := make(chan error, 1)
	go func() { done <- New(store, launcher, nil, nil).Reconcile(context.Background()) }()
	<-claimed
	want := domain.DependencyHandoff{SessionID: "ao-1", Handoff: &domain.AgentHandoff{ChangedFiles: []string{"sealed-after-list.go"}}}
	store.mu.Lock()
	store.handoffs["ao-2"] = []domain.DependencyHandoff{want}
	store.mu.Unlock()
	close(resume)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(launcher.handoffs) != 1 || !reflect.DeepEqual(launcher.handoffs[0], []domain.DependencyHandoff{want}) {
		t.Fatalf("launch handoffs = %#v, want payload sealed before reservation returned", launcher.handoffs)
	}
}

func TestRecoveryDiagnosticDoesNotWedgeIndependentReadyPromotion(t *testing.T) {
	store := &schedulerStore{ready: []domain.SessionID{"ready-child"}, tokens: make(map[domain.SessionID]string), promoted: make(map[domain.SessionID]bool), handoffs: make(map[domain.SessionID][]domain.DependencyHandoff)}
	launcher := &recoveringSchedulerLauncher{schedulerLauncher: &schedulerLauncher{}, recoveryErr: errors.New("dirty recovered workspace")}
	err := New(store, launcher, nil, nil).Reconcile(context.Background())
	if err != nil {
		t.Fatalf("unrelated safely fenced recovery diagnostic escaped caller: %v", err)
	}
	if !reflect.DeepEqual(launcher.ids, []domain.SessionID{"ready-child"}) {
		t.Fatalf("independent ready child was wedged: launches=%v", launcher.ids)
	}
}

func TestRecoverAndConcurrentReconcileWaitForActiveLaunch(t *testing.T) {
	store := &schedulerStore{ready: []domain.SessionID{"ao-2"}, tokens: make(map[domain.SessionID]string), promoted: make(map[domain.SessionID]bool), handoffs: make(map[domain.SessionID][]domain.DependencyHandoff)}
	resume := make(chan struct{})
	launcher := &blockingRecoveryLauncher{launchStarted: make(chan struct{}), launchContinue: resume}
	scheduler := New(store, launcher, nil, nil)
	firstDone := make(chan error, 1)
	go func() { firstDone <- scheduler.Reconcile(context.Background()) }()
	<-launcher.launchStarted

	secondDone := make(chan error, 1)
	go func() { secondDone <- scheduler.Reconcile(context.Background()) }()
	recoverDone := make(chan error, 1)
	go func() { recoverDone <- scheduler.Recover(context.Background()) }()
	time.Sleep(50 * time.Millisecond)
	launcher.mu.Lock()
	recoveryCallsDuringLaunch := launcher.recoverCalls
	launcher.mu.Unlock()
	store.mu.Lock()
	bootRecoverCallsDuringLaunch := store.recoverCalls
	store.mu.Unlock()
	if recoveryCallsDuringLaunch != 1 || bootRecoverCallsDuringLaunch != 0 {
		t.Fatalf("active launch raced recovery: periodic=%d boot=%d", recoveryCallsDuringLaunch, bootRecoverCallsDuringLaunch)
	}

	close(resume)
	for _, ch := range []<-chan error{firstDone, secondDone, recoverDone} {
		if err := <-ch; err != nil {
			t.Fatal(err)
		}
	}
	launcher.mu.Lock()
	defer launcher.mu.Unlock()
	if launcher.launchCalls != 1 || launcher.recoverCalls != 2 {
		t.Fatalf("serialized calls: launches=%d periodic recovery=%d", launcher.launchCalls, launcher.recoverCalls)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.recoverCalls != 1 || !store.promoted["ao-2"] {
		t.Fatalf("boot recovery did not run after launch: calls=%d promoted=%v", store.recoverCalls, store.promoted)
	}
}

func TestPreCancelledLifetimeMakesNoReconcileCalls(t *testing.T) {
	store := &schedulerStore{ready: []domain.SessionID{"ao-2"}, tokens: make(map[domain.SessionID]string), promoted: make(map[domain.SessionID]bool), handoffs: make(map[domain.SessionID][]domain.DependencyHandoff)}
	launcher := &schedulerLauncher{}
	scheduler := New(store, launcher, nil, nil)
	lifetime, cancel := context.WithCancel(context.Background())
	cancel()
	scheduler.SetLifetimeContext(lifetime)
	if err := scheduler.Reconcile(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Reconcile error = %v, want canceled daemon lifetime", err)
	}
	store.mu.Lock()
	readyCalls, staleCalls := store.readyCalls, store.staleCalls
	store.mu.Unlock()
	if readyCalls != 0 || staleCalls != 0 || len(launcher.ids) != 0 {
		t.Fatalf("old owner crossed canceled lifetime: ready=%d stale=%d launches=%v", readyCalls, staleCalls, launcher.ids)
	}
}

func TestWakeDoesNotBlockBehindActivePromotion(t *testing.T) {
	store := &schedulerStore{ready: []domain.SessionID{"dir-child"}, tokens: make(map[domain.SessionID]string), promoted: make(map[domain.SessionID]bool), handoffs: make(map[domain.SessionID][]domain.DependencyHandoff)}
	resume := make(chan struct{})
	launcher := &blockingRecoveryLauncher{launchStarted: make(chan struct{}), launchContinue: resume}
	scheduler := New(store, launcher, nil, nil)
	done := make(chan error, 1)
	go func() { done <- scheduler.Reconcile(context.Background()) }()
	<-launcher.launchStarted
	woke := make(chan struct{})
	go func() { scheduler.Wake(); close(woke) }()
	select {
	case <-woke:
	case <-time.After(time.Second):
		t.Fatal("lifecycle wake blocked behind active directory promotion")
	}
	close(resume)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentReconcileLaunchesOnePromotion(t *testing.T) {
	store := &schedulerStore{ready: []domain.SessionID{"ao-2"}, tokens: make(map[domain.SessionID]string), promoted: make(map[domain.SessionID]bool), handoffs: map[domain.SessionID][]domain.DependencyHandoff{"ao-2": {}}}
	launcher := &schedulerLauncher{}
	first := New(store, launcher, nil, nil)
	second := New(store, launcher, nil, nil)
	var wg sync.WaitGroup
	for _, scheduler := range []*Scheduler{first, second} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := scheduler.Reconcile(context.Background()); err != nil {
				t.Errorf("Reconcile: %v", err)
			}
		}()
	}
	wg.Wait()
	launcher.mu.Lock()
	defer launcher.mu.Unlock()
	if !reflect.DeepEqual(launcher.ids, []domain.SessionID{"ao-2"}) {
		t.Fatalf("concurrent launches = %#v", launcher.ids)
	}
}

func TestRecoverAbandonedClaimAfterDaemonRestart(t *testing.T) {
	store := &schedulerStore{ready: []domain.SessionID{"ao-2"}, tokens: map[domain.SessionID]string{"ao-2": "dead-daemon"}, promoted: make(map[domain.SessionID]bool), handoffs: map[domain.SessionID][]domain.DependencyHandoff{"ao-2": {}}}
	launcher := &schedulerLauncher{}
	restarted := New(store, launcher, nil, nil)
	if err := restarted.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(launcher.ids, []domain.SessionID{"ao-2"}) {
		t.Fatalf("restart launches = %#v", launcher.ids)
	}
}

func TestCancelledLaunchReleasesReservationWithoutRestart(t *testing.T) {
	store := &schedulerStore{ready: []domain.SessionID{"ao-2"}, tokens: make(map[domain.SessionID]string), promoted: make(map[domain.SessionID]bool), handoffs: map[domain.SessionID][]domain.DependencyHandoff{"ao-2": {}}}
	ctx, cancel := context.WithCancel(context.Background())
	scheduler := New(store, cancellingLauncher{cancel: cancel}, nil, nil)
	if err := scheduler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	if store.tokens["ao-2"] != "" || store.promoted["ao-2"] {
		t.Fatalf("cancelled reservation leaked: tokens=%v promoted=%v", store.tokens, store.promoted)
	}
	store.mu.Unlock()
	retry := &schedulerLauncher{}
	if err := New(store, retry, nil, nil).Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(retry.ids, []domain.SessionID{"ao-2"}) {
		t.Fatalf("cancelled child was not retryable without restart: %v", retry.ids)
	}
}

func TestNilLauncherReleasesReservation(t *testing.T) {
	store := &schedulerStore{ready: []domain.SessionID{"ao-2"}, tokens: make(map[domain.SessionID]string), promoted: make(map[domain.SessionID]bool), handoffs: map[domain.SessionID][]domain.DependencyHandoff{"ao-2": {}}}
	scheduler := New(store, nil, nil, nil)
	if err := scheduler.Reconcile(context.Background()); err == nil {
		t.Fatal("expected missing launcher error")
	}
	store.mu.Lock()
	if store.tokens["ao-2"] != "" {
		t.Fatalf("nil launcher leaked reservation %q", store.tokens["ao-2"])
	}
	store.mu.Unlock()
	retry := &schedulerLauncher{}
	if err := New(store, retry, nil, nil).Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(retry.ids, []domain.SessionID{"ao-2"}) {
		t.Fatalf("nil-launcher child was not retryable without restart: %v", retry.ids)
	}
}

func TestIncompleteExternalCleanupRetainsReservationFence(t *testing.T) {
	store := &schedulerStore{ready: []domain.SessionID{"ao-2"}, tokens: make(map[domain.SessionID]string), promoted: make(map[domain.SessionID]bool), handoffs: map[domain.SessionID][]domain.DependencyHandoff{"ao-2": {}}}
	scheduler := New(store, retainedFailureLauncher{}, nil, nil)
	if err := scheduler.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.tokens["ao-2"] == "" || store.promoted["ao-2"] {
		t.Fatalf("cleanup-incomplete launch lost its fence: tokens=%v promoted=%v", store.tokens, store.promoted)
	}
}

func TestStartBacksOffTransientLaunchFailuresAndPromotesWithoutExternalSignal(t *testing.T) {
	store := &schedulerStore{ready: []domain.SessionID{"ao-2"}, tokens: make(map[domain.SessionID]string), promoted: make(map[domain.SessionID]bool), handoffs: make(map[domain.SessionID][]domain.DependencyHandoff)}
	launcher := &failNLauncher{remaining: 2}
	scheduler := New(store, launcher, nil, nil)
	delays := make(chan time.Duration, 8)
	ticks := make(chan struct{}, 8)
	scheduler.wait = func(ctx context.Context, delay time.Duration, _ <-chan struct{}, _ bool) bool {
		delays <- delay
		select {
		case <-ctx.Done():
			return false
		case <-ticks:
			return true
		}
	}
	loopCtx, cancel := context.WithCancel(context.Background())
	scheduler.SetLifetimeContext(loopCtx)
	done := scheduler.Start(loopCtx, time.Second, 4*time.Second)
	for i, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second} {
		if got := <-delays; got != want {
			t.Fatalf("delay[%d] = %s, want %s", i, got, want)
		}
		ticks <- struct{}{}
	}
	// Successful promotion resets the next steady-state poll to the minimum.
	if got := <-delays; got != time.Second {
		t.Fatalf("post-success delay = %s, want %s", got, time.Second)
	}
	cancel()
	<-done
	launcher.mu.Lock()
	attempts := launcher.attempts
	launcher.mu.Unlock()
	store.mu.Lock()
	promoted := store.promoted["ao-2"]
	token := store.tokens["ao-2"]
	store.mu.Unlock()
	if attempts != 3 || !promoted || token != "" {
		t.Fatalf("loop outcome: attempts=%d promoted=%v token=%q", attempts, promoted, token)
	}
}
