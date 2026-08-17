package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func newTestServer(t *testing.T, mutate func(*Server)) (*httptest.Server, *Hub) {
	t.Helper()
	cfg := testConfig()
	hub := NewHub(cfg)
	srv := &Server{
		hub:     hub,
		cfg:     cfg,
		maxWait: 5 * time.Second,
		logger:  log.New(io.Discard, "", 0),
	}
	if mutate != nil {
		mutate(srv)
	}
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)
	return ts, hub
}

// call issues a request against the test server, optionally authenticated.
func call(t *testing.T, ts *httptest.Server, method, path, token string, body any) (*http.Response, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, ts.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, raw
}

func decode[T any](t *testing.T, raw []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decoding %s: %v", raw, err)
	}
	return v
}

// restJoin performs the join an LLM agent would do with curl.
func restJoin(t *testing.T, ts *httptest.Server, handle, color, role string) joinResponse {
	t.Helper()
	resp, raw := call(t, ts, "POST", "/api/join", "",
		joinRequest{Handle: handle, Color: color, Role: role})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("join %s: status %d, body %s", handle, resp.StatusCode, raw)
	}
	return decode[joinResponse](t, raw)
}

func TestRESTJoinPostRead(t *testing.T) {
	ts, _ := newTestServer(t, nil)

	ada := restJoin(t, ts, "ada", "#e6194b", "human")
	if ada.Token == "" || ada.Self.Handle != "ada" || ada.Self.Color != "#e6194b" {
		t.Fatalf("join response = %+v", ada)
	}
	if ada.Self.Role != RoleHuman {
		t.Errorf("role = %q; want human", ada.Self.Role)
	}

	claude := restJoin(t, ts, "claude", "4363D8", "llm")
	if claude.Self.Color != "#4363d8" {
		t.Errorf("color not normalized: %q", claude.Self.Color)
	}
	if claude.Self.Role != RoleLLM {
		t.Errorf("role = %q; want llm", claude.Self.Role)
	}
	// The joiner sees who is already in the room, including itself.
	if len(claude.Users) != 2 {
		t.Errorf("users on join = %d; want 2", len(claude.Users))
	}

	resp, raw := call(t, ts, "POST", "/api/messages", claude.Token,
		postRequest{Text: "hello from an agent"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("post: status %d, body %s", resp.StatusCode, raw)
	}

	// ada reads everything published after her own join.
	resp, raw = call(t, ts, "GET", "/api/messages?since="+itoa(ada.Cursor), ada.Token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("read: status %d, body %s", resp.StatusCode, raw)
	}
	read := decode[readResponse](t, raw)
	if len(read.Events) != 2 {
		t.Fatalf("read %d events; want claude's join and message: %s", len(read.Events), raw)
	}
	if read.Events[0].Type != EventJoin || read.Events[1].Type != EventMessage {
		t.Errorf("event types = %q, %q", read.Events[0].Type, read.Events[1].Type)
	}
	msg := read.Events[1]
	if msg.Text != "hello from an agent" || msg.From.Handle != "claude" || msg.From.Color != "#4363d8" {
		t.Errorf("message = %+v", msg)
	}
	if read.Truncated {
		t.Error("read reported truncation on a fresh hub")
	}

	// Reading again from the new cursor yields nothing.
	_, raw = call(t, ts, "GET", "/api/messages?since="+itoa(read.Cursor), ada.Token, nil)
	if got := decode[readResponse](t, raw); len(got.Events) != 0 {
		t.Errorf("re-read returned %d events; want 0", len(got.Events))
	}
}

func TestRESTJoinConflicts(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	restJoin(t, ts, "ada", "#e6194b", "human")

	cases := []struct {
		name       string
		req        joinRequest
		wantStatus int
	}{
		{"same handle", joinRequest{Handle: "ada", Color: "#3cb44b"}, http.StatusConflict},
		{"same handle other case", joinRequest{Handle: "AdA", Color: "#3cb44b"}, http.StatusConflict},
		{"same color", joinRequest{Handle: "bob", Color: "#E6194B"}, http.StatusConflict},
		{"near color", joinRequest{Handle: "bob", Color: "#e6194c"}, http.StatusConflict},
		{"bad color", joinRequest{Handle: "bob", Color: "crimson"}, http.StatusBadRequest},
		{"no color", joinRequest{Handle: "bob"}, http.StatusBadRequest},
		{"bad handle", joinRequest{Handle: "bo b", Color: "#3cb44b"}, http.StatusBadRequest},
		{"reserved handle", joinRequest{Handle: "system", Color: "#3cb44b"}, http.StatusBadRequest},
		{"bad role", joinRequest{Handle: "bob", Color: "#3cb44b", Role: "dolphin"}, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, raw := call(t, ts, "POST", "/api/join", "", tc.req)
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status %d, want %d; body %s", resp.StatusCode, tc.wantStatus, raw)
			}
			if decode[map[string]string](t, raw)["error"] == "" {
				t.Errorf("no error message in %s", raw)
			}
		})
	}

	if got := len(decode[map[string][]User](t, mustGet(t, ts, "/api/users"))["users"]); got != 1 {
		t.Errorf("failed joins left %d users behind; want 1", got)
	}
}

