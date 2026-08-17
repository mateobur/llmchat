package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidateConversationName(t *testing.T) {
	ok := map[string]string{
		"standup":        "standup",
		"standup.json":   "standup", // the extension is added, not repeated
		"2026-08-17_run": "2026-08-17_run",
		" trimmed ":      "trimmed",
	}
	for in, want := range ok {
		got, err := ValidateConversationName(in)
		if err != nil || got != want {
			t.Errorf("ValidateConversationName(%q) = %q, %v; want %q", in, got, err, want)
		}
	}

	// Anything that could escape the chats directory or need quoting.
	bad := []string{
		"", "   ", "../secrets", "chats/../etc/passwd", "/absolute", "with space",
		"back\\slash", ".hidden", "-dash", "a/b", "..", ".",
		strings.Repeat("x", 81),
	}
	for _, in := range bad {
		if got, err := ValidateConversationName(in); err == nil {
			t.Errorf("ValidateConversationName(%q) = %q; want an error", in, got)
		}
	}
}

func TestGenerateConversationName(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 35, 2, 0, time.UTC)
	name := GenerateConversationName(now)
	if !strings.HasPrefix(name, "2026-08-17T14-35-02-") {
		t.Fatalf("name = %q; want the date first so listings sort", name)
	}
	if _, err := ValidateConversationName(name); err != nil {
		t.Errorf("generated name is not a valid one: %v", err)
	}
	if other := GenerateConversationName(now); other == name {
		t.Error("two names generated in the same second collided")
	}
}

func TestBuildTranscriptSummarizesParticipants(t *testing.T) {
	h := NewHub(testConfig())
	ada, _ := h.Join("ada", "#e6194b", "human")
	bob, _ := h.Join("bob", "#3cb44b", "llm")
	h.Post(ada.token, "hello")
	h.Post(bob.token, "hi")
	h.Post(ada.token, "@bob how are you")
	h.Leave(bob.token, "left")

	events, _, _ := h.History(0)
	tr := BuildTranscript("standup", events, true)

	if tr.Conversation != "standup" || tr.Server != "llmchat" || !tr.Complete {
		t.Errorf("header = %+v", tr)
	}
	if tr.EventCount != len(events) || tr.FirstSeq != 1 || tr.LastSeq != int64(len(events)) {
		t.Errorf("counts = %d events, seq %d..%d", tr.EventCount, tr.FirstSeq, tr.LastSeq)
	}
	if tr.StartedAt != events[0].TS {
		t.Errorf("started_at = %v; want the first event's timestamp", tr.StartedAt)
	}

	if len(tr.Participants) != 2 {
		t.Fatalf("participants = %+v", tr.Participants)
	}
	first, second := tr.Participants[0], tr.Participants[1]
	if first.Handle != "ada" || first.Messages != 2 || first.Color != "#e6194b" {
		t.Errorf("ada = %+v; want 2 messages", first)
	}
	// bob has left, and still appears with his colour and his one message.
	if second.Handle != "bob" || second.Messages != 1 || second.Role != RoleLLM {
		t.Errorf("bob = %+v; want 1 message and role llm", second)
	}
	if second.LastSeen.Before(second.FirstSeen) {
		t.Errorf("bob last seen %v before first seen %v", second.LastSeen, second.FirstSeen)
	}
}

func TestBuildTranscriptEmptyRoom(t *testing.T) {
	tr := BuildTranscript("empty", nil, true)
	raw, err := json.Marshal(tr)
	if err != nil {
		t.Fatal(err)
	}
	// Empty must serialize as [] rather than null, so consumers can iterate.
	if !strings.Contains(string(raw), `"events":[]`) {
		t.Errorf("empty transcript = %s", raw)
	}
	if tr.EventCount != 0 || tr.FirstSeq != 0 {
		t.Errorf("counts on an empty transcript = %+v", tr)
	}
}

