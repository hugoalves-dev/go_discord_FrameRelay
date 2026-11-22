package relaycontrol

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseEventsFollowsEventStreamFraming(t *testing.T) {
	body := ": comment heartbeat\r\n" +
		"event: connected\r\ndata: {}\r\n\r\n" +
		"id: 7\nevent: session_ready\ndata: {\"intentId\":\n" + "data:\"a\"}\nretry: 10\nunknown: x\n\n" +
		"data: no type\r\r" +
		"event: heartbeat\n\n" + // no data: not dispatched
		"event: truncated\ndata: lost" // incomplete at EOF: discarded
	var events []Event
	lines := 0
	if err := ParseEvents(strings.NewReader(body), func() { lines++ }, func(e Event) { events = append(events, e) }); err != nil {
		t.Fatal(err)
	}
	want := []Event{
		{Type: "connected", Data: "{}"},
		{Type: "session_ready", ID: "7", Data: "{\"intentId\":\n\"a\"}"},
		{Type: "message", ID: "7", Data: "no type"},
	}
	if len(events) != len(want) {
		t.Fatalf("events = %#v", events)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("event %d = %#v, want %#v", i, events[i], want[i])
		}
	}
	if lines < 10 {
		t.Fatalf("liveness callback ran %d times; comments must count", lines)
	}
}

func TestParseEventsRejectsOversizedLinesAndEvents(t *testing.T) {
	longLine := "data: " + strings.Repeat("x", MaxEventSize) + "\n\n"
	if err := ParseEvents(strings.NewReader(longLine), nil, func(Event) {}); !errors.Is(err, ErrEventTooLarge) {
		t.Fatalf("long line err = %v", err)
	}
	manyLines := strings.Repeat("data: "+strings.Repeat("y", 1000)+"\n", 20) + "\n"
	emitted := false
	if err := ParseEvents(strings.NewReader(manyLines), nil, func(Event) { emitted = true }); !errors.Is(err, ErrEventTooLarge) || emitted {
		t.Fatalf("oversized event err = %v emitted = %v", err, emitted)
	}
}

func TestStreamEventsAuthenticatesAndDeliversUntilServerCloses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/launch-intents/events" || r.Header.Get("Authorization") != "Bearer token" ||
			r.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("unexpected request: %s %v", r.URL.Path, r.Header)
		}
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		_, _ = io.WriteString(w, "event: connected\ndata: {}\n\n: keepalive\n\nevent: session_ready\nid: 1\ndata: {\"intentId\":\"i-1\"}\n\n")
	}))
	defer server.Close()

	var events []Event
	err := NewClient(server.URL, "token", server.Client()).StreamEvents(context.Background(), func(e Event) { events = append(events, e) })

	if !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want io.EOF on clean close", err)
	}
	if len(events) != 2 || events[0].Type != EventConnected || events[1].Type != EventSessionReady || events[1].ID != "1" {
		t.Fatalf("events = %#v", events)
	}
}

func TestStreamEventsReturnsStatusErrorsForAuthAndUnsupportedBackends(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		}))
		err := NewClient(server.URL, "token", server.Client()).StreamEvents(context.Background(), func(Event) {})
		server.Close()
		var requestError *RequestError
		if !errors.As(err, &requestError) || requestError.StatusCode != status {
			t.Fatalf("status %d: err = %v", status, err)
		}
		if strings.Contains(err.Error(), "token") {
			t.Fatal("error leaked the bearer token")
		}
	}
}

func TestStreamEventsRejectsNonEventStreamResponses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "[]")
	}))
	defer server.Close()
	err := NewClient(server.URL, "token", server.Client()).StreamEvents(context.Background(), func(Event) {})
	if !errors.Is(err, ErrNotEventStream) {
		t.Fatalf("err = %v", err)
	}
}

func TestStreamEventsDropsSilentStreams(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: connected\ndata: {}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	client := NewClient(server.URL, "token", server.Client())
	client.streamIdleTimeout = 100 * time.Millisecond

	started := time.Now()
	err := client.StreamEvents(context.Background(), func(Event) {})

	if !errors.Is(err, ErrStreamIdle) {
		t.Fatalf("err = %v, want idle timeout", err)
	}
	if time.Since(started) > 5*time.Second {
		t.Fatal("idle timeout did not fire promptly")
	}
}

func TestStreamEventsStopsOnCancellation(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: connected\ndata: {}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())

	err := NewClient(server.URL, "token", server.Client()).StreamEvents(ctx, func(e Event) {
		if e.Type == EventConnected {
			cancel()
		}
	})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