func TestRESTRejectsUnknownFields(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	req, _ := http.NewRequest("POST", ts.URL+"/api/join",
		bytes.NewReader([]byte(`{"handle":"ada","color":"#e6194b","nickname":"typo"}`)))
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status %d; want 400 so typos are not silently ignored", resp.StatusCode)
	}
}

func TestRESTRequiresToken(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	for _, path := range []string{"/api/messages", "/api/whoami"} {
		resp, raw := call(t, ts, "GET", path, "", nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s without a token: status %d, body %s", path, resp.StatusCode, raw)
		}
	}
	resp, _ := call(t, ts, "POST", "/api/messages", "bogus", postRequest{Text: "hi"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("POST with a bogus token: status %d; want 401", resp.StatusCode)
	}
}

func TestRESTLongPollWakesOnMessage(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	ada := restJoin(t, ts, "ada", "#e6194b", "human")
	claude := restJoin(t, ts, "claude", "#4363d8", "llm")

	type result struct {
		read readResponse
		took time.Duration
	}
	done := make(chan result, 1)
	go func() {
		start := time.Now()
		_, raw := call(t, ts, "GET", "/api/messages?since="+itoa(claude.Cursor)+"&wait=5", claude.Token, nil)
		done <- result{decode[readResponse](t, raw), time.Since(start)}
	}()

	time.Sleep(150 * time.Millisecond) // let the poll block
	call(t, ts, "POST", "/api/messages", ada.Token, postRequest{Text: "wake up"})

	select {
	case got := <-done:
		if len(got.read.Events) != 1 || got.read.Events[0].Text != "wake up" {
			t.Fatalf("long poll returned %+v", got.read.Events)
		}
		if got.read.Cursor != got.read.Events[0].Seq {
			t.Errorf("cursor = %d; want %d", got.read.Cursor, got.read.Events[0].Seq)
		}
		if got.took > 3*time.Second {
			t.Errorf("long poll took %v; should return as soon as the message lands", got.took)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("long poll never returned")
	}
}

func TestRESTLongPollReturnsBacklogImmediately(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	ada := restJoin(t, ts, "ada", "#e6194b", "human")
	call(t, ts, "POST", "/api/messages", ada.Token, postRequest{Text: "already here"})

	start := time.Now()
	_, raw := call(t, ts, "GET", "/api/messages?since="+itoa(ada.Cursor)+"&wait=5", ada.Token, nil)
	if took := time.Since(start); took > time.Second {
		t.Errorf("waited %v even though a message was pending", took)
	}
	if got := decode[readResponse](t, raw); len(got.Events) != 1 {
		t.Errorf("returned %d events; want the pending one", len(got.Events))
	}
}

func TestRESTLongPollTimesOutEmpty(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	ada := restJoin(t, ts, "ada", "#e6194b", "human")

	start := time.Now()
	resp, raw := call(t, ts, "GET", "/api/messages?since="+itoa(ada.Cursor)+"&wait=1", ada.Token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	if took := time.Since(start); took < 900*time.Millisecond {
		t.Errorf("returned after %v; want it to wait ~1s", took)
	}
	got := decode[readResponse](t, raw)
	if len(got.Events) != 0 {
		t.Errorf("events = %+v; want none", got.Events)
	}
	if got.Cursor != ada.Cursor {
		t.Errorf("cursor moved to %d with nothing published; want %d", got.Cursor, ada.Cursor)
	}
}

func TestRESTBadQueryParams(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	ada := restJoin(t, ts, "ada", "#e6194b", "human")
	for _, q := range []string{"?since=-1", "?since=abc", "?wait=soon", "?wait=-5"} {
		resp, raw := call(t, ts, "GET", "/api/messages"+q, ada.Token, nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("GET /api/messages%s: status %d, body %s", q, resp.StatusCode, raw)
		}
	}
}

func TestRESTWaitIsCapped(t *testing.T) {
	ts, _ := newTestServer(t, func(s *Server) { s.maxWait = 500 * time.Millisecond })
	ada := restJoin(t, ts, "ada", "#e6194b", "human")
	start := time.Now()
	call(t, ts, "GET", "/api/messages?wait=60", ada.Token, nil)
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("wait=60 blocked for %v; server cap should apply", took)
	}
}

func TestRESTLeaveReleasesIdentity(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	ada := restJoin(t, ts, "ada", "#e6194b", "human")

	resp, raw := call(t, ts, "POST", "/api/leave", ada.Token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("leave: status %d, body %s", resp.StatusCode, raw)
	}
	if resp, _ := call(t, ts, "GET", "/api/whoami", ada.Token, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("token still valid after leaving: status %d", resp.StatusCode)
	}
	restJoin(t, ts, "ada", "#e6194b", "llm") // identity is free again
}

func TestPaletteReflectsTakenColors(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	type palette struct {
		Free        []string `json:"free"`
		Taken       []string `json:"taken"`
		MinDistance float64  `json:"min_distance"`
	}
	got := decode[palette](t, mustGet(t, ts, "/api/palette"))
	if len(got.Free) != len(Palette) || len(got.Taken) != 0 || got.MinDistance != 40 {
		t.Fatalf("fresh palette = %+v", got)
	}

	restJoin(t, ts, "ada", Palette[0], "human")
	got = decode[palette](t, mustGet(t, ts, "/api/palette"))
	if len(got.Taken) != 1 || got.Taken[0] != Palette[0] {
		t.Errorf("taken = %v; want [%s]", got.Taken, Palette[0])
	}
	if len(got.Free) != len(Palette)-1 {
		t.Errorf("free = %d; want %d", len(got.Free), len(Palette)-1)
	}
}

func TestAccessTokenGate(t *testing.T) {
	ts, _ := newTestServer(t, func(s *Server) { s.accessToken = "s3cret" })

	resp, _ := call(t, ts, "POST", "/api/join", "", joinRequest{Handle: "ada", Color: "#e6194b"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("join without the access token: status %d; want 403", resp.StatusCode)
	}

	req, _ := http.NewRequest("POST", ts.URL+"/api/join",
		bytes.NewReader([]byte(`{"handle":"ada","color":"#e6194b"}`)))
	req.Header.Set("X-Access-Token", "s3cret")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("join with the access token: status %d; want 201", resp.StatusCode)
	}
}

func TestHealthAndWhoami(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	ada := restJoin(t, ts, "ada", "#e6194b", "human")

	health := decode[struct {
		OK      bool  `json:"ok"`
		Users   int   `json:"users"`
		Cursor  int64 `json:"cursor"`
		History int   `json:"history"`
	}](t, mustGet(t, ts, "/api/health"))
	if !health.OK || health.Users != 1 || health.Cursor != 1 || health.History != 100 {
		t.Errorf("health = %+v", health)
	}

	// The full whoami shape, cursors included, is checked in mentions_test.go.
	_, raw := call(t, ts, "GET", "/api/whoami", ada.Token, nil)
	if self := decode[struct {
		Self User `json:"self"`
	}](t, raw).Self; self.Handle != "ada" {
		t.Errorf("whoami = %+v", self)
	}
}

func TestWebClientIsServed(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	// "/" negotiates (see docs_test.go); the assets are served unconditionally.
	for _, path := range []string{"/", "/app.js", "/style.css", "/favicon.svg"} {
		resp, _ := get(t, ts, path, browserAccept)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s: status %d", path, resp.StatusCode)
		}
	}
	resp, err := ts.Client().Get(ts.URL + "/nope")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /nope: status %d; want 404", resp.StatusCode)
	}
}

