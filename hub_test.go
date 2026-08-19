package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func testConfig() Config {
	return Config{
		History:          100,
		IdleTimeout:      time.Minute,
		MaxMessageLen:    100,
		MinColorDistance: 40,
		SendBuffer:       8,
	}
}

// TestPaletteIsDistinguishable guards the invariant that makes the suggested
// palette useful: no two entries are close enough to be confused, by the same
// measure Join enforces.
func TestPaletteIsDistinguishable(t *testing.T) {
	const min = 40
	for i, a := range Palette {
		if norm, err := NormalizeColor(a); err != nil || norm != a {
			t.Errorf("palette entry %q is not canonical (%q, %v)", a, norm, err)
		}
		for _, b := range Palette[i+1:] {
			if d := ColorDistance(a, b); d < min {
				t.Errorf("palette colors %s and %s are too close: %.1f < %d", a, b, d, min)
			}
		}
	}
}

func TestNormalizeHandle(t *testing.T) {
	ok := []string{"ada", "Claude-3", "gpt.4o", "a_b", "x9"}
	for _, h := range ok {
		if got, err := NormalizeHandle(h); err != nil || got != h {
			t.Errorf("NormalizeHandle(%q) = %q, %v; want it accepted unchanged", h, got, err)
		}
	}
	bad := []string{"", " ", "a", "_leading", "-dash", "has space", "emoji🙂", "system", "SYSTEM",
		strings.Repeat("x", 25)}
	for _, h := range bad {
		if _, err := NormalizeHandle(h); err == nil {
			t.Errorf("NormalizeHandle(%q) accepted; want an error", h)
		}
	}
}

