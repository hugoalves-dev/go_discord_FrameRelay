package relaycontrol

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"
)

// Event types emitted by GET /api/launch-intents/events.
const (
	EventConnected    = "connected"
	EventSessionReady = "session_ready"
	EventHeartbeat    = "heartbeat"
)

const (
	// MaxEventSize bounds one line and one event's accumulated data.
	MaxEventSize = 16 << 10
	// DefaultStreamIdleTimeout drops a stream that sent nothing (not even a heartbeat) for this long.
	// RelayControl sends a heartbeat every 20s by default.
	DefaultStreamIdleTimeout = 75 * time.Second
)

var (
	ErrEventTooLarge  = errors.New("server-sent event exceeds size limit")
	ErrStreamIdle     = errors.New("event stream idle timeout")
	ErrNotEventStream = errors.New("RelayControl did not return text/event-stream")
)

// Event is one dispatched server-sent event. Type defaults to "message" when the server omits it.
type Event struct {
	Type string
	ID   string
	Data string
}

// StreamEvents opens the bot event stream and calls onEvent for every event until the stream ends,
// ctx is cancelled, or nothing arrives within the idle timeout. It always returns a non-nil error:
// *RequestError for an HTTP failure status (404 = backend without SSE, 401/403 = credentials),
// io.EOF for a clean server close.
func (c *Client) StreamEvents(ctx context.Context, onEvent func(Event)) error {
	streamCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	request, err := http.NewRequestWithContext(streamCtx, http.MethodGet, c.baseURL+"/api/launch-intents/events", nil)
	if err != nil {
		return fmt.Errorf("create RelayControl event request: %w", err)
	}
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("Cache-Control", "no-cache")
	request.Header.Set("Authorization", "Bearer "+c.token)

	idle := c.streamIdleTimeout
	if idle <= 0 {
		idle = DefaultStreamIdleTimeout
	}
	watchdog := time.AfterFunc(idle, func() { cancel(ErrStreamIdle) })
	defer watchdog.Stop()

	response, err := c.stream.Do(request)
	if err != nil {
		if cause := context.Cause(streamCtx); errors.Is(cause, ErrStreamIdle) {
			return ErrStreamIdle
		}
		return fmt.Errorf("RelayControl event stream failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		return &RequestError{StatusCode: response.StatusCode}
	}
	if mediaType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type")); mediaType != "text/event-stream" {
		return ErrNotEventStream
	}

	err = ParseEvents(response.Body, func() { watchdog.Reset(idle) }, func(event Event) {
		onEvent(event)
		watchdog.Reset(idle) // slow handling must not count as server silence
	})
	if cause := context.Cause(streamCtx); errors.Is(cause, ErrStreamIdle) {
		return ErrStreamIdle
	}
	if err == nil {
		return io.EOF
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// ParseEvents reads a text/event-stream body per the WHATWG framing rules: fields are terminated
// by LF, CRLF or CR, an empty line dispatches the event, lines starting with ':' are comments, and
// an incomplete event at EOF is discarded. onLine (optional) runs after every line, including
// comments, so callers can track liveness. It returns nil at EOF.
func ParseEvents(body io.Reader, onLine func(), emit func(Event)) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 4096), MaxEventSize)
	scanner.Split(scanLines)
	var (
		eventType, lastID string
		data              strings.Builder
		hasData           bool
	)
	for scanner.Scan() {
		if onLine != nil {
			onLine()
		}
		line := scanner.Text()
		if line == "" {
			if hasData {
				event := Event{Type: eventType, ID: lastID, Data: strings.TrimSuffix(data.String(), "\n")}
				if event.Type == "" {
					event.Type = "message"
				}
				emit(event)
			}
			eventType, hasData = "", false
			data.Reset()
			continue
		}
		if line[0] == ':' {
			continue
		}
		field, value, found := strings.Cut(line, ":")
		if found {
			value = strings.TrimPrefix(value, " ")
		}
		switch field {
		case "event":
			eventType = value
		case "data":
			if data.Len()+len(value)+1 > MaxEventSize {
				return ErrEventTooLarge
			}
			data.WriteString(value)
			data.WriteByte('\n')
			hasData = true
		case "id":
			if !strings.ContainsRune(value, 0) {
				lastID = value
			}
		}
		// "retry" and unknown fields are ignored: reconnect timing is owned by the bot.
	}
	if errors.Is(scanner.Err(), bufio.ErrTooLong) {
		return ErrEventTooLarge
	}
	return scanner.Err()
}

// scanLines splits on LF, CRLF or a lone CR.
func scanLines(data []byte, atEOF bool) (int, []byte, error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		if data[i] == '\n' {
			return i + 1, data[:i], nil
		}
		if i+1 < len(data) {
			if data[i+1] == '\n' {
				return i + 2, data[:i], nil
			}
			return i + 1, data[:i], nil
		}
		if atEOF {
			return i + 1, data[:i], nil
		}
		return 0, nil, nil // need one more byte to tell CR from CRLF
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}
