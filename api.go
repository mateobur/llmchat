package main

import (
	"encoding/json"
	"errors"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Server wires the Hub to HTTP: a REST API usable with plain curl (for LLM
// agents) and a WebSocket endpoint (for the web client).
type Server struct {
	hub            *Hub
	cfg            Config
	accessToken    string        // when set, required to join
	maxWait        time.Duration // cap for long-poll ?wait=
	allowAnyOrigin bool          // relax the WebSocket Origin check
	conversation   string        // name used for the transcript and its filename
	recorder       *Recorder     // nil when persistence is off
	logger         *log.Logger
}

func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/join", s.handleJoin)
	mux.HandleFunc("POST /api/leave", s.handleLeave)
	mux.HandleFunc("POST /api/messages", s.handlePost)
	mux.HandleFunc("GET /api/messages", s.handleRead)
	mux.HandleFunc("GET /api/mentions", s.handleMentions)
	mux.HandleFunc("GET /api/users", s.handleUsers)
	mux.HandleFunc("GET /api/palette", s.handlePalette)
	mux.HandleFunc("GET /api/whoami", s.handleWhoami)
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/transcript", s.handleTranscript)
	mux.HandleFunc("GET /api", s.handleDocs)
	mux.HandleFunc("GET /ws", s.handleWS)
	mux.Handle("GET /", s.fallback())
	return mux
}

// postOnly are the endpoints that exist only for POST. Without this, a GET to
// one of them would fall through to the web client and look like a typo'd URL.
var postOnly = map[string]string{
	"/api/join":     http.MethodPost,
	"/api/leave":    http.MethodPost,
	"/api/messages": http.MethodPost, // GET is registered separately
}

// fallback serves the embedded web client, minus the API namespace.
func (s *Server) fallback() http.Handler {
	files := webClientHandler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if method, ok := postOnly[r.URL.Path]; ok {
			w.Header().Set("Allow", method)
			writeErr(w, http.StatusMethodNotAllowed, errors.New(r.URL.Path+" requires "+method))
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeErr(w, http.StatusNotFound,
				errors.New("no such endpoint: "+r.URL.Path+" — see GET /api"))
			return
		}
		// A client that did not ask for HTML gets the API guide instead of the
		// web client, so an agent hitting the bare URL is self-service.
		if (r.URL.Path == "/" || r.URL.Path == "/index.html") && !wantsHTML(r) {
			s.handleDocs(w, r)
			return
		}
		files.ServeHTTP(w, r)
	})
}

// ---------- helpers ----------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		return // client hung up
	}
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// statusFor maps hub errors onto HTTP status codes.
func statusFor(err error) int {
	var taken *TakenError
	var limited *RateLimitError
	switch {
	case errors.As(err, &taken):
		return http.StatusConflict
	case errors.As(err, &limited):
		return http.StatusTooManyRequests
	case errors.Is(err, ErrNotJoined):
		return http.StatusUnauthorized
	default:
		return http.StatusBadRequest
	}
}

// sessionToken pulls the session token from the Authorization header, the
// X-Chat-Token header, or a token query parameter, in that order.
func sessionToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if after, ok := cutPrefixFold(h, "bearer "); ok {
			return strings.TrimSpace(after)
		}
	}
	if h := strings.TrimSpace(r.Header.Get("X-Chat-Token")); h != "" {
		return h
	}
	return strings.TrimSpace(r.URL.Query().Get("token"))
}

func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
		return s[len(prefix):], true
	}
	return "", false
}

// checkAccess enforces the optional shared access token that gates joining.
func (s *Server) checkAccess(w http.ResponseWriter, r *http.Request) bool {
	if s.accessToken == "" {
		return true
	}
	got := strings.TrimSpace(r.Header.Get("X-Access-Token"))
	if got == "" {
		got = strings.TrimSpace(r.URL.Query().Get("access_token"))
	}
	if got != s.accessToken {
		writeErr(w, http.StatusForbidden, errors.New("missing or invalid access token"))
		return false
	}
	return true
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("invalid JSON body: "+err.Error()))
		return false
	}
	return true
}

// ---------- handlers ----------

type joinRequest struct {
	Handle string `json:"handle"`
	Color  string `json:"color"`
	Role   string `json:"role"`
}

type joinResponse struct {
	Token   string   `json:"token"`
	Self    User     `json:"self"`
	Users   []User   `json:"users"`
	Cursor  int64    `json:"cursor"`
	History []*Event `json:"history"`
}

