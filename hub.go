package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// Role distinguishes flesh-and-blood participants from LLM agents. It is
// self-declared and purely informational — the server does not police it.
type Role string

const (
	RoleHuman Role = "human"
	RoleLLM   Role = "llm"
)

// ParseRole maps the role field of a join request onto a Role, defaulting to
// human when omitted.
func ParseRole(raw string) (Role, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", string(RoleHuman), "person":
		return RoleHuman, nil
	case string(RoleLLM), "agent", "bot", "ai":
		return RoleLLM, nil
	}
	return "", fmt.Errorf("role must be %q or %q", RoleHuman, RoleLLM)
}

// User is the public identity of a participant, safe to hand to every client.
type User struct {
	Handle   string    `json:"handle"`
	Color    string    `json:"color"`
	Role     Role      `json:"role"`
	JoinedAt time.Time `json:"joined_at"`
}

// Event kinds. The first four are appended to the shared log and carry a Seq;
// the rest are per-connection and unnumbered.
const (
	EventMessage = "message" // someone said something
	EventJoin    = "join"    // someone claimed a handle
	EventLeave   = "leave"   // someone released a handle
	EventSystem  = "system"  // server announcement
	EventWelcome = "welcome" // sent once, to the connection that just joined
	EventUsers   = "users"   // roster snapshot
	EventError   = "error"   // something went wrong on this connection
)

// Event is the single wire type: everything the server sends, over WebSocket
// or REST, is an Event.
type Event struct {
	Type     string    `json:"type"`
	Seq      int64     `json:"seq,omitempty"`
	TS       time.Time `json:"ts"`
	From     *User     `json:"from,omitempty"`
	Text     string    `json:"text,omitempty"`
	Mentions []string  `json:"mentions,omitempty"` // lowercase handles tagged with @
	Users    []User    `json:"users,omitempty"`
	Self     *User     `json:"self,omitempty"`
	Token    string    `json:"token,omitempty"`
	Cursor   int64     `json:"cursor,omitempty"`
	Error    string    `json:"error,omitempty"`
	// RetryAfter is set on rate-limit errors, in seconds.
	RetryAfter float64 `json:"retry_after,omitempty"`
}

// subscriber is one live event stream (in practice, one WebSocket connection).
type subscriber struct {
	ch   chan *Event
	once sync.Once
}

func (s *subscriber) close() { s.once.Do(func() { close(s.ch) }) }

// Session is a joined participant: public identity plus the private bits used
// to authenticate REST calls and fan out events.
type Session struct {
	User
	token    string
	lastSeen time.Time
	subs     map[*subscriber]struct{}

	// Server-side cursors, so an agent that keeps no state of its own can still
	// ask "what happened since I last spoke / last read?".
	lastPost int64
	lastRead int64

	// Token bucket for posting.
	tokens   float64
	tokensAt time.Time
}

// Cursors are a session's two server-tracked positions in the transcript.
type Cursors struct {
	LastPost int64 `json:"last_post_seq"` // seq of this session's last message, 0 if silent
	LastRead int64 `json:"last_read_seq"` // highest seq already handed to this session
}

// ErrNotJoined is returned when a token does not match a live session.
var ErrNotJoined = errors.New("not joined: unknown or expired token")

// RateLimitError reports that a participant is posting too fast. The API turns
// it into a 429 with a Retry-After header.
type RateLimitError struct {
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("posting too fast: wait %s before the next message",
		e.RetryAfter.Round(100*time.Millisecond))
}

// TakenError reports a handle or color collision. The API turns it into a 409.
type TakenError struct {
	Field   string // "handle" or "color"
	Value   string
	By      string // handle of the participant holding it
	TooNear bool   // color was not identical, just indistinguishable
}

func (e *TakenError) Error() string {
	if e.TooNear {
		return fmt.Sprintf("color %s is too close to the color already used by %s", e.Value, e.By)
	}
	if e.By == e.Value {
		return fmt.Sprintf("%s %s is already taken", e.Field, e.Value)
	}
	return fmt.Sprintf("%s %s is already taken by %s", e.Field, e.Value, e.By)
}

