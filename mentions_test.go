package main

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseMentions(t *testing.T) {
	cases := []struct {
		text string
		want []string
	}{
		{"@ada can you look?", []string{"ada"}},
		{"hey @ada and @Bob", []string{"ada", "bob"}},
		{"@ADA @ada @Ada", []string{"ada"}},       // deduped, case-folded
		{"ask @ada.", []string{"ada"}},            // trailing prose punctuation
		{"(@ada) [@bob]", []string{"ada", "bob"}}, // brackets are boundaries
		{"@gpt-4o and @claude.v2 are fine", []string{"gpt-4o", "claude.v2"}},
		{"@everyone listen", []string{"everyone"}},
		{"mail me at someone@example.com", nil},         // not a mention
		{"a@b", nil},                                    // no boundary before @
		{"no mentions here", nil},                       // nothing to find
		{"@ 	ada", nil},                                 // @ alone
		{"@a", nil},                                     // shorter than a legal handle
		{"@" + strings.Repeat("x", 25), nil},            // longer than a legal handle
		{"email a@b.com but tag @ada", []string{"ada"}}, // both in one line
		{"@_leading is not a handle", nil},              // must start alphanumeric
		{"multiline\n@ada", []string{"ada"}},            // newline is a boundary
	}
	for _, tc := range cases {
		if got := ParseMentions(tc.text); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ParseMentions(%q) = %v; want %v", tc.text, got, tc.want)
		}
	}
}

func TestMentionsHandle(t *testing.T) {
	if !MentionsHandle([]string{"ada"}, "ADA", true) {
		t.Error("mention matching should be case-insensitive")
	}
	if MentionsHandle([]string{"ada"}, "bob", true) {
		t.Error("bob is not mentioned")
	}
	if !MentionsHandle([]string{"everyone"}, "bob", true) {
		t.Error("@everyone should reach bob")
	}
	if MentionsHandle([]string{"everyone"}, "bob", false) {
		t.Error("@everyone should be ignored when broadcast is off")
	}
}

func TestPostRecordsMentions(t *testing.T) {
	h := NewHub(testConfig())
	s, _ := h.Join("ada", "#e6194b", "human")
	ev, err := h.Post(s.token, "@Bob and @everyone, look at this")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ev.Mentions, []string{"bob", "everyone"}) {
		t.Errorf("mentions = %v", ev.Mentions)
	}
	// A message with no tags carries no mentions field at all.
	ev, _ = h.Post(s.token, "nothing to see")
	if ev.Mentions != nil {
		t.Errorf("mentions = %v; want nil", ev.Mentions)
	}
}

func TestHubMentionsQuery(t *testing.T) {
	h := NewHub(testConfig())
	ada, _ := h.Join("ada", "#e6194b", "human")
	bob, _ := h.Join("bob", "#3cb44b", "llm")

	h.Post(ada.token, "plain message")
	tagged, _ := h.Post(ada.token, "@bob please review")
	h.Post(ada.token, "@carol is not here but still tagged")
	broadcast, _ := h.Post(ada.token, "@everyone standup")
	ownBroadcast, _ := h.Post(bob.token, "@everyone I am bob and I shout")

	q := MentionQuery{Handle: "bob", IncludeBroadcast: true}
	events, _, _ := h.Mentions(q)
	if len(events) != 2 || events[0].Seq != tagged.Seq || events[1].Seq != broadcast.Seq {
		t.Fatalf("bob's mentions = %v; want ada's two", seqs(events))
	}

	// Bob's own broadcast is not somebody calling bob.
	q.IncludeSelf = true
	if events, _, _ := h.Mentions(q); len(events) != 3 || events[2].Seq != ownBroadcast.Seq {
		t.Errorf("include_self mentions = %v; want bob's own too", seqs(events))
	}
	q.IncludeSelf = false

	q.IncludeBroadcast = false
	if events, _, _ = h.Mentions(q); len(events) != 1 || events[0].Seq != tagged.Seq {
		t.Errorf("bob's direct mentions = %v", seqs(events))
	}

	// Someone who never joined can still have been tagged.
	if events, _, _ := h.Mentions(MentionQuery{Handle: "carol"}); len(events) != 1 {
		t.Errorf("carol's mentions = %v", seqs(events))
	}

	// since skips what you have already seen.
	q = MentionQuery{Handle: "bob", Since: tagged.Seq, IncludeBroadcast: true}
	if events, _, _ := h.Mentions(q); len(events) != 1 || events[0].Seq != broadcast.Seq {
		t.Errorf("mentions after seq %d = %v", tagged.Seq, seqs(events))
	}

	// from filters by time.
	q = MentionQuery{Handle: "bob", IncludeBroadcast: true, From: time.Now().UTC().Add(time.Minute)}
	if events, _, _ := h.Mentions(q); len(events) != 0 {
		t.Errorf("mentions from the future = %v", seqs(events))
	}
	q.From = time.Now().UTC().Add(-time.Minute)
	if events, _, _ := h.Mentions(q); len(events) != 2 {
		t.Errorf("mentions from a minute ago = %v", seqs(events))
	}
}