func TestRESTMethodAndPathErrors(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	cases := []struct {
		method, path string
		want         int
	}{
		{"GET", "/api/join", http.StatusMethodNotAllowed},
		{"GET", "/api/leave", http.StatusMethodNotAllowed},
		{"PUT", "/api/messages", http.StatusMethodNotAllowed},
		{"POST", "/api/users", http.StatusMethodNotAllowed},
		{"GET", "/api/nope", http.StatusNotFound},
	}
	for _, tc := range cases {
		resp, raw := call(t, ts, tc.method, tc.path, "", nil)
		if resp.StatusCode != tc.want {
			t.Errorf("%s %s: status %d, want %d; body %s", tc.method, tc.path, resp.StatusCode, tc.want, raw)
		}
	}
}

func mustGet(t *testing.T, ts *httptest.Server, path string) []byte {
	t.Helper()
	resp, err := ts.Client().Get(ts.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d, body %s", path, resp.StatusCode, raw)
	}
	return raw
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestRESTRateLimitReturns429WithRetryAfter(t *testing.T) {
	ts, _ := newTestServer(t, func(s *Server) {
		s.cfg.Rate, s.cfg.Burst = 60, 2
		s.hub.cfg = s.cfg // the hub enforces it
	})
	ada := restJoin(t, ts, "ada", "#e6194b", "human")

	for i := 0; i < 2; i++ {
		resp, raw := call(t, ts, "POST", "/api/messages", ada.Token, postRequest{Text: "burst"})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("message %d: status %d, body %s", i+1, resp.StatusCode, raw)
		}
	}

	resp, raw := call(t, ts, "POST", "/api/messages", ada.Token, postRequest{Text: "too much"})
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status %d, want 429; body %s", resp.StatusCode, raw)
	}
	if got := resp.Header.Get("Retry-After"); got == "" || got == "0" {
		t.Errorf("Retry-After = %q; want at least 1 second", got)
	}
	if msg := decode[map[string]string](t, raw)["error"]; !strings.Contains(msg, "too fast") {
		t.Errorf("error = %q", msg)
	}

	// The refused message was never published.
	_, raw = call(t, ts, "GET", "/api/messages?since=0", ada.Token, nil)
	for _, ev := range decode[readResponse](t, raw).Events {
		if ev.Text == "too much" {
			t.Error("a throttled message reached the transcript")
		}
	}
}

func TestGuideDocumentsRateLimit(t *testing.T) {
	ts, _ := newTestServer(t, func(s *Server) {
		s.cfg.Rate, s.cfg.Burst = 45, 7
	})
	_, body := get(t, ts, "/api", "*/*")
	for _, want := range []string{"45 messages per minute", "bursts of 7", "429", "Retry-After", "retry_after"} {
		if !strings.Contains(body, want) {
			t.Errorf("guide does not mention %q", want)
		}
	}

	off, _ := newTestServer(t, nil) // testConfig leaves the limit disabled
	if _, body = get(t, off, "/api", "*/*"); strings.Contains(body, "rate limited") {
		t.Error("guide advertises a rate limit that is switched off")
	}
}