func TestRecorderWritesEverythingBeyondTheHistory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "chats")
	rec, err := NewRecorder(dir, "run", log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	if rec.Path() != filepath.Join(dir, "run.json") {
		t.Errorf("path = %s", rec.Path())
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("chats directory was not created: %v", err)
	}

	// A history of 3 with 10 messages: the hub forgets, the recorder does not.
	cfg := testConfig()
	cfg.History = 3
	h := NewHub(cfg)
	h.SetRecorder(rec)
	s, _ := h.Join("ada", "#e6194b", "human")
	for i := 0; i < 10; i++ {
		if _, err := h.Post(s.token, "message"); err != nil {
			t.Fatal(err)
		}
	}
	if h.Evicted() == 0 {
		t.Fatal("expected the hub to have evicted events")
	}

	if err := rec.Flush(); err != nil {
		t.Fatal(err)
	}
	tr := readTranscript(t, rec.Path())
	if !tr.Complete {
		t.Error("a recorded transcript should be complete")
	}
	if tr.EventCount != 11 || tr.FirstSeq != 1 || tr.LastSeq != 11 { // join + 10
		t.Errorf("transcript has %d events, seq %d..%d; want 11 from 1", tr.EventCount, tr.FirstSeq, tr.LastSeq)
	}
	if got, _, _ := h.History(0); len(got) != 3 {
		t.Errorf("the hub kept %d events; the window should still be 3", len(got))
	}
}

func TestRecorderFlushIsIdempotentAndAtomic(t *testing.T) {
	dir := t.TempDir()
	rec, err := NewRecorder(dir, "run", log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}

	// Nothing recorded yet: no write, and no file.
	if err := rec.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(rec.Path()); !os.IsNotExist(err) {
		t.Errorf("an empty recorder wrote a file: %v", err)
	}

	rec.Record(&Event{Type: EventSystem, Seq: 1, TS: time.Now().UTC(), Text: "hello"})
	if err := rec.Flush(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(rec.Path())
	if err != nil {
		t.Fatal(err)
	}
	// A second flush with nothing new must not rewrite.
	if err := rec.Flush(); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.Stat(rec.Path()); again.ModTime() != info.ModTime() {
		t.Error("flush rewrote the file with nothing to save")
	}

	// No temporary files left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "run.json" {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("directory contains %v; want only run.json", names)
	}
}

func TestRecorderRefusesToOverwrite(t *testing.T) {
	dir := t.TempDir()
	logger := log.New(io.Discard, "", 0)
	rec, err := NewRecorder(dir, "run", logger)
	if err != nil {
		t.Fatal(err)
	}
	rec.Record(&Event{Type: EventSystem, Seq: 1, TS: time.Now().UTC(), Text: "first run"})
	if err := rec.Flush(); err != nil {
		t.Fatal(err)
	}

	_, err = NewRecorder(dir, "run", logger)
	if err == nil {
		t.Fatal("a second recorder clobbered an existing transcript")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error = %v; want it to say the file exists", err)
	}
	// The original is untouched.
	if tr := readTranscript(t, filepath.Join(dir, "run.json")); tr.Events[0].Text != "first run" {
		t.Errorf("existing transcript was modified: %+v", tr.Events)
	}
}

func TestRecorderRunFlushesOnShutdown(t *testing.T) {
	dir := t.TempDir()
	rec, err := NewRecorder(dir, "run", log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		rec.Run(ctx, time.Hour) // never ticks; only the shutdown path can save
		close(done)
	}()

	rec.Record(&Event{Type: EventMessage, Seq: 1, TS: time.Now().UTC(), Text: "last words"})
	cancel()
	<-done

	if tr := readTranscript(t, rec.Path()); tr.EventCount != 1 || tr.Events[0].Text != "last words" {
		t.Errorf("transcript after shutdown = %+v", tr)
	}
}

// ---------- the HTTP endpoint ----------

func TestTranscriptEndpointNeedsAToken(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	resp, _ := call(t, ts, "GET", "/api/transcript", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status %d; want 401", resp.StatusCode)
	}
}