// Config holds the tunables the Hub cares about.
type Config struct {
	History          int           // messages kept in memory
	IdleTimeout      time.Duration // REST sessions with no activity are dropped
	MaxMessageLen    int
	MinColorDistance float64 // 0 disables the "too similar" check
	SendBuffer       int     // per-subscriber queue depth
	Rate             float64 // messages per minute per participant, 0 disables
	Burst            float64 // how many may be sent back to back
}

// Hub is the whole chat: the roster, the message log and the fan-out. All
// state is in memory and guarded by a single mutex; every operation is short.
type Hub struct {
	cfg Config

	mu       sync.Mutex
	sessions map[string]*Session // by token
	byHandle map[string]*Session // by HandleKey(handle)
	byColor  map[string]*Session // by normalized color
	log      []*Event            // last cfg.History numbered events
	seq      int64
	evicted  int64 // events pushed out of log, so exports can admit the gap

	// recorder, when set, keeps every event for the JSON transcript. Set once
	// before serving starts and never changed afterwards.
	recorder *Recorder
}

// SetRecorder attaches a transcript recorder. Call it before serving.
func (h *Hub) SetRecorder(r *Recorder) {
	h.mu.Lock()
	h.recorder = r
	h.mu.Unlock()
}

// Evicted is how many events have been dropped from the in-memory history. A
// non-zero value means an export from memory alone cannot claim to be complete.
func (h *Hub) Evicted() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.evicted
}

func NewHub(cfg Config) *Hub {
	return &Hub{
		cfg:      cfg,
		sessions: map[string]*Session{},
		byHandle: map[string]*Session{},
		byColor:  map[string]*Session{},
	}
}

