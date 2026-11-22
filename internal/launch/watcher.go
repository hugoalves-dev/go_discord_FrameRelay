package launch

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/hugoalves-dev/go_discord_FrameRelay/internal/relaycontrol"
)

type Backend interface {
	ListPending(context.Context) ([]relaycontrol.PendingIntent, error)
	GetStatus(context.Context, string, int) (relaycontrol.IntentStatus, error)
	MarkPublished(context.Context, string, string) error
}

type Publisher interface {
	PublishReady(context.Context, relaycontrol.PendingIntent, relaycontrol.IntentStatus) (string, error)
}

// Watcher publishes ready, unpublished launch intents and acknowledges them. It is driven by
// EventLoop; PollOnce is idempotent because acknowledged intents leave GET /pending.
type Watcher struct {
	backend   Backend
	publisher Publisher
	now       func() time.Time
	logger    *slog.Logger
	cacheMu   sync.Mutex
	ready     map[string]relaycontrol.IntentStatus
}

func NewWatcher(backend Backend, publisher Publisher, logger *slog.Logger) *Watcher {
	return &Watcher{
		backend: backend, publisher: publisher,
		now: time.Now, logger: logger, ready: make(map[string]relaycontrol.IntentStatus),
	}
}

func (w *Watcher) PollOnce(ctx context.Context) error {
	intents, err := w.backend.ListPending(ctx)
	if err != nil {
		return err
	}
	var firstError error
	for _, intent := range intents {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !intent.ExpiresAt.IsZero() && !intent.ExpiresAt.After(w.now()) && intent.Status != "session_ready" {
			continue
		}
		status, err := w.getStatus(ctx, intent.ID)
		if err != nil {
			if firstError == nil {
				firstError = err
			}
			continue
		}
		if status.Status != "session_ready" {
			continue
		}
		messageID, err := w.publisher.PublishReady(ctx, intent, status)
		if err == nil {
			err = w.backend.MarkPublished(ctx, intent.ID, messageID)
		}
		if err != nil && firstError == nil {
			firstError = err
		} else if err == nil {
			w.cacheMu.Lock()
			delete(w.ready, intent.ID)
			w.cacheMu.Unlock()
		}
	}
	return firstError
}

func (w *Watcher) getStatus(ctx context.Context, intentID string) (relaycontrol.IntentStatus, error) {
	w.cacheMu.Lock()
	cached, ok := w.ready[intentID]
	if ok && cached.ExpiresAt.After(w.now()) {
		w.cacheMu.Unlock()
		return cached, nil
	}
	delete(w.ready, intentID)
	w.cacheMu.Unlock()

	// Poll readiness only. Each user's watch command creates their own capability.
	status, err := w.backend.GetStatus(ctx, intentID, 0)
	if err != nil {
		return status, err
	}
	if status.Status == "session_ready" && status.ExpiresAt.After(w.now()) {
		w.cacheMu.Lock()
		w.ready[intentID] = status
		w.cacheMu.Unlock()
	}
	return status, nil
}