func TestTranscriptEndpointWithoutPersistence(t *testing.T) {
	ts, hub := newTestServer(t, func(s *Server) { s.conversation = "in-memory-run" })
	ada := restJoin(t, ts, "ada", "#e6194b", "human")
	call(t, ts, "POST", "/api/messages", ada.Token, postRequest{Text: "on the record"})

	resp, raw := call(t, ts, "GET", "/api/transcript", ada.Token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	if got := resp.Header.Get("Content-Disposition"); got != `attachment; filename="in-memory-run.json"` {
		t.Errorf("Content-Disposition = %q", got)
	}

	tr := decode[Transcript](t, raw)
	if tr.Conversation != "in-memory-run" || !tr.Complete {
		t.Errorf("transcript = %+v", tr)
	}
	if tr.EventCount != 2 || tr.Events[1].Text != "on the record" {
		t.Errorf("events = %+v", tr.Events)
	}
	if len(tr.Participants) != 1 || tr.Participants[0].Messages != 1 {
		t.Errorf("participants = %+v", tr.Participants)
	}

	// Once the history has been trimmed, an export from memory admits the gap.
	hub.cfg.History = 2
	for i := 0; i < 5; i++ {
		call(t, ts, "POST", "/api/messages", ada.Token, postRequest{Text: "filler"})
	}
	_, raw = call(t, ts, "GET", "/api/transcript", ada.Token, nil)
	if tr = decode[Transcript](t, raw); tr.Complete {
		t.Error("complete=true after events were evicted")
	}
	if tr.FirstSeq <= 1 {
		t.Errorf("first_seq = %d; want it to show where the record starts", tr.FirstSeq)
	}
}

func TestTranscriptEndpointWithPersistence(t *testing.T) {
	dir := t.TempDir()
	var rec *Recorder
	ts, hub := newTestServer(t, func(s *Server) {
		var err error
		rec, err = NewRecorder(dir, "saved-run", log.New(io.Discard, "", 0))
		if err != nil {
			t.Fatal(err)
		}
		s.conversation, s.recorder = "saved-run", rec
		s.cfg.History = 2
		s.hub.cfg = s.cfg
		s.hub.SetRecorder(rec)
	})
	_ = hub

	ada := restJoin(t, ts, "ada", "#e6194b", "human")
	for i := 0; i < 6; i++ {
		call(t, ts, "POST", "/api/messages", ada.Token, postRequest{Text: "kept"})
	}

	// The download is the whole conversation even though the hub forgot most.
	_, raw := call(t, ts, "GET", "/api/transcript", ada.Token, nil)
	downloaded := decode[Transcript](t, raw)
	if !downloaded.Complete || downloaded.EventCount != 7 {
		t.Fatalf("downloaded %d events, complete=%v; want 7 complete", downloaded.EventCount, downloaded.Complete)
	}

	// And the file on disk says the same thing.
	if err := rec.Flush(); err != nil {
		t.Fatal(err)
	}
	onDisk := readTranscript(t, rec.Path())
	if onDisk.EventCount != downloaded.EventCount || onDisk.Conversation != downloaded.Conversation {
		t.Errorf("file and download disagree: %d vs %d events", onDisk.EventCount, downloaded.EventCount)
	}
	if len(onDisk.Events) != len(downloaded.Events) {
		t.Fatalf("event counts differ")
	}
	for i := range onDisk.Events {
		if onDisk.Events[i].Seq != downloaded.Events[i].Seq {
			t.Errorf("event %d: file seq %d, download seq %d", i, onDisk.Events[i].Seq, downloaded.Events[i].Seq)
		}
	}
}

func TestGuideDocumentsTheTranscript(t *testing.T) {
	off, _ := newTestServer(t, nil)
	_, body := get(t, off, "/api", "*/*")
	for _, want := range []string{"TRANSCRIPT", "/api/transcript", "complete=false", "NOT saving to disk"} {
		if !strings.Contains(body, want) {
			t.Errorf("guide does not mention %q", want)
		}
	}

	on, _ := newTestServer(t, func(s *Server) {
		rec, err := NewRecorder(t.TempDir(), "kept", log.New(io.Discard, "", 0))
		if err != nil {
			t.Fatal(err)
		}
		s.recorder, s.conversation = rec, "kept"
	})
	if _, body = get(t, on, "/api", "*/*"); !strings.Contains(body, `saving the conversation as "kept"`) {
		t.Error("guide does not say the conversation is being saved")
	}
}

func readTranscript(t *testing.T, path string) Transcript {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var tr Transcript
	if err := json.Unmarshal(raw, &tr); err != nil {
		t.Fatalf("%s is not valid JSON: %v", path, err)
	}
	return tr
}