func newToken() string {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// Join claims a handle and a color. Both must be free; the color must also be
// far enough from every color in use when MinColorDistance is set.
func (h *Hub) Join(rawHandle, rawColor, rawRole string) (*Session, error) {
	handle, err := NormalizeHandle(rawHandle)
	if err != nil {
		return nil, err
	}
	color, err := NormalizeColor(rawColor)
	if err != nil {
		return nil, err
	}
	role, err := ParseRole(rawRole)
	if err != nil {
		return nil, err
	}

	h.mu.Lock()
	if other, ok := h.byHandle[HandleKey(handle)]; ok {
		h.mu.Unlock()
		return nil, &TakenError{Field: "handle", Value: handle, By: other.Handle}
	}
	if other, ok := h.byColor[color]; ok {
		h.mu.Unlock()
		return nil, &TakenError{Field: "color", Value: color, By: other.Handle}
	}
	if h.cfg.MinColorDistance > 0 {
		for used, other := range h.byColor {
			if ColorDistance(color, used) < h.cfg.MinColorDistance {
				h.mu.Unlock()
				return nil, &TakenError{Field: "color", Value: color, By: other.Handle, TooNear: true}
			}
		}
	}

	now := time.Now().UTC()
	s := &Session{
		User:     User{Handle: handle, Color: color, Role: role, JoinedAt: now},
		token:    newToken(),
		lastSeen: now,
		subs:     map[*subscriber]struct{}{},
		tokens:   h.cfg.Burst,
		tokensAt: now,
	}
	h.sessions[s.token] = s
	h.byHandle[HandleKey(handle)] = s
	h.byColor[color] = s

	user := s.User
	h.publishLocked(&Event{Type: EventJoin, From: &user, Text: fmt.Sprintf("%s joined as %s", handle, role)})
	h.mu.Unlock()
	return s, nil
}

// Leave releases the handle and color and disconnects any live subscribers.
// It is safe to call twice; the second call is a no-op.
func (h *Hub) Leave(token, reason string) {
	h.mu.Lock()
	s, ok := h.sessions[token]
	if !ok {
		h.mu.Unlock()
		return
	}
	delete(h.sessions, token)
	if cur, ok := h.byHandle[HandleKey(s.Handle)]; ok && cur == s {
		delete(h.byHandle, HandleKey(s.Handle))
	}
	if cur, ok := h.byColor[s.Color]; ok && cur == s {
		delete(h.byColor, s.Color)
	}
	subs := make([]*subscriber, 0, len(s.subs))
	for sub := range s.subs {
		subs = append(subs, sub)
	}
	s.subs = map[*subscriber]struct{}{}

	user := s.User
	if reason == "" {
		reason = "left"
	}
	h.publishLocked(&Event{Type: EventLeave, From: &user, Text: fmt.Sprintf("%s %s", s.Handle, reason)})
	h.mu.Unlock()

	// Closing outside the lock: readers drain their queue before noticing.
	for _, sub := range subs {
		sub.close()
	}
}

// Post appends a message from the given session.
func (h *Hub) Post(token, text string) (*Event, error) {
	text = strings.TrimRight(text, " \t\r\n")
	if strings.TrimSpace(text) == "" {
		return nil, errors.New("message text is empty")
	}
	if len([]rune(text)) > h.cfg.MaxMessageLen {
		return nil, fmt.Errorf("message is longer than %d characters", h.cfg.MaxMessageLen)
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.sessions[token]
	if !ok {
		return nil, ErrNotJoined
	}
	now := time.Now().UTC()
	// The limit is checked before anything is published, so a throttled message
	// costs the room nothing: no sequence number, no fan-out, no history slot.
	if err := h.allowPostLocked(s, now); err != nil {
		return nil, err
	}
	s.lastSeen = now
	user := s.User
	ev := &Event{Type: EventMessage, From: &user, Text: text, Mentions: ParseMentions(text)}
	h.publishLocked(ev)
	s.lastPost = ev.Seq
	if ev.Seq > s.lastRead {
		s.lastRead = ev.Seq // you have obviously seen your own message
	}
	return ev, nil
}

// allowPostLocked implements the per-participant token bucket. Without it one
// agent stuck in a loop would evict the whole in-memory history — the room has
// no disk, so those messages would be gone for everyone.
func (h *Hub) allowPostLocked(s *Session, now time.Time) error {
	if h.cfg.Rate <= 0 {
		return nil
	}
	perSecond := h.cfg.Rate / 60

	if s.tokensAt.IsZero() {
		s.tokens, s.tokensAt = h.cfg.Burst, now
	}
	if elapsed := now.Sub(s.tokensAt).Seconds(); elapsed > 0 {
		s.tokens = math.Min(h.cfg.Burst, s.tokens+elapsed*perSecond)
		s.tokensAt = now
	}

	if s.tokens < 1 {
		wait := time.Duration((1 - s.tokens) / perSecond * float64(time.Second))
		return &RateLimitError{RetryAfter: wait}
	}
	s.tokens--
	return nil
}

// Announce posts a server-authored system message.
func (h *Hub) Announce(text string) *Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	ev := &Event{Type: EventSystem, Text: text}
	h.publishLocked(ev)
	return ev
}

// publishLocked stamps an event, appends it to the log and fans it out.
// Caller must hold h.mu.
func (h *Hub) publishLocked(ev *Event) {
	h.seq++
	ev.Seq = h.seq
	if ev.TS.IsZero() {
		ev.TS = time.Now().UTC()
	}

	// The recorder keeps everything; the log keeps a window. Recording only
	// appends to a slice, so the fan-out below is not waiting on I/O.
	if h.recorder != nil {
		h.recorder.Record(ev)
	}

	h.log = append(h.log, ev)
	if over := len(h.log) - h.cfg.History; over > 0 {
		h.log = h.log[over:]
		h.evicted += int64(over)
	}

	for _, s := range h.sessions {
		for sub := range s.subs {
			select {
			case sub.ch <- ev:
			default:
				// Subscriber is not keeping up. Drop it rather than stall the
				// whole hub; the client can reconnect and replay from the log.
				delete(s.subs, sub)
				sub.close()
			}
		}
	}
}

// Touch marks a session as alive and returns its public identity.
func (h *Hub) Touch(token string) (User, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.sessions[token]
	if !ok {
		return User{}, ErrNotJoined
	}
	s.lastSeen = time.Now().UTC()
	return s.User, nil
}

// Cursors returns the session's server-tracked positions and marks it alive.
func (h *Hub) Cursors(token string) (Cursors, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.sessions[token]
	if !ok {
		return Cursors{}, ErrNotJoined
	}
	s.lastSeen = time.Now().UTC()
	return Cursors{LastPost: s.lastPost, LastRead: s.lastRead}, nil
}

// MarkRead advances the session's server-side read cursor. It never moves
// backwards, so concurrent reads cannot lose ground.
func (h *Hub) MarkRead(token string, seq int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s, ok := h.sessions[token]; ok && seq > s.lastRead {
		s.lastRead = seq
	}
}

// MentionQuery selects which mentions of a handle to return.
type MentionQuery struct {
	Handle           string
	Since            int64
	From             time.Time // zero means no time filter
	IncludeBroadcast bool      // count @everyone and friends
	IncludeSelf      bool      // count the handle's own messages
}

// matches reports whether an event satisfies the query.
func (q MentionQuery) matches(ev *Event) bool {
	if ev.Type != EventMessage || ev.Seq <= q.Since {
		return false
	}
	if !q.From.IsZero() && ev.TS.Before(q.From) {
		return false
	}
	// Your own @everyone is not somebody calling your name; without this an
	// agent polling its mentions would wake itself up.
	if !q.IncludeSelf && ev.From != nil && HandleKey(ev.From.Handle) == HandleKey(q.Handle) {
		return false
	}
	return MentionsHandle(ev.Mentions, q.Handle, q.IncludeBroadcast)
}

// Mentions returns the messages matching the query, oldest first.
func (h *Hub) Mentions(q MentionQuery) (events []*Event, cursor int64, truncated bool) {
	since := q.Since
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ev := range h.log {
		if q.matches(ev) {
			events = append(events, ev)
		}
	}
	if len(h.log) > 0 && since > 0 && h.log[0].Seq > since+1 {
		truncated = true
	}
	return events, h.seq, truncated
}

// Subscribe attaches an event stream to a session. The returned subscriber is
// closed by Unsubscribe, by Leave, or when it falls too far behind.
func (h *Hub) Subscribe(token string) (*subscriber, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.sessions[token]
	if !ok {
		return nil, ErrNotJoined
	}
	sub := &subscriber{ch: make(chan *Event, h.cfg.SendBuffer)}
	s.subs[sub] = struct{}{}
	s.lastSeen = time.Now().UTC()
	return sub, nil
}

func (h *Hub) Unsubscribe(token string, sub *subscriber) {
	h.mu.Lock()
	if s, ok := h.sessions[token]; ok {
		delete(s.subs, sub)
		s.lastSeen = time.Now().UTC()
	}
	h.mu.Unlock()
	sub.close()
}

// Users returns the roster, oldest join first.
func (h *Hub) Users() []User {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.usersLocked()
}

func (h *Hub) usersLocked() []User {
	out := make([]User, 0, len(h.sessions))
	for _, s := range h.sessions {
		out = append(out, s.User)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].JoinedAt.Equal(out[j].JoinedAt) {
			return out[i].Handle < out[j].Handle
		}
		return out[i].JoinedAt.Before(out[j].JoinedAt)
	})
	return out
}

