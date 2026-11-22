package launch

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hugoalves-dev/go_discord_FrameRelay/internal/relaycontrol"
)

var (
	connected = relaycontrol.Event{Type: relaycontrol.EventConnected, Data: "{}"}
	heartbeat = relaycontrol.Event{Type: relaycontrol.EventHeartbeat, Data: "{}"}
)

func ready(id string) relaycontrol.Event {
	return relaycontrol.Event{Type: relaycontrol.EventSessionReady, ID: "1", Data: `{"intentId":"` + id + `"}`}
}

func TestEventLoopPublishesReadyIntentOnceDespiteDuplicateEvents(t *testing.T) {
	backend := newRecoveryBackend()
	publisher := &countingPublisher{}
	source := &scriptedSource{sessions: []session{
		{events: []relaycontrol.Event{connected}, then: func() { backend.makeReady("intent-1") },
			more: []relaycontrol.Event{ready("intent-1"), ready("intent-1"), heartbeat}, block: true},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	loop := fastLoop(source, NewWatcher(backend, publisher, nil))
	done := runLoop(ctx, loop)

	waitFor(t, func() bool { return backend.published("intent-1") })
	time.Sleep(50 * time.Millisecond) // let any duplicate-triggered reconcile run
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v", err)
	}
	if publisher.calls.Load() != 1 {
		t.Fatalf("published %d times, want exactly once", publisher.calls.Load())
	}
	if backend.listCalls.Load() > 4 {
		t.Fatalf("event-driven loop listed pending %d times", backend.listCalls.Load())
	}
}

func TestEventLoopRecoversIntentMissedWhileDisconnected(t *testing.T) {
	backend := newRecoveryBackend()
	publisher := &countingPublisher{}
	source := &scriptedSource{sessions: []session{
		{events: []relaycontrol.Event{connected}, err: errors.New("connection reset"),
			then: func() { backend.makeReady("intent-2") }}, // committed with no subscriber connected
		{events: []relaycontrol.Event{connected}, block: true},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runLoop(ctx, fastLoop(source, NewWatcher(backend, publisher, nil)))

	waitFor(t, func() bool { return backend.published("intent-2") })
	cancel()
	<-done
	if source.calls.Load() < 2 {
		t.Fatal("expected a reconnect")
	}
}

func TestEventLoopRetriesFailedPublicationWithoutNewEvents(t *testing.T) {
	backend := newRecoveryBackend()
	backend.makeReady("intent-3")
	publisher := &countingPublisher{failures: 2}
	source := &scriptedSource{sessions: []session{{events: []relaycontrol.Event{connected}, block: true}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runLoop(ctx, fastLoop(source, NewWatcher(backend, publisher, nil)))

	waitFor(t, func() bool { return backend.published("intent-3") })
	cancel()
	<-done
	if publisher.calls.Load() != 3 {
		t.Fatalf("publish attempts = %d, want 3", publisher.calls.Load())
	}
}

func TestEventLoopBacksOffOnInvalidCredentials(t *testing.T) {
	source := &scriptedSource{sessions: []session{{err: &relaycontrol.RequestError{StatusCode: 401}}}}
	health := &healthRecorder{}
	reconciler := &countingReconciler{}
	loop := fastLoop(source, reconciler)
	loop.SetHealthy = health.set
	loop.AuthBackoff = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := runLoop(ctx, loop)

	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done
	if source.calls.Load() != 1 {
		t.Fatalf("stream attempts = %d; 401 must not busy-retry", source.calls.Load())
	}
	if health.last() || reconciler.calls.Load() != 0 {
		t.Fatalf("healthy=%v reconciles=%d after auth failure", health.last(), reconciler.calls.Load())
	}
}

func TestEventLoopFallsBackToSlowPollingOnlyForUnsupportedBackend(t *testing.T) {
	notFound := &relaycontrol.RequestError{StatusCode: 404}
	source := &scriptedSource{sessions: []session{{err: notFound}, {err: notFound}, {err: notFound}}}
	reconciler := &countingReconciler{}
	health := &healthRecorder{}
	loop := fastLoop(source, reconciler)
	loop.SetHealthy = health.set
	loop.FallbackInterval = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runLoop(ctx, loop)

	waitFor(t, func() bool { return reconciler.calls.Load() >= 3 })
	cancel()
	<-done
	if !health.last() {
		t.Fatal("a reachable pre-SSE backend should still count as ready")
	}
}

func TestEventLoopIgnoresMalformedAndUnknownEvents(t *testing.T) {
	source := &scriptedSource{sessions: []session{{events: []relaycontrol.Event{
		connected,
		{Type: relaycontrol.EventSessionReady, Data: "not json"},
		{Type: relaycontrol.EventSessionReady, Data: `{"intentId":""}`},
		{Type: "future_event", Data: `{"x":1}`},
	}, block: true}}}
	reconciler := &countingReconciler{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runLoop(ctx, fastLoop(source, reconciler))

	waitFor(t, func() bool { return reconciler.calls.Load() == 1 })
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done
	if reconciler.calls.Load() != 1 {
		t.Fatalf("reconciles = %d; only connected should trigger one", reconciler.calls.Load())
	}
}

func TestEventLoopReconnectsAfterDropsAndStopsOnShutdown(t *testing.T) {
	source := &scriptedSource{sessions: []session{
		{err: errors.New("dial failed")},
		{events: []relaycontrol.Event{connected}, err: io.EOF},
		{events: []relaycontrol.Event{connected}, block: true},
	}}
	health := &healthRecorder{}
	loop := fastLoop(source, &countingReconciler{})
	loop.SetHealthy = health.set
	ctx, cancel := context.WithCancel(context.Background())
	done := runLoop(ctx, loop)

	waitFor(t, func() bool { return source.calls.Load() == 3 && health.last() })
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop on cancellation")
	}
}

func TestBackoffGrowsWithJitterAndCaps(t *testing.T) {
	for attempt, ceiling := range map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second, 20: time.Minute} {
		for range 50 {
			got := backoff(time.Second, time.Minute, attempt)
			if got < ceiling/2 || got > ceiling {
				t.Fatalf("attempt %d: %s outside [%s, %s]", attempt, got, ceiling/2, ceiling)
			}
		}
	}
}

// --- helpers ---

func fastLoop(source EventSource, reconciler Reconciler) *EventLoop {
	loop := NewEventLoop(source, reconciler, time.Minute, nil, nil)
	loop.MinBackoff, loop.MaxBackoff = 5*time.Millisecond, 20*time.Millisecond
	return loop
}

func runLoop(ctx context.Context, loop *EventLoop) <-chan error {
	done := make(chan error, 1)
	go func() { done <- loop.Run(ctx) }()
	return done
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type session struct {
	events []relaycontrol.Event
	then   func() // runs after events, before more
	more   []relaycontrol.Event
	err    error
	block  bool // stay connected until cancelled
}

type scriptedSource struct {
	sessions []session
	calls    atomic.Int32
}

func (s *scriptedSource) StreamEvents(ctx context.Context, onEvent func(relaycontrol.Event)) error {
	index := int(s.calls.Add(1)) - 1
	if index >= len(s.sessions) {
		<-ctx.Done()
		return ctx.Err()
	}
	current := s.sessions[index]
	for _, event := range current.events {
		onEvent(event)
	}
	if current.then != nil {
		time.Sleep(20 * time.Millisecond) // let the connected reconcile finish first
		current.then()
	}
	for _, event := range current.more {
		onEvent(event)
	}
	if current.block {
		<-ctx.Done()
		return ctx.Err()
	}
	if current.err == nil {
		return io.EOF
	}
	return current.err
}

// recoveryBackend mimics RelayControl: ready intents stay in /pending until acknowledged.
type recoveryBackend struct {
	mu        sync.Mutex
	ready     map[string]bool
	acked     map[string]bool
	listCalls atomic.Int32
}

func newRecoveryBackend() *recoveryBackend {
	return &recoveryBackend{ready: map[string]bool{}, acked: map[string]bool{}}
}

func (b *recoveryBackend) makeReady(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ready[id] = true
}

func (b *recoveryBackend) published(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.acked[id]
}

func (b *recoveryBackend) ListPending(context.Context) ([]relaycontrol.PendingIntent, error) {
	b.listCalls.Add(1)
	b.mu.Lock()
	defer b.mu.Unlock()
	var intents []relaycontrol.PendingIntent
	for id := range b.ready {
		if !b.acked[id] {
			intents = append(intents, relaycontrol.PendingIntent{ID: id, Status: "session_ready"})
		}
	}
	return intents, nil
}

func (b *recoveryBackend) GetStatus(_ context.Context, id string, _ int) (relaycontrol.IntentStatus, error) {
	return relaycontrol.IntentStatus{ID: id, Status: "session_ready", ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func (b *recoveryBackend) MarkPublished(_ context.Context, id, _ string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.acked[id] = true
	return nil
}

type countingPublisher struct {
	calls    atomic.Int32
	failures int32
}

func (p *countingPublisher) PublishReady(context.Context, relaycontrol.PendingIntent, relaycontrol.IntentStatus) (string, error) {
	if p.calls.Add(1) <= p.failures {
		return "", errors.New("discord unavailable")
	}
	return "message-1", nil
}

type countingReconciler struct{ calls atomic.Int32 }

func (r *countingReconciler) PollOnce(context.Context) error {
	r.calls.Add(1)
	return nil
}

type healthRecorder struct {
	mu     sync.Mutex
	values []bool
}

func (h *healthRecorder) set(value bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.values = append(h.values, value)
}

func (h *healthRecorder) last() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.values) > 0 && h.values[len(h.values)-1]
}