func seqs(events []*Event) []int64 {
	out := make([]int64, len(events))
	for i, ev := range events {
		out[i] = ev.Seq
	}
	return out
}

func TestServerCursorsTrackPostAndRead(t *testing.T) {
	h := NewHub(testConfig())
	ada, _ := h.Join("ada", "#e6194b", "human")
	bob, _ := h.Join("bob", "#3cb44b", "llm")

	if c, _ := h.Cursors(bob.token); c.LastPost != 0 || c.LastRead != 0 {
		t.Fatalf("fresh session cursors = %+v; want zeroes", c)
	}

	mine, _ := h.Post(bob.token, "hello")
	c, _ := h.Cursors(bob.token)
	if c.LastPost != mine.Seq {
		t.Errorf("last post = %d; want %d", c.LastPost, mine.Seq)
	}
	if c.LastRead != mine.Seq {
		t.Errorf("posting should count as having read your own message, got %d", c.LastRead)
	}

	h.Post(ada.token, "a reply")
	_, cursor, _ := h.History(0)
	h.MarkRead(bob.token, cursor)
	if c, _ := h.Cursors(bob.token); c.LastRead != cursor {
		t.Errorf("last read = %d; want %d", c.LastRead, cursor)
	}
	// Cursors never move backwards.
	h.MarkRead(bob.token, 1)
	if c, _ := h.Cursors(bob.token); c.LastRead != cursor {
		t.Errorf("last read went backwards to %d", c.LastRead)
	}
}

// ---------- REST ----------

func TestRESTSinceKeywords(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	ada := restJoin(t, ts, "ada", "#e6194b", "human")
	bob := restJoin(t, ts, "bob", "#3cb44b", "llm")

	call(t, ts, "POST", "/api/messages", bob.Token, postRequest{Text: "bob speaks"})
	call(t, ts, "POST", "/api/messages", ada.Token, postRequest{Text: "ada answers"})
	call(t, ts, "POST", "/api/messages", ada.Token, postRequest{Text: "and again"})

	// last-read starts at bob's own message (posting counts as reading it) and
	// advances every time a read returns.
	_, raw := call(t, ts, "GET", "/api/messages?since=last-read", bob.Token, nil)
	got := decode[readResponse](t, raw)
	if len(got.Events) != 2 || got.Events[0].Text != "ada answers" {
		t.Fatalf("first since=last-read returned %d events: %s", len(got.Events), raw)
	}
	_, raw = call(t, ts, "GET", "/api/messages?since=last-read", bob.Token, nil)
	if got = decode[readResponse](t, raw); len(got.Events) != 0 {
		t.Errorf("second since=last-read returned %d events; want none", len(got.Events))
	}

	call(t, ts, "POST", "/api/messages", ada.Token, postRequest{Text: "something new"})
	_, raw = call(t, ts, "GET", "/api/messages?since=last-read", bob.Token, nil)
	if got = decode[readResponse](t, raw); len(got.Events) != 1 || got.Events[0].Text != "something new" {
		t.Errorf("since=last-read missed the new message: %s", raw)
	}

	// last-post does not move: it is still bob's own message, so it replays
	// everything said since, however many times it is asked.
	for i := 0; i < 2; i++ {
		_, raw = call(t, ts, "GET", "/api/messages?since=last-post", bob.Token, nil)
		got = decode[readResponse](t, raw)
		if len(got.Events) != 3 || got.Events[0].Text != "ada answers" {
			t.Fatalf("since=last-post (attempt %d) returned %d events: %s", i+1, len(got.Events), raw)
		}
	}

	resp, _ := call(t, ts, "GET", "/api/messages?since=whenever", bob.Token, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown keyword: status %d; want 400", resp.StatusCode)
	}
}