func (s *Server) handleJoin(w http.ResponseWriter, r *http.Request) {
	if !s.checkAccess(w, r) {
		return
	}
	var req joinRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	sess, err := s.hub.Join(req.Handle, req.Color, req.Role)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	history, cursor, _ := s.hub.History(0)
	s.logger.Printf("join: %s (%s) as %s", sess.Handle, sess.Color, sess.Role)
	writeJSON(w, http.StatusCreated, joinResponse{
		Token:   sess.token,
		Self:    sess.User,
		Users:   s.hub.Users(),
		Cursor:  cursor,
		History: history,
	})
}

func (s *Server) handleLeave(w http.ResponseWriter, r *http.Request) {
	token := sessionToken(r)
	if _, err := s.hub.Touch(token); err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	s.hub.Leave(token, "left")
	writeJSON(w, http.StatusOK, map[string]bool{"left": true})
}

type postRequest struct {
	Text string `json:"text"`
}

func (s *Server) handlePost(w http.ResponseWriter, r *http.Request) {
	var req postRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	ev, err := s.hub.Post(sessionToken(r), req.Text)
	if err != nil {
		var limited *RateLimitError
		if errors.As(err, &limited) {
			// Round up: telling a client to wait 0 seconds invites a hot loop.
			seconds := int(math.Ceil(limited.RetryAfter.Seconds()))
			w.Header().Set("Retry-After", strconv.Itoa(max(seconds, 1)))
		}
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"seq": ev.Seq, "ts": ev.TS})
}

type readResponse struct {
	Events    []*Event `json:"events"`
	Cursor    int64    `json:"cursor"`
	Truncated bool     `json:"truncated"`
	Users     []User   `json:"users,omitempty"`
}

// handleRead serves GET /api/messages?since=N&wait=30s. Without wait it
// returns immediately; with wait it blocks until something new shows up, which
// is what lets an LLM agent follow the conversation using only curl.
func (s *Server) handleRead(w http.ResponseWriter, r *http.Request) {
	token := sessionToken(r)
	cursors, err := s.hub.Cursors(token)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}

	q := r.URL.Query()
	since, err := resolveSince(q.Get("since"), cursors)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	wait, err := parseWait(q.Get("wait"), s.maxWait)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	withUsers := q.Get("users") == "true"

	respond := func(events []*Event, cursor int64, truncated bool) {
		resp := readResponse{Events: events, Cursor: cursor, Truncated: truncated}
		if resp.Events == nil {
			resp.Events = []*Event{}
		}
		if withUsers {
			resp.Users = s.hub.Users()
		}
		// Remember how far this session has been served, so a stateless agent
		// can come back with since=last-read.
		s.hub.MarkRead(token, cursor)
		writeJSON(w, http.StatusOK, resp)
	}

	if wait == 0 {
		events, cursor, truncated := s.hub.History(since)
		respond(events, cursor, truncated)
		return
	}

	// Subscribe before reading history so nothing can slip through the gap.
	sub, err := s.hub.Subscribe(token)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	defer s.hub.Unsubscribe(token, sub)

	if events, cursor, truncated := s.hub.History(since); len(events) > 0 {
		respond(events, cursor, truncated)
		return
	}

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case ev, ok := <-sub.ch:
		if !ok { // session ended underneath us
			writeErr(w, http.StatusUnauthorized, ErrNotJoined)
			return
		}
		events := []*Event{ev}
		// Drain whatever else has already queued up, so a burst of replies
		// comes back in one response.
		for drained := true; drained; {
			select {
			case more, ok := <-sub.ch:
				if !ok {
					drained = false
					break
				}
				events = append(events, more)
			default:
				drained = false
			}
		}
		respond(events, events[len(events)-1].Seq, false)
	case <-timer.C:
		respond(nil, since, false)
	case <-r.Context().Done():
	}
}

// resolveSince accepts a raw sequence number or one of two keywords that let a
// client with no memory of its own pick up where it left off.
func resolveSince(raw string, cursors Cursors) (int64, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return 0, nil
	case "last-post", "last_post", "mine":
		return cursors.LastPost, nil
	case "last-read", "last_read":
		return cursors.LastRead, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0, errors.New(`since must be a non-negative integer, "last-post" or "last-read"`)
	}
	return n, nil
}

// parseFrom accepts an RFC3339 timestamp ("2026-08-17T10:00:00Z") or a duration
// meaning "that long ago" ("15m"), and returns the zero time when empty.
func parseFrom(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, nil
	}
	if ts, err := time.Parse(time.RFC3339, raw); err == nil {
		return ts.UTC(), nil
	}
	if d, err := time.ParseDuration(raw); err == nil {
		if d < 0 {
			d = -d
		}
		return time.Now().UTC().Add(-d), nil
	}
	return time.Time{}, errors.New(`from must be an RFC3339 timestamp ("2026-08-17T10:00:00Z") or a duration ago ("15m")`)
}

