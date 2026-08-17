package main

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// sseEvent is one parsed frame off the wire.
type sseEvent struct {
	ID    string
	Event string
	Data  string
}

// openStream starts a stream and returns a channel of parsed frames plus a stop
// function. Reading happens in a goroutine so a test can assert on timing.
func openStream(t *testing.T, ts *httptest.Server, path, token string, header http.Header) (<-chan sseEvent, func()) {
	t.Helper()
	req, err := http.NewRequest("GET", ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("stream: status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content type = %q; want text/event-stream", ct)
	}

	out := make(chan sseEvent, 64)
	go func() {
		defer close(out)
		scanner := bufio.NewScanner(resp.Body)
		var cur sseEvent
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case line == "": // blank line terminates a frame
				if cur.Data != "" || cur.Event != "" {
					out <- cur
				}
				cur = sseEvent{}
			case strings.HasPrefix(line, ":"): // a comment: the heartbeat
				out <- sseEvent{Event: "__ping__"}
			case strings.HasPrefix(line, "id: "):
				cur.ID = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				cur.Event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				cur.Data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	return out, func() { resp.Body.Close() }
}

func nextEvent(t *testing.T, stream <-chan sseEvent, within time.Duration, what string) sseEvent {
	t.Helper()
	select {
	case ev, ok := <-stream:
		if !ok {
			t.Fatalf("stream closed while waiting for %s", what)
		}
		return ev
	case <-time.After(within):
		t.Fatalf("no %s within %s", what, within)
		return sseEvent{}
	}
}

// drain collects frames until the stream goes quiet for the given period.
func drain(stream <-chan sseEvent, quietFor time.Duration) []sseEvent {
	var out []sseEvent
	for {
		select {
		case ev, ok := <-stream:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-time.After(quietFor):
			return out
		}
	}
}

func TestStreamPushesWithoutPolling(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	agent := restJoin(t, ts, "agent", "#4363d8", "llm")
	human := restJoin(t, ts, "human", "#3cb44b", "human")

	stream, stop := openStream(t, ts, "/api/stream?since=last-read", agent.Token, nil)
	defer stop()

	// The stream opens by saying what you are attached to.
	if ev := nextEvent(t, stream, 2*time.Second, "welcome"); ev.Event != EventWelcome {
		t.Fatalf("first frame = %+v; want welcome", ev)
	}
	// Then the backlog — both joins happened before the stream opened.
	backlog := drain(stream, 300*time.Millisecond)
	if len(backlog) != 2 || backlog[0].Event != EventJoin || backlog[1].Event != EventJoin {
		t.Fatalf("backlog = %+v; want the two joins", backlog)
	}

	// Now the point of the whole thing: a message published later arrives on the
	// open connection, with no second request, and without waiting.
	start := time.Now()
	call(t, ts, "POST", "/api/messages", human.Token, postRequest{Text: "pushed, not polled"})

	ev := nextEvent(t, stream, 2*time.Second, "the pushed message")
	took := time.Since(start)
	if ev.Event != EventMessage || !strings.Contains(ev.Data, "pushed, not polled") {
		t.Fatalf("frame = %+v", ev)
	}
	if took > 500*time.Millisecond {
		t.Errorf("the message took %v to arrive; a push should be immediate", took)
	}
	// Numbered events carry an id, which is what Last-Event-ID resumes from.
	if ev.ID == "" {
		t.Error("a numbered event arrived without an SSE id")
	}
}

func TestStreamResumesFromLastEventID(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	agent := restJoin(t, ts, "agent", "#4363d8", "llm")
	human := restJoin(t, ts, "human", "#3cb44b", "human")

	call(t, ts, "POST", "/api/messages", human.Token, postRequest{Text: "first"})
	call(t, ts, "POST", "/api/messages", human.Token, postRequest{Text: "second"})
	_, raw := call(t, ts, "GET", "/api/messages?since=0", agent.Token, nil)
	events := decode[readResponse](t, raw).Events
	firstMessage := events[len(events)-2] // "first"

	// Resuming from that id must replay "second" and nothing before it.
	stream, stop := openStream(t, ts, "/api/stream", agent.Token,
		http.Header{"Last-Event-ID": {itoa(firstMessage.Seq)}})
	defer stop()

	nextEvent(t, stream, 2*time.Second, "welcome")
	ev := nextEvent(t, stream, 2*time.Second, "the replayed message")
	if !strings.Contains(ev.Data, "second") {
		t.Errorf("resumed with %+v; want the message after the given id", ev)
	}
	if strings.Contains(ev.Data, "first") {
		t.Error("resuming replayed an event the client had already seen")
	}
}

func TestStreamKeepsTheSessionAlive(t *testing.T) {
	ts, hub := newTestServer(t, nil)
	agent := restJoin(t, ts, "agent", "#4363d8", "llm")

	stream, stop := openStream(t, ts, "/api/stream", agent.Token, nil)
	defer stop()
	nextEvent(t, stream, 2*time.Second, "welcome")

	// An open stream is a live subscriber, so the reaper must leave it alone —
	// this is what stops an agent losing its handle between turns.
	if n := hub.ReapIdle(time.Now().Add(time.Hour)); n != 0 {
		t.Errorf("reaped %d sessions with an open stream; want 0", n)
	}
	if _, err := hub.Touch(agent.Token); err != nil {
		t.Errorf("session died with a stream open: %v", err)
	}
}

func TestStreamDropsSubscriberOnDisconnect(t *testing.T) {
	ts, hub := newTestServer(t, nil)
	agent := restJoin(t, ts, "agent", "#4363d8", "llm")

	stream, stop := openStream(t, ts, "/api/stream", agent.Token, nil)
	nextEvent(t, stream, 2*time.Second, "welcome")
	stop() // hang up

	// The handler must notice and unsubscribe, or subscribers would pile up.
	if !eventually(3*time.Second, func() bool {
		hub.mu.Lock()
		defer hub.mu.Unlock()
		return len(hub.sessions[agent.Token].subs) == 0
	}) {
		t.Error("the subscriber outlived the disconnected client")
	}
	// The session itself survives: it was a REST session, not owned by the stream.
	if _, err := hub.Touch(agent.Token); err != nil {
		t.Errorf("session died with the stream: %v", err)
	}
}

func TestStreamEndsWhenTheSessionDoes(t *testing.T) {
	ts, hub := newTestServer(t, nil)
	agent := restJoin(t, ts, "agent", "#4363d8", "llm")

	stream, stop := openStream(t, ts, "/api/stream", agent.Token, nil)
	defer stop()
	nextEvent(t, stream, 2*time.Second, "welcome")

	hub.Leave(agent.Token, "left")

	// Somewhere in what follows there must be an error frame explaining why, and
	// then the stream must close rather than hang.
	sawError := false
	deadline := time.After(3 * time.Second)
	for !sawError {
		select {
		case ev, ok := <-stream:
			if !ok {
				t.Fatal("stream closed without saying the session had ended")
			}
			if ev.Event == EventError && strings.Contains(ev.Data, "not joined") {
				sawError = true
			}
		case <-deadline:
			t.Fatal("no error frame after the session ended")
		}
	}
}

func TestStreamNeedsATokenAndValidatesSince(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	if resp, _ := call(t, ts, "GET", "/api/stream", "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no token: status %d; want 401", resp.StatusCode)
	}
	agent := restJoin(t, ts, "agent", "#4363d8", "llm")
	if resp, _ := call(t, ts, "GET", "/api/stream?since=whenever", agent.Token, nil); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad since: status %d; want 400", resp.StatusCode)
	}
}

func TestStreamSetsHeadersThatStopBuffering(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	agent := restJoin(t, ts, "agent", "#4363d8", "llm")

	req, _ := http.NewRequest("GET", ts.URL+"/api/stream", nil)
	req.Header.Set("Authorization", "Bearer "+agent.Token)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Without these a proxy will happily hold events until a buffer fills.
	for header, want := range map[string]string{
		"Content-Type":      "text/event-stream",
		"Cache-Control":     "no-store",
		"X-Accel-Buffering": "no",
	} {
		if got := resp.Header.Get(header); got != want {
			t.Errorf("%s = %q; want %q", header, got, want)
		}
	}
}

func TestGuideDocumentsTheStream(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	_, body := get(t, ts, "/api", "*/*")
	for _, want := range []string{"/api/stream", "curl -N", "no polling", "Last-Event-ID"} {
		if !strings.Contains(body, want) {
			t.Errorf("guide does not mention %q", want)
		}
	}
}