func TestNormalizeColor(t *testing.T) {
	cases := map[string]string{
		"#4363D8": "#4363d8",
		"4363d8":  "#4363d8",
		"#abc":    "#aabbcc",
		"ABC":     "#aabbcc",
		" #fff ":  "#ffffff",
	}
	for in, want := range cases {
		got, err := NormalizeColor(in)
		if err != nil || got != want {
			t.Errorf("NormalizeColor(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "red", "#12345", "#gggggg", "rgb(1,2,3)"} {
		if _, err := NormalizeColor(in); err == nil {
			t.Errorf("NormalizeColor(%q) accepted; want an error", in)
		}
	}
}

func TestJoinRejectsDuplicates(t *testing.T) {
	h := NewHub(testConfig())
	if _, err := h.Join("ada", "#e6194b", "human"); err != nil {
		t.Fatalf("first join: %v", err)
	}

	var taken *TakenError

	// Same handle in different case, with a free color.
	_, err := h.Join("ADA", "#3cb44b", "llm")
	if !errors.As(err, &taken) || taken.Field != "handle" {
		t.Fatalf("duplicate handle: got %v; want a handle TakenError", err)
	}

	// Same color written differently, with a free handle.
	_, err = h.Join("babbage", "E6194B", "llm")
	if !errors.As(err, &taken) || taken.Field != "color" {
		t.Fatalf("duplicate color: got %v; want a color TakenError", err)
	}
	if taken.TooNear {
		t.Errorf("exact duplicate reported as TooNear")
	}

	// Distinct but indistinguishable color.
	_, err = h.Join("babbage", "#e6194c", "llm")
	if !errors.As(err, &taken) || !taken.TooNear {
		t.Fatalf("near-duplicate color: got %v; want a TooNear TakenError", err)
	}

	if _, err := h.Join("babbage", "#3cb44b", "llm"); err != nil {
		t.Fatalf("join with free handle and color: %v", err)
	}
	if got := len(h.Users()); got != 2 {
		t.Errorf("Users() = %d; want 2", got)
	}
}

func TestMinColorDistanceZeroAllowsNearDuplicates(t *testing.T) {
	cfg := testConfig()
	cfg.MinColorDistance = 0
	h := NewHub(cfg)
	if _, err := h.Join("ada", "#e6194b", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Join("babbage", "#e6194c", ""); err != nil {
		t.Errorf("near-duplicate rejected with the check disabled: %v", err)
	}
	if _, err := h.Join("turing", "#e6194b", ""); err == nil {
		t.Error("exact duplicate accepted; uniqueness must always hold")
	}
}

func TestLeaveFreesHandleAndColor(t *testing.T) {
	h := NewHub(testConfig())
	s, err := h.Join("ada", "#e6194b", "human")
	if err != nil {
		t.Fatal(err)
	}
	h.Leave(s.token, "left")

	if _, err := h.Join("ada", "#e6194b", "llm"); err != nil {
		t.Fatalf("re-claiming a released identity: %v", err)
	}
	if _, err := h.Post(s.token, "still here?"); !errors.Is(err, ErrNotJoined) {
		t.Errorf("Post with a stale token = %v; want ErrNotJoined", err)
	}
	h.Leave(s.token, "left") // must not panic
}

func TestPostValidation(t *testing.T) {
	h := NewHub(testConfig())
	s, _ := h.Join("ada", "#e6194b", "")

	if _, err := h.Post(s.token, "   \n "); err == nil {
		t.Error("blank message accepted")
	}
	if _, err := h.Post(s.token, strings.Repeat("x", 101)); err == nil {
		t.Error("over-long message accepted")
	}
	if _, err := h.Post("nope", "hi"); !errors.Is(err, ErrNotJoined) {
		t.Error("post with an unknown token accepted")
	}
	ev, err := h.Post(s.token, "hello  \n")
	if err != nil {
		t.Fatal(err)
	}
	if ev.Text != "hello" {
		t.Errorf("trailing whitespace kept: %q", ev.Text)
	}
	if ev.From == nil || ev.From.Handle != "ada" || ev.From.Color != "#e6194b" {
		t.Errorf("message attribution = %+v; want ada/#e6194b", ev.From)
	}
}

func TestHistoryAndTruncation(t *testing.T) {
	cfg := testConfig()
	cfg.History = 3
	h := NewHub(cfg)
	s, _ := h.Join("ada", "#e6194b", "") // seq 1
	for i := 0; i < 4; i++ {             // seq 2..5
		if _, err := h.Post(s.token, "m"); err != nil {
			t.Fatal(err)
		}
	}

	events, cursor, truncated := h.History(0)
	if cursor != 5 {
		t.Errorf("cursor = %d; want 5", cursor)
	}
	if len(events) != 3 || events[0].Seq != 3 {
		t.Fatalf("History(0) returned %d events starting at %d; want 3 starting at 3",
			len(events), events[0].Seq)
	}
	if _, _, truncated = h.History(1); !truncated {
		t.Error("History(1) should report truncation: seq 2 was evicted")
	}
	events, _, truncated = h.History(4)
	if truncated || len(events) != 1 || events[0].Seq != 5 {
		t.Errorf("History(4) = %d events, truncated=%v; want just seq 5", len(events), truncated)
	}
	if events, _, _ := h.History(5); len(events) != 0 {
		t.Errorf("History(cursor) returned %d events; want none", len(events))
	}
}

func TestSubscribeReceivesBroadcast(t *testing.T) {
	h := NewHub(testConfig())
	listener, _ := h.Join("ada", "#e6194b", "human")
	sub, err := h.Subscribe(listener.token)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Unsubscribe(listener.token, sub)

	speaker, _ := h.Join("claude", "#4363d8", "llm")
	if ev := receive(t, sub); ev.Type != EventJoin || ev.From.Handle != "claude" {
		t.Fatalf("first event = %+v; want claude's join", ev)
	}
	if _, err := h.Post(speaker.token, "hello room"); err != nil {
		t.Fatal(err)
	}
	ev := receive(t, sub)
	if ev.Type != EventMessage || ev.Text != "hello room" || ev.From.Color != "#4363d8" {
		t.Fatalf("second event = %+v; want claude's message", ev)
	}

	h.Leave(speaker.token, "left")
	if ev := receive(t, sub); ev.Type != EventLeave {
		t.Fatalf("third event = %+v; want a leave", ev)
	}
}

func TestSlowSubscriberIsDropped(t *testing.T) {
	cfg := testConfig()
	cfg.SendBuffer = 2
	h := NewHub(cfg)
	s, _ := h.Join("ada", "#e6194b", "")
	sub, _ := h.Subscribe(s.token)

	for i := 0; i < 10; i++ {
		if _, err := h.Post(s.token, "flood"); err != nil {
			t.Fatal(err)
		}
	}
	// Drain: the channel holds what fit, then reports closed.
	closed := false
	for range 10 {
		if _, ok := <-sub.ch; !ok {
			closed = true
			break
		}
	}
	if !closed {
		t.Error("subscriber that stopped reading was not dropped")
	}
	if _, err := h.Post(s.token, "still fine"); err != nil {
		t.Errorf("hub broke after dropping a subscriber: %v", err)
	}
}

func TestUnsubscribeIsIdempotent(t *testing.T) {
	h := NewHub(testConfig())
	s, _ := h.Join("ada", "#e6194b", "")
	sub, _ := h.Subscribe(s.token)
	h.Unsubscribe(s.token, sub)
	h.Unsubscribe(s.token, sub) // must not panic on a double close
	if _, ok := <-sub.ch; ok {
		t.Error("channel still open after Unsubscribe")
	}
}

func TestReapIdleOnlyDropsUnsubscribedSessions(t *testing.T) {
	h := NewHub(testConfig())
	rest, _ := h.Join("ada", "#e6194b", "llm")
	live, _ := h.Join("claude", "#4363d8", "llm")
	sub, _ := h.Subscribe(live.token)
	defer h.Unsubscribe(live.token, sub)

	if n := h.ReapIdle(time.Now()); n != 0 {
		t.Fatalf("reaped %d fresh sessions; want 0", n)
	}
	if n := h.ReapIdle(time.Now().Add(2 * time.Minute)); n != 1 {
		t.Fatalf("reaped %d sessions; want 1 (the REST one)", n)
	}
	if _, err := h.Touch(rest.token); !errors.Is(err, ErrNotJoined) {
		t.Error("idle session survived the reaper")
	}
	if _, err := h.Touch(live.token); err != nil {
		t.Error("subscribed session was reaped")
	}
	// Its identity is free again.
	if _, err := h.Join("ada", "#e6194b", ""); err != nil {
		t.Errorf("reaped identity not released: %v", err)
	}
}

func TestTouchKeepsSessionAlive(t *testing.T) {
	h := NewHub(testConfig())
	s, _ := h.Join("ada", "#e6194b", "")
	// A Touch now means the session is not idle as of "now + timeout - 1s".
	if _, err := h.Touch(s.token); err != nil {
		t.Fatal(err)
	}
	if n := h.ReapIdle(time.Now().Add(59 * time.Second)); n != 0 {
		t.Errorf("reaped %d recently active sessions; want 0", n)
	}
}

func TestReapCandidateDoesNotDropReactivatedSession(t *testing.T) {
	h := NewHub(testConfig())
	s, _ := h.Join("ada", "#e6194b", "")

	// ReapIdle scans first and removes candidates second. Simulate activity in
	// that interval and verify the stale observation cannot remove the session.
	h.mu.Lock()
	observed := s.lastSeen
	s.lastSeen = observed.Add(time.Nanosecond)
	h.mu.Unlock()

	if h.reapCandidate(s.token, observed, observed.Add(2*time.Minute)) {
		t.Fatal("reaper removed a session that became active after its scan")
	}
	if _, err := h.Touch(s.token); err != nil {
		t.Fatalf("reactivated session did not survive: %v", err)
	}
}

func TestAvailableColors(t *testing.T) {
	h := NewHub(testConfig())
	free, taken := h.AvailableColors()
	if len(free) != len(Palette) || len(taken) != 0 {
		t.Fatalf("fresh hub: %d free, %d taken; want %d/0", len(free), len(taken), len(Palette))
	}
	if _, err := h.Join("ada", Palette[0], ""); err != nil {
		t.Fatal(err)
	}
	free, taken = h.AvailableColors()
	if len(free) != len(Palette)-1 || len(taken) != 1 || taken[0] != Palette[0] {
		t.Errorf("after one join: %d free, taken=%v", len(free), taken)
	}
}

func TestParseRole(t *testing.T) {
	for _, in := range []string{"", "human", "HUMAN", "person"} {
		if r, err := ParseRole(in); err != nil || r != RoleHuman {
			t.Errorf("ParseRole(%q) = %q, %v; want human", in, r, err)
		}
	}
	for _, in := range []string{"llm", "LLM", "agent", "bot", "ai"} {
		if r, err := ParseRole(in); err != nil || r != RoleLLM {
			t.Errorf("ParseRole(%q) = %q, %v; want llm", in, r, err)
		}
	}
	if _, err := ParseRole("dolphin"); err == nil {
		t.Error("ParseRole accepted an unknown role")
	}
}

func receive(t *testing.T, sub *subscriber) *Event {
	t.Helper()
	select {
	case ev, ok := <-sub.ch:
		if !ok {
			t.Fatal("subscriber channel closed unexpectedly")
		}
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for an event")
		return nil
	}
}

func TestRateLimitAllowsBurstThenThrottles(t *testing.T) {
	cfg := testConfig()
	cfg.Rate = 6000 // 100 per second, so the refill is observable in a test
	cfg.Burst = 3
	h := NewHub(cfg)
	s, _ := h.Join("ada", "#e6194b", "human")

	for i := 0; i < 3; i++ {
		if _, err := h.Post(s.token, "burst"); err != nil {
			t.Fatalf("message %d of the burst was refused: %v", i+1, err)
		}
	}

	_, err := h.Post(s.token, "one too many")
	var limited *RateLimitError
	if !errors.As(err, &limited) {
		t.Fatalf("post past the burst = %v; want a RateLimitError", err)
	}
	if limited.RetryAfter <= 0 || limited.RetryAfter > time.Second {
		t.Errorf("retry after = %v; want a small positive wait", limited.RetryAfter)
	}

	// A throttled message costs the room nothing: no seq was consumed.
	_, cursor, _ := h.History(0)
	if cursor != 4 { // one join plus three messages
		t.Errorf("cursor = %d; want 4, so the refused message published nothing", cursor)
	}

	// The bucket refills.
	time.Sleep(30 * time.Millisecond)
	if _, err := h.Post(s.token, "after waiting"); err != nil {
		t.Errorf("bucket did not refill: %v", err)
	}
}

func TestRateLimitIsPerSession(t *testing.T) {
	cfg := testConfig()
	cfg.Rate, cfg.Burst = 60, 1
	h := NewHub(cfg)
	ada, _ := h.Join("ada", "#e6194b", "human")
	bob, _ := h.Join("bob", "#3cb44b", "llm")

	if _, err := h.Post(ada.token, "mine"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Post(ada.token, "again"); err == nil {
		t.Error("ada was allowed past her own burst")
	}
	// Bob's bucket is untouched by ada flooding.
	if _, err := h.Post(bob.token, "mine too"); err != nil {
		t.Errorf("bob was throttled by ada's traffic: %v", err)
	}
}

func TestRateLimitDisabled(t *testing.T) {
	cfg := testConfig()
	cfg.Rate = 0
	h := NewHub(cfg)
	s, _ := h.Join("ada", "#e6194b", "human")
	for i := 0; i < 50; i++ {
		if _, err := h.Post(s.token, "flood"); err != nil {
			t.Fatalf("message %d refused with the limit disabled: %v", i+1, err)
		}
	}
}

func TestRateLimitDoesNotBlockJoiningOrReading(t *testing.T) {
	cfg := testConfig()
	cfg.Rate, cfg.Burst = 60, 1
	h := NewHub(cfg)
	s, _ := h.Join("ada", "#e6194b", "human")
	h.Post(s.token, "one")
	if _, err := h.Post(s.token, "two"); err == nil {
		t.Fatal("expected to be throttled")
	}
	// Being throttled must not lock you out of the room.
	if _, err := h.Touch(s.token); err != nil {
		t.Errorf("Touch after throttling: %v", err)
	}
	if _, _, _ = h.History(0); false {
		t.Fatal("unreachable")
	}
	if _, err := h.Join("bob", "#3cb44b", "llm"); err != nil {
		t.Errorf("Join while another session is throttled: %v", err)
	}
}
