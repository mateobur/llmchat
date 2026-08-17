package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Server-Sent Events: one HTTP request that stays open while the server pushes
// events down it. This is the answer to "how do I follow the room without a
// polling loop" for any agent that runs as a process — one curl, no library, no
// reconnect logic, and no delay between something being said and being seen.

const (
	// Comment lines keep the connection warm and let both ends notice a dead
	// peer. Proxies commonly close idle connections after a minute.
	ssePingEvery = 25 * time.Second
)

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	token := sessionToken(r)
	cursors, err := s.hub.Cursors(token)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	self, err := s.hub.Touch(token)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, errors.New("this server cannot stream"))
		return
	}

	// Last-Event-ID wins over ?since=: that is how a browser EventSource resumes
	// by itself after a dropped connection.
	rawSince := r.URL.Query().Get("since")
	if resume := r.Header.Get("Last-Event-ID"); resume != "" {
		rawSince = resume
	}
	since, err := resolveSince(rawSince, cursors)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	// Subscribe before reading the backlog so nothing can slip through the gap.
	// Events published in between arrive twice; clients dedupe on seq, exactly
	// as they do on the WebSocket.
	sub, err := s.hub.Subscribe(token)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	defer s.hub.Unsubscribe(token, sub)

	head := w.Header()
	head.Set("Content-Type", "text/event-stream")
	head.Set("Cache-Control", "no-store")
	head.Set("Connection", "keep-alive")
	// Nginx buffers proxied responses by default, which would hold events back
	// until the buffer filled — the one thing a stream must not do.
	head.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	backlog, cursor, truncated := s.hub.History(since)
	welcome := &Event{
		Type:   EventWelcome,
		TS:     time.Now().UTC(),
		Self:   &self,
		Users:  s.hub.Users(),
		Cursor: cursor,
	}
	if truncated {
		welcome.Text = "some events before this point are no longer in the history"
	}
	if err := writeSSE(w, welcome); err != nil {
		return
	}
	for _, ev := range backlog {
		if err := writeSSE(w, ev); err != nil {
			return
		}
		s.hub.MarkRead(token, ev.Seq)
	}
	flusher.Flush()

	ping := time.NewTicker(ssePingEvery)
	defer ping.Stop()

	for {
		select {
		case ev, ok := <-sub.ch:
			if !ok {
				// The session ended underneath us: say so and close, rather
				// than leaving the client waiting on a stream that is finished.
				writeSSE(w, &Event{Type: EventError, TS: time.Now().UTC(), Error: ErrNotJoined.Error()})
				flusher.Flush()
				return
			}
			if err := writeSSE(w, ev); err != nil {
				return
			}
			s.hub.MarkRead(token, ev.Seq)
			flusher.Flush()

		case <-ping.C:
			// A comment: valid SSE, ignored by every client, and enough to
			// notice a connection that has gone away.
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()

		case <-r.Context().Done():
			return
		}
	}
}

// writeSSE frames one event. Numbered events carry an id, which is what a
// client sends back as Last-Event-ID to resume where it left off.
func writeSSE(w io.Writer, ev *Event) error {
	payload, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	if ev.Seq > 0 {
		if _, err := fmt.Fprintf(w, "id: %d\n", ev.Seq); err != nil {
			return err
		}
	}
	// json.Marshal escapes newlines, so data is always a single line.
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, payload)
	return err
}
