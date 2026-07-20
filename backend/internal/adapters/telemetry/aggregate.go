package telemetry

import (
	"context"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// AggregatingSink folds every occurrence of a bursty event in a flush window
// into a single rollup event carrying a count, so cost scales with flush
// windows (one event per name per window), not occurrences, and the
// true magnitude of a burst is still visible.
//
// PostHog bills per event ingested, regardless of what's in the payload. For
// event names that are prone to bursts (a 5xx storm, a panic loop, a retry
// hammering a bad command), per-occurrence rate limiting bounds the bill but
// throws away everything past the cap - a storm of 10,000 errors and one of
// 6 both show up as "5, then silence."
//
// Only event names named at construction are aggregated; everything else
// passes straight through to next unchanged, so per-occurrence semantics and
// RateLimitedSink's existing caps still apply to names this sink doesn't own.
type AggregatingSink struct {
	next       ports.EventSink
	names      map[string]struct{}
	flushEvery time.Duration

	mu        sync.Mutex
	buckets   map[string]*aggBucket
	closed    bool
	now       func() time.Time
	stop      chan struct{}
	loopDone  chan struct{}
	emitWG    sync.WaitGroup
	closeOnce sync.Once
	closeErr  error
}

type aggBucket struct {
	windowStart time.Time
	count       int
	sample      ports.TelemetryEvent
}

// NewAggregatingSink wraps next, folding occurrences of any event name in
// names into one rollup event per flushEvery window. Event names not in
// names are forwarded to next immediately, unaggregated. Starts a background
// flush loop; call Close to stop it and flush anything buffered.
func NewAggregatingSink(next ports.EventSink, names []string, flushEvery time.Duration) *AggregatingSink {
	nameSet := make(map[string]struct{}, len(names))
	for _, n := range names {
		nameSet[n] = struct{}{}
	}
	s := &AggregatingSink{
		next:       next,
		names:      nameSet,
		flushEvery: flushEvery,
		buckets:    make(map[string]*aggBucket),
		now:        time.Now,
		stop:       make(chan struct{}),
		loopDone:   make(chan struct{}),
	}
	go s.flushLoop()
	return s
}

// Emit forwards ev immediately if its name isn't aggregated; otherwise it is
// folded into the current window's bucket for that name and only reaches
// next as part of the next rollup.
func (s *AggregatingSink) Emit(ctx context.Context, ev ports.TelemetryEvent) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	if _, ok := s.names[ev.Name]; !ok || pollNeedsImmediateVisibility(ev) {
		// Add while holding mu so Close cannot begin waiting between the closed
		// check and Add. This joins passthrough emits before closing next.
		s.emitWG.Add(1)
		s.mu.Unlock()
		defer s.emitWG.Done()
		s.next.Emit(ctx, ev)
		return
	}

	b, ok := s.buckets[ev.Name]
	if !ok {
		b = &aggBucket{windowStart: s.now()}
		s.buckets[ev.Name] = b
	}
	b.count++
	b.sample = ev // last occurrence wins; only used for dims (level/source/ids), count/window carry the volume
	s.mu.Unlock()
}

func (s *AggregatingSink) flushLoop() {
	defer close(s.loopDone)
	ticker := time.NewTicker(s.flushEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.flush(context.Background())
		case <-s.stop:
			return
		}
	}
}

// flush emits one rollup event per non-empty bucket and clears the buckets.
func (s *AggregatingSink) flush(ctx context.Context) {
	s.mu.Lock()
	buckets := s.buckets
	s.buckets = make(map[string]*aggBucket)
	s.mu.Unlock()

	now := s.now().UTC()
	for name, b := range buckets {
		payload := make(map[string]any, len(b.sample.Payload)+3)
		for k, v := range b.sample.Payload {
			payload[k] = v
		}
		payload["count"] = b.count
		// Stringified (not time.Time): sanitizeRemoteValue's allowlist
		// filter only understands JSON scalar types (string/bool/number), so
		// a raw time.Time here would silently vanish before reaching
		// PostHog even once the field name is allowlisted.
		payload["window_start"] = b.windowStart.UTC().Format(time.RFC3339Nano)
		payload["window_end"] = now.Format(time.RFC3339Nano)

		s.next.Emit(ctx, ports.TelemetryEvent{
			Name:       name,
			Source:     b.sample.Source,
			OccurredAt: now,
			Level:      b.sample.Level,
			ProjectID:  b.sample.ProjectID,
			SessionID:  b.sample.SessionID,
			RequestID:  b.sample.RequestID,
			Payload:    payload,
		})
	}
}

// Close stops the flush loop, emits one final rollup for anything buffered
// since the last tick, and closes the wrapped sink.
func (s *AggregatingSink) Close(ctx context.Context) error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		close(s.stop)
		s.mu.Unlock()

		// The ticker may already be inside a blocking downstream Emit. Join it
		// before the final flush and before closing next, then join any immediate
		// passthrough emits that began before closed was set.
		<-s.loopDone
		s.emitWG.Wait()
		s.flush(ctx)
		s.closeErr = s.next.Close(ctx)
	})
	return s.closeErr
}