func TestRESTWhoamiReportsCursors(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	ada := restJoin(t, ts, "ada", "#e6194b", "human")
	_, raw := call(t, ts, "POST", "/api/messages", ada.Token, postRequest{Text: "hi"})
	posted := decode[map[string]any](t, raw)["seq"].(float64)

	_, raw = call(t, ts, "GET", "/api/whoami", ada.Token, nil)
	who := decode[struct {
		Self        User  `json:"self"`
		LastPostSeq int64 `json:"last_post_seq"`
		LastReadSeq int64 `json:"last_read_seq"`
		Cursor      int64 `json:"cursor"`
	}](t, raw)
	if who.Self.Handle != "ada" || who.LastPostSeq != int64(posted) || who.Cursor != int64(posted) {
		t.Errorf("whoami = %+v", who)
	}
}

func TestRESTMentionsEndpoint(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	ada := restJoin(t, ts, "ada", "#e6194b", "human")
	bob := restJoin(t, ts, "bob", "#3cb44b", "llm")

	call(t, ts, "POST", "/api/messages", ada.Token, postRequest{Text: "just talking"})
	call(t, ts, "POST", "/api/messages", ada.Token, postRequest{Text: "@bob take a look"})
	call(t, ts, "POST", "/api/messages", ada.Token, postRequest{Text: "@everyone heads up"})

	// Defaults to your own handle, broadcasts included.
	_, raw := call(t, ts, "GET", "/api/mentions", bob.Token, nil)
	got := decode[mentionsResponse](t, raw)
	if got.Handle != "bob" || len(got.Events) != 2 {
		t.Fatalf("mentions = %+v (%d events)", got.Handle, len(got.Events))
	}
	if !reflect.DeepEqual(got.Events[0].Mentions, []string{"bob"}) {
		t.Errorf("first mention event = %+v", got.Events[0])
	}

	// Broadcasts can be excluded.
	_, raw = call(t, ts, "GET", "/api/mentions?broadcast=false", bob.Token, nil)
	if got = decode[mentionsResponse](t, raw); len(got.Events) != 1 {
		t.Errorf("broadcast=false returned %d events", len(got.Events))
	}

	// Anyone can ask about anyone.
	_, raw = call(t, ts, "GET", "/api/mentions?handle=bob&broadcast=false", ada.Token, nil)
	if got = decode[mentionsResponse](t, raw); got.Handle != "bob" || len(got.Events) != 1 {
		t.Errorf("ada asking about bob = %+v", got)
	}

	// Your own broadcast does not count as somebody calling you.
	call(t, ts, "POST", "/api/messages", bob.Token, postRequest{Text: "@everyone bob here"})
	_, raw = call(t, ts, "GET", "/api/mentions", bob.Token, nil)
	if got = decode[mentionsResponse](t, raw); len(got.Events) != 2 {
		t.Errorf("bob's own @everyone leaked into his mentions: %d events", len(got.Events))
	}
	_, raw = call(t, ts, "GET", "/api/mentions?include_self=true", bob.Token, nil)
	if got = decode[mentionsResponse](t, raw); len(got.Events) != 3 {
		t.Errorf("include_self=true returned %d events; want 3", len(got.Events))
	}

	// Reading mentions must not move the read cursor: it sits where bob's own
	// message left it, and several mentions reads later it has not budged.
	before := lastReadSeq(t, ts, bob.Token)
	call(t, ts, "GET", "/api/mentions", bob.Token, nil)
	call(t, ts, "GET", "/api/mentions?from=1h", bob.Token, nil)
	if after := lastReadSeq(t, ts, bob.Token); after != before {
		t.Errorf("reading mentions moved the read cursor from %d to %d", before, after)
	}

	// ...so an ordinary read still delivers what bob has genuinely not seen.
	call(t, ts, "POST", "/api/messages", ada.Token, postRequest{Text: "the last word"})
	_, raw = call(t, ts, "GET", "/api/messages?since=last-read", bob.Token, nil)
	if events := decode[readResponse](t, raw).Events; len(events) != 1 ||
		events[0].Text != "the last word" {
		t.Errorf("after reading mentions, since=last-read returned %s", raw)
	}
}

