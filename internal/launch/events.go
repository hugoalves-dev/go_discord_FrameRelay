package launch

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"time"

	"github.com/hugoalves-dev/go_discord_FrameRelay/internal/relaycontrol"
)

// EventSource is the RelayControl SSE stream (relaycontrol.Client.StreamEvents).
type EventSource interface {
	StreamEvents(context.Context, func(relaycontrol.Event)) error
}

// Reconciler publishes every ready-but-unpublished intent once (Watcher.PollOnce).
type Reconciler interface {
	PollOnce(context.Context) error
}

// EventLoop replaces fixed-interval polling with the RelayControl event stream.
//
// On every connection it waits for "connected" (sent after the server registered the
// subscription) and then reconciles GET /pending once, so a session_ready committed while
// disconnected is recovered and one committed afterwards arrives on the stream. session_ready
// events trigger the same idempotent reconciliation, so duplicate events are harmless.
// Reconciliation runs in its own goroutine and coalesces triggers; a failed run (Discord or API
// unavailable) is retried with backoff instead of waiting for the next event.
//
// Fallback: only when the backend answers 404 (no SSE support yet) does the loop reconcile at
// FallbackInterval and re-probe the stream; any other disconnect reconnects with backoff.
type EventLoop struct {
	Source           EventSource
	Reconciler       Reconciler
	FallbackInterval time.Duration
	// SetHealthy reports whether RelayControl is reachable through the stream (optional).
	SetHealthy func(bool)
	Logger     *slog.Logger

	MinBackoff  time.Duration // first reconnect / retry delay
	MaxBackoff  time.Duration // cap for transient failures
	AuthBackoff time.Duration // delay after 401/403: a configuration problem, not a busy retry
}

const (
	defaultMinBackoff  = time.Second
	defaultMaxBackoff  = time.Minute
	defaultAuthBackoff = 5 * time.Minute
)

func NewEventLoop(source EventSource, reconciler Reconciler, fallbackInterval time.Duration, setHealthy func(bool), logger *slog.Logger) *EventLoop {
	return &EventLoop{
		Source: source, Reconciler: reconciler, FallbackInterval: fallbackInterval,
		SetHealthy: setHealthy, Logger: logger,
		MinBackoff: defaultMinBackoff, MaxBackoff: defaultMaxBackoff, AuthBackoff: defaultAuthBackoff,
	}
}

func (l *EventLoop) Run(ctx context.Context) error {
	if l.FallbackInterval <= 0 {
		return errors.New("intent fallback interval must be positive")
	}
	triggers := make(chan struct{}, 1)
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		l.reconcileWorker(ctx, triggers)
	}()
	defer func() { <-workerDone }()

	failures := 0
	for {
		connected := false
		err := l.Source.StreamEvents(ctx, func(event relaycontrol.Event) {
			switch event.Type {
			case relaycontrol.EventConnected:
				connected, failures = true, 0
				l.setHealthy(true)
				trigger(triggers)
			case relaycontrol.EventSessionReady:
				l.setHealthy(true)
				if !validReadyEvent(event.Data) {
					l.warn("ignored malformed launch event", "event", event.Type)
					return
				}
				trigger(triggers)
			case relaycontrol.EventHeartbeat:
				l.setHealthy(true)
			default:
				if l.Logger != nil {
					l.Logger.Debug("ignored unknown launch event", "operation", "intent_events", "event", event.Type)
				}
			}
		})
		if ctx.Err() != nil {
			return ctx.Err()
		}

		var requestError *relaycontrol.RequestError
		isRequestError := errors.As(err, &requestError)
		var delay time.Duration
		switch {
		case isRequestError && requestError.StatusCode == http.StatusNotFound:
			// Backend predates SSE: reconcile at the low fallback cadence, then re-probe.
			l.setHealthy(true)
			l.info("launch event stream unsupported; using low-frequency fallback")
			trigger(triggers)
			delay = l.FallbackInterval
		case isRequestError && (requestError.StatusCode == http.StatusUnauthorized || requestError.StatusCode == http.StatusForbidden):
			l.setHealthy(false)
			if l.Logger != nil {
				l.Logger.Error("RelayControl rejected the bot credential; check RELAYCONTROL_SERVICE_TOKEN",
					"operation", "intent_events", "status", requestError.StatusCode)
			}
			delay = l.AuthBackoff
		default:
			l.setHealthy(false)
			if !connected {
				failures++
			} else {
				failures = 1
			}
			delay = backoff(l.MinBackoff, l.MaxBackoff, failures)
			l.warn("launch event stream disconnected; reconnecting", "attempt", failures)
		}
		if !sleep(ctx, delay) {
			return ctx.Err()
		}
	}
}

func (l *EventLoop) reconcileWorker(ctx context.Context, triggers <-chan struct{}) {
	var retry *time.Timer
	var retryC <-chan time.Time
	failures := 0
	defer func() {
		if retry != nil {
			retry.Stop()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-triggers:
		case <-retryC:
		}
		if retry != nil {
			retry.Stop()
			retry, retryC = nil, nil
		}
		err := l.Reconciler.PollOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			failures = 0
			continue
		}
		failures++
		l.warn("launch recovery will retry", "attempt", failures)
		retry = time.NewTimer(backoff(l.MinBackoff, l.MaxBackoff, failures))
		retryC = retry.C
	}
}

func (l *EventLoop) setHealthy(healthy bool) {
	if l.SetHealthy != nil {
		l.SetHealthy(healthy)
	}
}

func (l *EventLoop) warn(message string, args ...any) {
	if l.Logger != nil {
		l.Logger.Warn(message, append([]any{"operation", "intent_events"}, args...)...)
	}
}

func (l *EventLoop) info(message string) {
	if l.Logger != nil {
		l.Logger.Info(message, "operation", "intent_events")
	}
}

func trigger(triggers chan<- struct{}) {
	select {
	case triggers <- struct{}{}:
	default: // a reconciliation is already queued; it will observe this change too
	}
}

func validReadyEvent(data string) bool {
	var payload struct {
		IntentID string `json:"intentId"`
	}
	return json.Unmarshal([]byte(data), &payload) == nil && payload.IntentID != "" && len(payload.IntentID) <= 64
}

// backoff returns exponential backoff with equal jitter: a value in [d/2, d) where
// d = min(max, min*2^(attempt-1)).
func backoff(minDelay, maxDelay time.Duration, attempt int) time.Duration {
	if minDelay <= 0 {
		minDelay = defaultMinBackoff
	}
	if maxDelay < minDelay {
		maxDelay = minDelay
	}
	delay := minDelay
	for i := 1; i < attempt && delay < maxDelay; i++ {
		delay *= 2
	}
	delay = min(delay, maxDelay)
	half := delay / 2
	return half + time.Duration(rand.Int64N(int64(half)+1))
}

func sleep(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
