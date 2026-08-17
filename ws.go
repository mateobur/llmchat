package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const (
	wsWriteWait   = 10 * time.Second
	wsPongWait    = 60 * time.Second
	wsPingPeriod  = 25 * time.Second
	wsMaxFrame    = 1 << 20
	wsJoinTimeout = 2 * time.Minute
)

// clientFrame is what a WebSocket client sends us.
type clientFrame struct {
	Type   string `json:"type"`
	Handle string `json:"handle,omitempty"`
	Color  string `json:"color,omitempty"`
	Role   string `json:"role,omitempty"`
	Text   string `json:"text,omitempty"`
}

func (s *Server) upgrader() *websocket.Upgrader {
	return &websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
		CheckOrigin:     s.checkOrigin,
	}
}

// checkOrigin accepts non-browser clients (no Origin header) and browsers whose
// Origin matches the host being served, unless the operator opted out.
func (s *Server) checkOrigin(r *http.Request) bool {
	if s.allowAnyOrigin {
		return true
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // curl, Go clients, LLM agents
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	token := sessionToken(r)
	// A caller with no session must pass the access gate before it can join.
	if token == "" && !s.checkAccess(w, r) {
		return
	}

	conn, err := s.upgrader().Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade already wrote the error
	}
	defer conn.Close()

	conn.SetReadLimit(wsMaxFrame)
	c := &wsConn{srv: s, conn: conn, local: make(chan *Event, 8)}
	c.serve(token)
}

type wsConn struct {
	srv   *Server
	conn  *websocket.Conn
	local chan *Event // per-connection replies (welcome, errors, roster)

	token string
	self  User
	owns  bool // this socket created the session, so it also ends it
}

func (c *wsConn) serve(token string) {
	if token != "" {
		user, err := c.srv.hub.Touch(token)
		if err != nil {
			c.writeDirect(&Event{Type: EventError, TS: time.Now().UTC(), Error: err.Error()})
			return
		}
		c.token, c.self = token, user
	} else if !c.joinPhase() {
		return
	}

	sub, err := c.srv.hub.Subscribe(c.token)
	if err != nil {
		c.writeDirect(&Event{Type: EventError, TS: time.Now().UTC(), Error: err.Error()})
		return
	}

	// Backlog goes out before the write pump starts, so a long history cannot
	// overflow the per-connection queue. Events published between Subscribe and
	// here arrive twice; clients dedupe on seq.
	history, cursor, _ := c.srv.hub.History(0)
	self := c.self
	if err := c.writeDirect(&Event{
		Type:   EventWelcome,
		TS:     time.Now().UTC(),
		Self:   &self,
		Users:  c.srv.hub.Users(),
		Cursor: cursor,
	}); err != nil {
		c.srv.hub.Unsubscribe(c.token, sub)
		return
	}
	for _, ev := range history {
		if err := c.writeDirect(ev); err != nil {
			c.srv.hub.Unsubscribe(c.token, sub)
			return
		}
	}

	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		c.writePump(sub)
	}()

	c.readPump()

	c.srv.hub.Unsubscribe(c.token, sub) // closes sub.ch, unblocking writePump
	if c.owns {
		c.srv.hub.Leave(c.token, "disconnected")
	}
	<-writerDone
}

// joinPhase reads frames until the client successfully claims a handle and a
// color. Rejections are reported on the same socket so the web client can let
// the user pick again without reconnecting.
func (c *wsConn) joinPhase() bool {
	deadline := time.Now().Add(wsJoinTimeout)
	c.conn.SetReadDeadline(deadline)
	for {
		var f clientFrame
		if err := c.conn.ReadJSON(&f); err != nil {
			return false
		}
		if f.Type != "" && f.Type != EventJoin {
			c.writeDirect(&Event{Type: EventError, TS: time.Now().UTC(),
				Error: "declare your identity first: {\"type\":\"join\",\"handle\":\"...\",\"color\":\"#rrggbb\",\"role\":\"human|llm\"}"})
			continue
		}
		sess, err := c.srv.hub.Join(f.Handle, f.Color, f.Role)
		if err != nil {
			c.writeDirect(&Event{Type: EventError, TS: time.Now().UTC(), Error: err.Error()})
			continue
		}
		c.token, c.self, c.owns = sess.token, sess.User, true
		c.srv.logger.Printf("join(ws): %s (%s) as %s", sess.Handle, sess.Color, sess.Role)
		// Hand the token back so the same identity can also be driven over REST.
		c.writeDirect(&Event{Type: EventJoin, TS: time.Now().UTC(), Self: &sess.User, Token: sess.token})
		return true
	}
}

func (c *wsConn) readPump() {
	c.conn.SetReadDeadline(time.Now().Add(wsPongWait))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(wsPongWait))
	})
	for {
		var f clientFrame
		if err := c.conn.ReadJSON(&f); err != nil {
			// A malformed frame is recoverable: the message was fully consumed,
			// so tell the client and keep the socket open. Anything else means
			// the connection is gone.
			var syntax *json.SyntaxError
			var wrongType *json.UnmarshalTypeError
			if errors.As(err, &syntax) || errors.As(err, &wrongType) {
				c.queue(&Event{Type: EventError, TS: time.Now().UTC(), Error: "invalid JSON frame"})
				continue
			}
			return
		}
		switch f.Type {
		case EventMessage, "":
			if _, err := c.srv.hub.Post(c.token, f.Text); err != nil {
				ev := &Event{Type: EventError, TS: time.Now().UTC(), Error: err.Error()}
				var limited *RateLimitError
				if errors.As(err, &limited) {
					ev.RetryAfter = limited.RetryAfter.Seconds()
				}
				c.queue(ev)
				if errors.Is(err, ErrNotJoined) {
					return
				}
			}
		case EventUsers:
			c.queue(&Event{Type: EventUsers, TS: time.Now().UTC(), Users: c.srv.hub.Users()})
		case "ping":
			c.queue(&Event{Type: "pong", TS: time.Now().UTC()})
		case EventLeave:
			c.srv.hub.Leave(c.token, "left")
			return
		case EventJoin:
			c.queue(&Event{Type: EventError, TS: time.Now().UTC(),
				Error: "already joined as " + c.self.Handle})
		default:
			c.queue(&Event{Type: EventError, TS: time.Now().UTC(), Error: "unknown frame type " + f.Type})
		}
	}
}

// writePump owns the write side of the socket once the connection is joined.
func (c *wsConn) writePump(sub *subscriber) {
	ticker := time.NewTicker(wsPingPeriod)
	defer ticker.Stop()
	for {
		select {
		case ev, ok := <-sub.ch:
			if !ok {
				c.sendClose("session ended")
				return
			}
			if err := c.writeDirect(ev); err != nil {
				return
			}
		case ev := <-c.local:
			if err := c.writeDirect(ev); err != nil {
				return
			}
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// queue sends a per-connection event through the write pump. Before the pump
// starts, the buffered channel simply holds the events until it does.
func (c *wsConn) queue(ev *Event) {
	select {
	case c.local <- ev:
	default: // connection is wedged; the read side will notice and tear down
	}
}

func (c *wsConn) writeDirect(ev *Event) error {
	c.conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
	return c.conn.WriteJSON(ev)
}

func (c *wsConn) sendClose(reason string) {
	msg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, reason)
	c.conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
	c.conn.WriteMessage(websocket.CloseMessage, msg)
}
