package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func wsDial(t *testing.T, ts *httptest.Server, query url.Values, header http.Header) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	u := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	conn, resp, err := websocket.DefaultDialer.Dial(u, header)
	if conn != nil {
		t.Cleanup(func() { conn.Close() })
	}
	return conn, resp, err
}

func send(t *testing.T, conn *websocket.Conn, frame clientFrame) {
	t.Helper()
	if err := conn.WriteJSON(frame); err != nil {
		t.Fatalf("writing %+v: %v", frame, err)
	}
}

func readEvent(t *testing.T, conn *websocket.Conn) *Event {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var ev Event
	if err := conn.ReadJSON(&ev); err != nil {
		t.Fatalf("reading event: %v", err)
	}
	return &ev
}

// waitFor reads events until one satisfies pred, so tests do not depend on how
// much backlog the server replays first.
func waitFor(t *testing.T, conn *websocket.Conn, what string, pred func(*Event) bool) *Event {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		ev := readEvent(t, conn)
		if pred(ev) {
			return ev
		}
	}
	t.Fatalf("never saw %s", what)
	return nil
}

// wsJoin claims an identity over a fresh socket and returns it with its token.
func wsJoin(t *testing.T, ts *httptest.Server, handle, color, role string) (*websocket.Conn, string) {
	t.Helper()
	conn, _, err := wsDial(t, ts, nil, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	send(t, conn, clientFrame{Type: "join", Handle: handle, Color: color, Role: role})
	ack := readEvent(t, conn)
	if ack.Type != EventJoin || ack.Token == "" || ack.Self == nil || ack.Self.Handle != handle {
		t.Fatalf("join ack = %+v", ack)
	}
	if ev := waitFor(t, conn, "welcome", func(e *Event) bool { return e.Type == EventWelcome }); ev.Self.Handle != handle {
		t.Fatalf("welcome = %+v", ev)
	}
	return conn, ack.Token
}

func TestWSJoinAndBroadcast(t *testing.T) {
	ts, _ := newTestServer(t, nil)

	ada, _ := wsJoin(t, ts, "ada", "#e6194b", "human")
	claude, _ := wsJoin(t, ts, "claude", "#4363d8", "llm")

	// ada is told that claude arrived.
	ev := waitFor(t, ada, "claude's join", func(e *Event) bool {
		return e.Type == EventJoin && e.From != nil && e.From.Handle == "claude"
	})
	if ev.From.Role != RoleLLM || ev.From.Color != "#4363d8" {
		t.Errorf("join event = %+v", ev.From)
	}

	send(t, claude, clientFrame{Type: "message", Text: "hello humans"})
	for name, conn := range map[string]*websocket.Conn{"ada": ada, "claude": claude} {
		ev := waitFor(t, conn, name+"'s copy of the message", func(e *Event) bool {
			return e.Type == EventMessage
		})
		if ev.Text != "hello humans" || ev.From.Handle != "claude" || ev.Seq == 0 {
			t.Errorf("%s saw %+v", name, ev)
		}
	}

	// The roster is available on demand.
	send(t, ada, clientFrame{Type: "users"})
	ev = waitFor(t, ada, "roster", func(e *Event) bool { return e.Type == EventUsers })
	if len(ev.Users) != 2 {
		t.Errorf("roster = %+v; want 2 users", ev.Users)
	}
}

func TestWSJoinRejectionIsRetryableOnTheSameSocket(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	wsJoin(t, ts, "ada", "#e6194b", "human")

	conn, _, err := wsDial(t, ts, nil, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	// Taken handle, then taken color, then a bad color: all recoverable.
	for _, f := range []clientFrame{
		{Type: "join", Handle: "ADA", Color: "#3cb44b"},
		{Type: "join", Handle: "bob", Color: "#E6194B"},
		{Type: "join", Handle: "bob", Color: "crimson"},
		{Type: "message", Text: "let me in"},
	} {
		send(t, conn, f)
		if ev := readEvent(t, conn); ev.Type != EventError || ev.Error == "" {
			t.Fatalf("frame %+v got %+v; want an error event", f, ev)
		}
	}

	send(t, conn, clientFrame{Type: "join", Handle: "bob", Color: "#3cb44b", Role: "llm"})
	if ev := readEvent(t, conn); ev.Type != EventJoin || ev.Token == "" {
		t.Fatalf("retry after rejections got %+v; want a join ack", ev)
	}
}

func TestWSDisconnectCanResumeIdentity(t *testing.T) {
	ts, hub := newTestServer(t, nil)
	conn, token := wsJoin(t, ts, "ada", "#e6194b", "human")
	conn.Close()

	// Wait until the server has observed the disconnect. The identity stays
	// alive without a subscriber so the browser can resume after a network blip.
	if !eventually(2*time.Second, func() bool {
		hub.mu.Lock()
		defer hub.mu.Unlock()
		s, ok := hub.sessions[token]
		return ok && len(s.subs) == 0
	}) {
		t.Fatalf("session was removed or stayed subscribed after disconnect: %+v", hub.Users())
	}

	resumed, _, err := wsDial(t, ts, url.Values{"token": {token}}, nil)
	if err != nil {
		t.Fatalf("resume after disconnect: %v", err)
	}
	if ev := waitFor(t, resumed, "welcome after resume", func(e *Event) bool {
		return e.Type == EventWelcome
	}); ev.Self == nil || ev.Self.Handle != "ada" {
		t.Fatalf("resume welcome = %+v; want ada", ev)
	}

	send(t, resumed, clientFrame{Type: "message", Text: "back online"})
	waitFor(t, resumed, "message after resume", func(e *Event) bool {
		return e.Type == EventMessage && e.Text == "back online"
	})
}

func TestWSResumesRESTSessionWithoutEndingIt(t *testing.T) {
	ts, hub := newTestServer(t, nil)
	agent := restJoin(t, ts, "claude", "#4363d8", "llm")

	conn, _, err := wsDial(t, ts, url.Values{"token": {agent.Token}}, nil)
	if err != nil {
		t.Fatalf("dial with a REST token: %v", err)
	}
	ev := waitFor(t, conn, "welcome", func(e *Event) bool { return e.Type == EventWelcome })
	if ev.Self.Handle != "claude" {
		t.Fatalf("welcome = %+v; want the existing identity", ev.Self)
	}

	// Messages sent over the socket are attributed to the same identity.
	send(t, conn, clientFrame{Type: "message", Text: "same me"})
	waitFor(t, conn, "own message", func(e *Event) bool { return e.Type == EventMessage })

	conn.Close()
	// The socket did not own the session, so the REST identity survives.
	time.Sleep(200 * time.Millisecond)
	if _, err := hub.Touch(agent.Token); err != nil {
		t.Errorf("REST session died with the socket: %v", err)
	}
	resp, _ := call(t, ts, "GET", "/api/whoami", agent.Token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("whoami after socket close: status %d", resp.StatusCode)
	}
}

func TestWSAndRESTShareTheRoom(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	human, _ := wsJoin(t, ts, "ada", "#e6194b", "human")
	agent := restJoin(t, ts, "claude", "#4363d8", "llm")

	// REST -> WebSocket
	call(t, ts, "POST", "/api/messages", agent.Token, postRequest{Text: "from the API"})
	ev := waitFor(t, human, "the API message", func(e *Event) bool {
		return e.Type == EventMessage && e.Text == "from the API"
	})
	if ev.From.Handle != "claude" {
		t.Errorf("attributed to %+v", ev.From)
	}

	// WebSocket -> REST
	send(t, human, clientFrame{Type: "message", Text: "from the browser"})
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, raw := call(t, ts, "GET", "/api/messages?since="+itoa(agent.Cursor)+"&wait=2", agent.Token, nil)
		got := decode[readResponse](t, raw)
		for _, e := range got.Events {
			if e.Type == EventMessage && e.Text == "from the browser" && e.From.Handle == "ada" {
				return
			}
		}
		if got.Cursor > agent.Cursor {
			agent.Cursor = got.Cursor
		}
		if time.Now().After(deadline) {
			t.Fatal("the WebSocket message never showed up on the REST side")
		}
	}
}

func TestWSInvalidFrameDoesNotKillTheConnection(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	conn, _ := wsJoin(t, ts, "ada", "#e6194b", "human")

	if err := conn.WriteMessage(websocket.TextMessage, []byte("not json at all")); err != nil {
		t.Fatal(err)
	}
	ev := waitFor(t, conn, "the parse error", func(e *Event) bool { return e.Type == EventError })
	if !strings.Contains(ev.Error, "invalid JSON") {
		t.Errorf("error = %q", ev.Error)
	}

	send(t, conn, clientFrame{Type: "wat"})
	ev = waitFor(t, conn, "the unknown-type error", func(e *Event) bool { return e.Type == EventError })
	if !strings.Contains(ev.Error, "unknown frame type") {
		t.Errorf("error = %q", ev.Error)
	}

	send(t, conn, clientFrame{Type: "message", Text: "still connected"})
	waitFor(t, conn, "the message", func(e *Event) bool {
		return e.Type == EventMessage && e.Text == "still connected"
	})
}

func TestWSLeaveFrame(t *testing.T) {
	ts, hub := newTestServer(t, nil)
	conn, token := wsJoin(t, ts, "ada", "#e6194b", "human")

	send(t, conn, clientFrame{Type: "leave"})
	if !eventually(2*time.Second, func() bool {
		_, err := hub.Touch(token)
		return err != nil
	}) {
		t.Error("session survived an explicit leave frame")
	}
}

func TestWSStaleTokenIsRejected(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	conn, _, err := wsDial(t, ts, url.Values{"token": {"deadbeef"}}, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if ev := readEvent(t, conn); ev.Type != EventError {
		t.Fatalf("event = %+v; want an error for an unknown token", ev)
	}
}

func TestWSOriginIsChecked(t *testing.T) {
	ts, _ := newTestServer(t, nil)

	header := http.Header{"Origin": {"http://evil.example"}}
	if _, resp, err := wsDial(t, ts, nil, header); err == nil {
		t.Error("a foreign Origin was accepted")
	} else if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("err = %v, resp = %v; want 403", err, resp)
	}

	// The client's own origin is fine.
	header = http.Header{"Origin": {ts.URL}}
	if _, _, err := wsDial(t, ts, nil, header); err != nil {
		t.Errorf("same-origin dial rejected: %v", err)
	}

	// And so is anything, once the operator opts out of the check.
	open, _ := newTestServer(t, func(s *Server) { s.allowAnyOrigin = true })
	if _, _, err := wsDial(t, open, nil, http.Header{"Origin": {"http://evil.example"}}); err != nil {
		t.Errorf("dial with -allow-any-origin rejected: %v", err)
	}
}

func TestWSAccessTokenGate(t *testing.T) {
	ts, _ := newTestServer(t, func(s *Server) { s.accessToken = "s3cret" })

	if _, resp, err := wsDial(t, ts, nil, nil); err == nil {
		t.Error("dial without the access token succeeded")
	} else if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("err = %v, resp = %v; want 403", err, resp)
	}

	conn, _, err := wsDial(t, ts, url.Values{"access_token": {"s3cret"}}, nil)
	if err != nil {
		t.Fatalf("dial with the access token: %v", err)
	}
	send(t, conn, clientFrame{Type: "join", Handle: "ada", Color: "#e6194b"})
	if ev := readEvent(t, conn); ev.Type != EventJoin || ev.Token == "" {
		t.Errorf("event = %+v; want a join ack", ev)
	}
}

func eventually(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cond()
}

func TestWSRateLimitReportsRetryAfter(t *testing.T) {
	ts, hub := newTestServer(t, func(s *Server) {
		s.cfg.Rate, s.cfg.Burst = 60, 1
		s.hub.cfg = s.cfg
	})
	_ = hub
	conn, _ := wsJoin(t, ts, "ada", "#e6194b", "human")

	send(t, conn, clientFrame{Type: "message", Text: "first one is fine"})
	waitFor(t, conn, "the message", func(e *Event) bool {
		return e.Type == EventMessage && e.Text == "first one is fine"
	})

	send(t, conn, clientFrame{Type: "message", Text: "second one is too soon"})
	ev := waitFor(t, conn, "the rate-limit error", func(e *Event) bool { return e.Type == EventError })
	if !strings.Contains(ev.Error, "too fast") {
		t.Errorf("error = %q", ev.Error)
	}
	if ev.RetryAfter <= 0 {
		t.Errorf("retry_after = %v; want a positive number of seconds", ev.RetryAfter)
	}

	// Being throttled does not close the socket: the roster still answers.
	send(t, conn, clientFrame{Type: "users"})
	waitFor(t, conn, "the roster", func(e *Event) bool { return e.Type == EventUsers })
}