func lastReadSeq(t *testing.T, ts *httptest.Server, token string) int64 {
	t.Helper()
	_, raw := call(t, ts, "GET", "/api/whoami", token, nil)
	return int64(decode[map[string]any](t, raw)["last_read_seq"].(float64))
}

func TestRESTMentionsFromTime(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	ada := restJoin(t, ts, "ada", "#e6194b", "human")
	bob := restJoin(t, ts, "bob", "#3cb44b", "llm")

	call(t, ts, "POST", "/api/messages", ada.Token, postRequest{Text: "@bob early"})
	cut := time.Now().UTC()
	time.Sleep(30 * time.Millisecond)
	call(t, ts, "POST", "/api/messages", ada.Token, postRequest{Text: "@bob late"})

	// RFC3339 boundary.
	_, raw := call(t, ts, "GET", "/api/mentions?from="+cut.Format(time.RFC3339Nano), bob.Token, nil)
	got := decode[mentionsResponse](t, raw)
	if len(got.Events) != 1 || got.Events[0].Text != "@bob late" {
		t.Fatalf("from=<timestamp> returned %d events: %s", len(got.Events), raw)
	}

	// Duration form: everything in the last hour.
	_, raw = call(t, ts, "GET", "/api/mentions?from=1h", bob.Token, nil)
	if got = decode[mentionsResponse](t, raw); len(got.Events) != 2 {
		t.Errorf("from=1h returned %d events", len(got.Events))
	}

	// Nothing in the last nanosecond.
	_, raw = call(t, ts, "GET", "/api/mentions?from=1ns", bob.Token, nil)
	if got = decode[mentionsResponse](t, raw); len(got.Events) != 0 {
		t.Errorf("from=1ns returned %d events", len(got.Events))
	}

	if resp, _ := call(t, ts, "GET", "/api/mentions?from=yesterday", bob.Token, nil); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("from=yesterday: status %d; want 400", resp.StatusCode)
	}
}

func TestRESTMentionsWaitBlocksUntilTagged(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	ada := restJoin(t, ts, "ada", "#e6194b", "human")
	bob := restJoin(t, ts, "bob", "#3cb44b", "llm")

	done := make(chan mentionsResponse, 1)
	go func() {
		_, raw := call(t, ts, "GET", "/api/mentions?wait=5", bob.Token, nil)
		done <- decode[mentionsResponse](t, raw)
	}()

	time.Sleep(150 * time.Millisecond)
	// Chatter that does not tag bob must not wake the call.
	call(t, ts, "POST", "/api/messages", ada.Token, postRequest{Text: "unrelated"})
	call(t, ts, "POST", "/api/messages", ada.Token, postRequest{Text: "@carol not you"})
	select {
	case got := <-done:
		t.Fatalf("woke up on a message that did not tag bob: %+v", got.Events)
	case <-time.After(300 * time.Millisecond):
	}

	call(t, ts, "POST", "/api/messages", ada.Token, postRequest{Text: "@bob finally"})
	select {
	case got := <-done:
		if len(got.Events) != 1 || got.Events[0].Text != "@bob finally" {
			t.Errorf("mentions wait returned %+v", got.Events)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("mentions wait never returned")
	}
}

func TestRESTMentionsWaitTimesOut(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	bob := restJoin(t, ts, "bob", "#3cb44b", "llm")
	start := time.Now()
	_, raw := call(t, ts, "GET", "/api/mentions?wait=1", bob.Token, nil)
	if took := time.Since(start); took < 900*time.Millisecond {
		t.Errorf("returned after %v; want it to wait ~1s", took)
	}
	if got := decode[mentionsResponse](t, raw); len(got.Events) != 0 {
		t.Errorf("events = %+v; want none", got.Events)
	}
}

func TestGuideDocumentsMentionsAndCursors(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	_, body := get(t, ts, "/api", "*/*")
	for _, want := range []string{
		"MENTIONS", "@handle", "/api/mentions", "since=last-read", "since=last-post",
		"broadcast=false", "RFC3339", "does NOT move your read cursor",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("guide does not mention %q", want)
		}
	}
}