// History returns the events numbered above since, the current cursor, and
// whether anything between since and the returned events was already evicted.
func (h *Hub) History(since int64) (events []*Event, cursor int64, truncated bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ev := range h.log {
		if ev.Seq > since {
			events = append(events, ev)
		}
	}
	if len(h.log) > 0 && since > 0 && h.log[0].Seq > since+1 {
		truncated = true
	}
	return events, h.seq, truncated
}

// AvailableColors splits the suggested palette into free and taken, applying
// the same distance rule Join enforces.
func (h *Hub) AvailableColors() (free, taken []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range Palette {
		if _, used := h.byColor[c]; used {
			taken = append(taken, c)
			continue
		}
		tooNear := false
		if h.cfg.MinColorDistance > 0 {
			for used := range h.byColor {
				if ColorDistance(c, used) < h.cfg.MinColorDistance {
					tooNear = true
					break
				}
			}
		}
		if tooNear {
			taken = append(taken, c)
		} else {
			free = append(free, c)
		}
	}
	return free, taken
}

// ReapIdle drops sessions that have no live subscriber and have not called the
// REST API within IdleTimeout, so their handle and color come back into play.
func (h *Hub) ReapIdle(now time.Time) int {
	if h.cfg.IdleTimeout <= 0 {
		return 0
	}
	h.mu.Lock()
	var stale []string
	for token, s := range h.sessions {
		if len(s.subs) == 0 && now.Sub(s.lastSeen) > h.cfg.IdleTimeout {
			stale = append(stale, token)
		}
	}
	h.mu.Unlock()

	for _, token := range stale {
		h.Leave(token, "timed out")
	}
	return len(stale)
}