// parseWait accepts a bare number of seconds ("30") or a Go duration ("30s").
func parseWait(raw string, max time.Duration) (time.Duration, error) {
	if raw == "" {
		return 0, nil
	}
	var d time.Duration
	if n, err := strconv.Atoi(raw); err == nil {
		d = time.Duration(n) * time.Second
	} else {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return 0, errors.New("wait must be seconds (30) or a duration (30s)")
		}
		d = parsed
	}
	if d < 0 {
		return 0, errors.New("wait must not be negative")
	}
	if d > max {
		d = max
	}
	return d, nil
}

func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"users": s.hub.Users()})
}

func (s *Server) handlePalette(w http.ResponseWriter, r *http.Request) {
	free, taken := s.hub.AvailableColors()
	if free == nil {
		free = []string{}
	}
	if taken == nil {
		taken = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"free":         free,
		"taken":        taken,
		"min_distance": s.cfg.MinColorDistance,
	})
}

func (s *Server) handleWhoami(w http.ResponseWriter, r *http.Request) {
	token := sessionToken(r)
	user, err := s.hub.Touch(token)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	cursors, err := s.hub.Cursors(token)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	_, cursor, _ := s.hub.History(0)
	writeJSON(w, http.StatusOK, map[string]any{
		"self":          user,
		"last_post_seq": cursors.LastPost,
		"last_read_seq": cursors.LastRead,
		"cursor":        cursor,
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	_, cursor, _ := s.hub.History(0)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"users":   len(s.hub.Users()),
		"cursor":  cursor,
		"history": s.cfg.History,
	})
}

type mentionsResponse struct {
	Handle    string   `json:"handle"`
	Events    []*Event `json:"events"`
	Cursor    int64    `json:"cursor"`
	Truncated bool     `json:"truncated"`
}

// handleMentions serves GET /api/mentions: the messages that tagged you with
// @handle. Reading mentions deliberately does not advance the read cursor, so
// checking who called your name never makes you miss the rest of the room.
func (s *Server) handleMentions(w http.ResponseWriter, r *http.Request) {
	token := sessionToken(r)
	self, err := s.hub.Touch(token)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	cursors, err := s.hub.Cursors(token)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}

	q := r.URL.Query()
	handle := strings.TrimSpace(q.Get("handle"))
	if handle == "" {
		handle = self.Handle
	}
	since, err := resolveSince(q.Get("since"), cursors)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	from, err := parseFrom(q.Get("from"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	wait, err := parseWait(q.Get("wait"), s.maxWait)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	query := MentionQuery{
		Handle: handle,
		Since:  since,
		From:   from,
		// @everyone and friends count unless you say otherwise; your own
		// messages do not, unless you ask for them.
		IncludeBroadcast: q.Get("broadcast") != "false",
		IncludeSelf:      q.Get("include_self") == "true",
	}

	respond := func(events []*Event, cursor int64, truncated bool) {
		if events == nil {
			events = []*Event{}
		}
		writeJSON(w, http.StatusOK, mentionsResponse{
			Handle: handle, Events: events, Cursor: cursor, Truncated: truncated,
		})
	}

	if wait == 0 {
		respond(s.hub.Mentions(query))
		return
	}

	sub, err := s.hub.Subscribe(token)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	defer s.hub.Unsubscribe(token, sub)

	if events, cursor, truncated := s.hub.Mentions(query); len(events) > 0 {
		respond(events, cursor, truncated)
		return
	}

	// Nothing yet: wait for a message that tags the handle, ignoring the rest of
	// the traffic on the way past.
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		select {
		case ev, ok := <-sub.ch:
			if !ok {
				writeErr(w, http.StatusUnauthorized, ErrNotJoined)
				return
			}
			if !query.matches(ev) {
				continue
			}
			respond([]*Event{ev}, ev.Seq, false)
			return
		case <-timer.C:
			respond(nil, since, false)
			return
		case <-r.Context().Done():
			return
		}
	}
}

// handleTranscript serves the conversation as the same JSON document that gets
// written to chats/. It works whether or not persistence is enabled: with a
// recorder it is the whole conversation, without one it is whatever is still in
// the in-memory history, and the document says which.
func (s *Server) handleTranscript(w http.ResponseWriter, r *http.Request) {
	if _, err := s.hub.Touch(sessionToken(r)); err != nil {
		writeErr(w, statusFor(err), err)
		return
	}

	var events []*Event
	var complete bool
	if s.recorder != nil {
		events, complete = s.recorder.Snapshot()
	} else {
		events, _, _ = s.hub.History(0)
		complete = s.hub.Evicted() == 0
	}

	transcript := BuildTranscript(s.conversation, events, complete)
	filename := s.conversation + ".json"

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("Cache-Control", "no-store")
	// Indented on purpose: a transcript is something a person opens.
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(transcript); err != nil {
		s.logger.Printf("writing transcript to client: %v", err)
	}
}
