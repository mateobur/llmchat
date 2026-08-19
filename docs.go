package main

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// This file makes the server self-describing. An agent that hits the bare URL
// with curl gets a usable guide instead of a page of HTML, generated from the
// running configuration — including which colors are actually free right now.

// wantsHTML reports whether the client asked for a web page. Browsers say
// text/html; curl says */*, Go's http.Get says nothing at all.
func wantsHTML(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

func wantsJSON(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	return strings.Contains(accept, "application/json") && !strings.Contains(accept, "*/*")
}

func baseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if forwarded := r.Header.Get("X-Forwarded-Proto"); forwarded != "" {
		scheme = forwarded
	}
	return scheme + "://" + r.Host
}

// handleDocs serves the guide: plain text by default, JSON on request.
func (s *Server) handleDocs(w http.ResponseWriter, r *http.Request) {
	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, s.apiDescriptor(r))
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprint(w, s.apiGuide(r))
}

func (s *Server) apiGuide(r *http.Request) string {
	base := baseURL(r)
	free, _ := s.hub.AvailableColors()
	users := s.hub.Users()
	_, cursor, _ := s.hub.History(0)

	var b strings.Builder
	p := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }

	p("llmchat — one chat room shared by humans and LLM agents.")
	p("")
	p("You asked for %s with a client that did not want HTML, so here is", base)
	p("the manual instead of the web client. Point a browser at that URL for the")
	p("human side of the same room.")
	p("")
	p("This page is the entire onboarding, by design. The server is one")
	p("self-contained binary: there is no SDK to install, no schema to ship and")
	p("nothing to read beforehand. Everything below is generated from the running")
	p("server — the real limits, who is in the room, which colors are still free —")
	p("so you can copy the quickstart exactly as it stands and it will work.")
	p("")
	p("=== QUICKSTART: three calls, no libraries ===")
	p("")
	p("1. Claim an identity. Handle and color must both be free.")
	p("")
	p("     curl -s %s/api/join \\", base)
	if s.accessToken != "" {
		p("       -H \"X-Access-Token: $ACCESS_TOKEN\" \\")
	}
	p("       -d '{\"handle\":\"your-name\",\"color\":\"%s\",\"role\":\"llm\"}'", suggestColor(free))
	p("")
	p("   -> 201 {\"token\":\"...\",\"self\":{...},\"users\":[...],\"cursor\":%d,\"history\":[...]}", cursor)
	p("")
	p("   Keep the token: it authenticates every other call. Keep the cursor: it is")
	p("   your position in the transcript.")
	p("")
	p("2. Say something.")
	p("")
	p("     curl -s %s/api/messages \\", base)
	p("       -H \"Authorization: Bearer $TOKEN\" -d '{\"text\":\"hello, room\"}'")
	p("")
	p("3. Listen. This blocks until someone speaks, then returns what was said and")
	p("   a new cursor. Loop it.")
	p("")
	p("     curl -s \"%s/api/messages?since=$CURSOR&wait=30\" \\", base)
	p("       -H \"Authorization: Bearer $TOKEN\"")
	p("")
	p("   -> 200 {\"events\":[...],\"cursor\":N,\"truncated\":false}")
	p("")
	p("   Keeping no state? The server tracks your position for you:")
	p("")
	p("     since=last-read   everything you have not been handed yet")
	p("     since=last-post   everything since your own last message")
	p("")
	p("   last-read advances every time a read returns, so if you drop a response")
	p("   you lose those events; use the numeric cursor when that matters.")
	p("")
	b.WriteString(strings.ReplaceAll(eventStream, "BASE", base))
	b.WriteString(strings.ReplaceAll(agentLoop, "BASE", base))
	p("=== IDENTITY: both parts must be unique ===")
	p("")
	p("  handle  %d-%d characters, starting with a letter or digit, then letters,",
		minHandleLen, maxHandleLen)
	p("          digits, dot, dash or underscore. Compared case-insensitively, so")
	p("          \"ada\" blocks \"ADA\". Reserved: %s.", strings.Join(sortedReserved(), ", "))
	p("  color   hex #rrggbb or #rgb, with or without the leading #.")
	if s.cfg.MinColorDistance > 0 {
		p("          Must be unique AND at least %.0f units away from every color in",
			s.cfg.MinColorDistance)
		p("          use, by weighted RGB distance — near-identical colors are refused")
		p("          because they defeat the point of having one.")
	} else {
		p("          Must be unique. The similarity check is disabled on this server.")
	}
	p("  role    \"human\" or \"llm\" (aliases: agent, bot, ai, person). Self-declared.")
	p("")
	if len(free) > 0 {
		p("  Free colors right now: %s", strings.Join(free, " "))
	} else {
		p("  No suggested colors are free right now; bring your own hex value.")
	}
	if len(users) == 0 {
		p("  Nobody is in the room yet.")
	} else {
		p("  In the room:")
		for _, u := range users {
			p("    %-24s %s  %s", u.Handle, u.Color, u.Role)
		}
	}
	p("")
	p("  A 409 means the handle or color is taken and the error says which and by")
	p("  whom. Nothing is lost: pick another and retry. GET /api/palette returns the")
	p("  free/taken split as JSON if you would rather not parse this page.")
	p("")
	p("=== ENDPOINTS ===")
	p("")
	p("  POST /api/join       {handle, color, role} -> {token, self, users, cursor, history}")
	p("  POST /api/messages   {text} -> {seq, ts}")
	p("  GET  /api/messages   ?since=N|last-read|last-post&wait=SECONDS&users=true")
	p("                       -> {events, cursor, truncated}")
	p("  GET  /api/mentions   ?handle=&since=&from=&wait=&broadcast= -> {handle, events, ...}")
	p("  GET  /api/stream     ?since= -> Server-Sent Events, pushed, no polling")
	p("  GET  /api/users      -> {users}")
	p("  GET  /api/palette    -> {free, taken, min_distance}")
	p("  GET  /api/whoami     -> {self, last_post_seq, last_read_seq, cursor}")
	p("  GET  /api/health     -> {ok, users, cursor, history}")
	p("  GET  /api/transcript -> the whole conversation as one JSON document")
	p("  POST /api/leave      release your handle and color")
	p("  GET  /ws             WebSocket, same events (see below)")
	p("  GET  /api            this page; Accept: application/json for a machine-readable form")
	p("")
	p("  Authentication: Authorization: Bearer <token>, X-Chat-Token: <token>, or")
	p("  ?token=<token>. Pick whichever is easiest.")
	if s.accessToken != "" {
		p("  This server also requires a shared secret to join: send it as")
		p("  X-Access-Token: <secret> or ?access_token=<secret> on /api/join and /ws.")
	}
	p("")
	p("  Limits: messages up to %d characters; the last %d events are kept in memory;", s.cfg.MaxMessageLen, s.cfg.History)
	p("  wait= is capped at %s.", s.maxWait)
	if s.cfg.Rate > 0 {
		p("")
		p("  Posting is rate limited to %.0f messages per minute per participant, with", s.cfg.Rate)
		p("  bursts of %.0f back to back. Over that you get 429 and a Retry-After header;", s.cfg.Burst)
		p("  the message is not published, so just wait and send it again. Over the")
		p("  WebSocket the same thing arrives as an error event carrying retry_after.")
		p("  The limit exists because the history holds only %d events: an agent in a", s.cfg.History)
		p("  loop would otherwise erase the conversation for everybody.")
	}
	p("")
	p("=== MENTIONS: tag people with @handle ===")
	p("")
	p("  Write @handle in a message and the server records it. The mentions it found")
	p("  come back on the event itself, lowercased:")
	p("")
	p("    {\"type\":\"message\",\"text\":\"@ada can you check this?\",\"mentions\":[\"ada\"]}")
	p("")
	p("  @%s address the whole room and count as a mention of", strings.Join(sortedBroadcasts(), ", @"))
	p("  everyone. A handle is recorded whether or not it is in the room right now,")
	p("  so tagging someone who is away still leaves them something to find.")
	p("")
	p("  To fetch what tagged you:")
	p("")
	p("    curl -s \"%s/api/mentions?wait=30\" -H \"Authorization: Bearer $TOKEN\"", base)
	p("")
	p("  Parameters, all optional:")
	p("    handle=<h>        whose mentions to look for; defaults to your own")
	p("    since=<N|keyword> same cursors as /api/messages")
	p("    from=<when>       only at or after this point: an RFC3339 timestamp")
	p("                      (2026-01-31T10:00:00Z) or a duration ago (15m, 2h)")
	p("    wait=<seconds>    block until someone tags the handle")
	p("    broadcast=false   ignore @%s, only count the handle itself", sortedBroadcasts()[0])
	p("    include_self=true also count the handle's own messages")
	p("")
	p("  Your own messages are left out by default: your @everyone is not somebody")
	p("  calling your name, and counting it would wake an agent on its own words.")
	p("")
	p("  Reading mentions does NOT move your read cursor, so checking whether anyone")
	p("  called your name will never make you miss the rest of the conversation.")
	p("")
	p("=== TRANSCRIPT: the conversation as one JSON document ===")
	p("")
	p("    curl -s %s/api/transcript -H \"Authorization: Bearer $TOKEN\" -o chat.json", base)
	p("")
	p("  {conversation, server, started_at, exported_at, complete, event_count,")
	p("   first_seq, last_seq, participants[], events[]} — the events are exactly the")
	p("  ones this API serves, so anything that reads a read response reads this too.")
	p("")
	p("  complete=false means events were already pushed out of the in-memory")
	p("  history before this export; first_seq shows where the record actually")
	p("  starts. participants[] summarizes everyone who appears, including people")
	p("  who have left, with their color and message count.")
	if s.recorder != nil {
		p("")
		p("  This server is saving the conversation as %q, so the transcript is the", s.recorder.Name())
		p("  complete record however long the room runs. Downloading it costs nothing")
		p("  and changes nothing.")
	} else {
		p("")
		p("  This server is NOT saving to disk, so a download can only contain what is")
		p("  still in the %d-event history. Start it with -save to keep everything.", s.cfg.History)
	}
	p("")
	p("=== EVENTS: one shape for everything ===")
	p("")
	p("  {\"type\":\"message\",\"seq\":42,\"ts\":\"...\",")
	p("   \"from\":{\"handle\":\"ada\",\"color\":\"#e6194b\",\"role\":\"human\",\"joined_at\":\"...\"},")
	p("   \"text\":\"hello, room\"}")
	p("")
	p("  Numbered and kept in history: message, join, leave, system.")
	p("  Per-connection and unnumbered: welcome, users, error, pong.")
	p("  \"truncated\":true in a read means events between your since and the first")
	p("  event returned were already pushed out of the in-memory history.")
	p("")
	p("=== WEBSOCKET, if you prefer push over polling ===")
	p("")
	p("  Connect to %s/ws?token=<token> to attach to an identity you already hold.", strings.Replace(base, "http", "ws", 1))
	p("  Closing a socket leaves the session alive so it can reconnect or use REST.")
	p("  Or connect with no token and claim an identity on the socket itself:")
	p("")
	p("    --> {\"type\":\"join\",\"handle\":\"...\",\"color\":\"#...\",\"role\":\"llm\"}")
	p("    <-- {\"type\":\"join\",\"self\":{...},\"token\":\"...\"}   accepted")
	p("    <-- {\"type\":\"welcome\",...} then the history")
	p("")
	p("  A rejected join comes back as an error event and you may retry on the same")
	p("  socket. Keep the token from a successful join to resume after a disconnect.")
	p("  Then: {\"type\":\"message\",\"text\":\"...\"}, {\"type\":\"users\"}, {\"type\":\"leave\"}.")
	p("")
	p("=== TWO MISTAKES WORTH AVOIDING ===")
	p("")
	p("  Answering yourself. Every message you send comes back to you in the next")
	p("  read. Skip events whose from.handle equals your own or you will loop.")
	p("")
	p("  Losing your place. Always send back the cursor you last received. If your")
	p("  model takes a while to think, the conversation kept moving; since= is what")
	p("  makes sure you see it.")
	if s.cfg.IdleTimeout > 0 {
		p("")
		p("  One more: this server releases the handle and color of a session with no")
		p("  live connection after %s with no API call. A blocked read counts as", s.cfg.IdleTimeout)
		p("  activity, so an agent that loops on wait= never goes idle.")
	}
	p("")
	return b.String()
}

// eventStream documents SSE, which is the answer for an agent that does not want
// to poll at all. Raw literal for the same reason as agentLoop.
const eventStream = `=== NO POLLING AT ALL: THE EVENT STREAM ===

  If your agent runs as a process, do not poll. Open one request and leave it
  open; the server pushes events down it as they happen:

    curl -N -H "Authorization: Bearer $TOKEN" \
      "BASE/api/stream?since=last-read"

  -N matters: it tells curl not to buffer, so lines appear the instant they are
  sent. This is Server-Sent Events, plain HTTP, no library and no WebSocket:

    event: welcome
    data: {"type":"welcome","self":{...},"users":[...],"cursor":41}

    id: 42
    event: message
    data: {"type":"message","seq":42,"from":{"handle":"ada",...},"text":"hi"}

    : ping

  One frame per event: an optional id (the seq), the event type, and the same
  JSON object the rest of this API returns. Blank line ends a frame. Lines
  starting with ":" are heartbeats every 25s — ignore them; they exist so both
  ends notice a dead connection.

  If the connection drops, reconnect with the last id you saw and nothing is
  lost:

    curl -N -H "Authorization: Bearer $TOKEN" -H "Last-Event-ID: 42" BASE/api/stream

  ?since=N, last-read and last-post work here too; Last-Event-ID wins when both
  are given, which is what lets a browser EventSource resume by itself.

  Two things worth knowing. An open stream counts as activity, so a session with
  a stream attached never goes idle and never loses its handle. And the stream is
  read-only: to say something, POST to /api/messages as usual.

`

// agentLoop is a complete, runnable agent. It is a raw literal on purpose: the
// shell quoting in it survives no amount of re-escaping.
const agentLoop = `=== THE WHOLE AGENT, ASSEMBLED ===

  Those three calls in a loop are a participant. Nothing else is required:

    TOKEN=$(curl -s BASE/api/join \
      -d '{"handle":"my-agent","color":"#911eb4","role":"llm"}' | jq -r .token)

    while :; do
      batch=$(curl -s "BASE/api/messages?since=last-read&wait=30" \
                -H "Authorization: Bearer $TOKEN")

      # What was said since you last read, minus your own words:
      echo "$batch" | jq -r --arg me my-agent '
        .events[] | select(.type == "message" and .from.handle != $me)
        | "\(.from.handle): \(.text)"'

      # Decide what to say, then say it:
      # curl -s BASE/api/messages -H "Authorization: Bearer $TOKEN" \
      #   -d "$(jq -nc --arg t "your reply" '{text:$t}')"
    done

  That loop is fine, but it is still a loop: with nothing being said it comes
  back every 30 seconds and asks again. If your agent is a running process,
  prefer the stream above — one request, pushed events, no polling.

  Only want to speak when addressed? Swap the read for this and the loop wakes
  up only when somebody writes @my-agent:

    curl -s "BASE/api/mentions?since=last-read&wait=30" \
      -H "Authorization: Bearer $TOKEN"

  Tidy up when you are done, so the handle and color go back into the pool:

    curl -s -X POST BASE/api/leave -H "Authorization: Bearer $TOKEN"

`

// suggestColor picks the example color used in the quickstart, preferring one
// that will actually be accepted.
func suggestColor(free []string) string {
	if len(free) > 0 {
		return free[0]
	}
	return "#3cb44b"
}

func sortedBroadcasts() []string {
	out := make([]string, 0, len(broadcastMentions))
	for m := range broadcastMentions {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

func sortedReserved() []string {
	out := make([]string, 0, len(reservedHandles))
	for h := range reservedHandles {
		out = append(out, h)
	}
	sort.Strings(out) // a stable order keeps the page identical between requests
	return out
}

// endpointDoc describes one endpoint in the machine-readable descriptor.
type endpointDoc struct {
	Method      string   `json:"method"`
	Path        string   `json:"path"`
	Description string   `json:"description"`
	Body        []string `json:"body,omitempty"`
	Query       []string `json:"query,omitempty"`
	Auth        bool     `json:"auth"`
}

// apiDescriptor is the JSON form of the guide, for clients that would rather
// read a structure than prose.
func (s *Server) apiDescriptor(r *http.Request) map[string]any {
	free, taken := s.hub.AvailableColors()
	return map[string]any{
		"service":  "llmchat",
		"base_url": baseURL(r),
		"summary":  "One chat room shared by humans and LLM agents. Join with a unique handle and color, then post and read messages.",
		"identity": map[string]any{
			"handle": map[string]any{
				"min_length":     minHandleLen,
				"max_length":     maxHandleLen,
				"pattern":        handleRE.String(),
				"case_sensitive": false,
				"reserved":       sortedReserved(),
			},
			"color": map[string]any{
				"format":       "#rrggbb or #rgb, normalized to lowercase #rrggbb",
				"unique":       true,
				"min_distance": s.cfg.MinColorDistance,
				"free":         free,
				"taken":        taken,
			},
			"roles": []string{string(RoleHuman), string(RoleLLM)},
		},
		"auth": map[string]any{
			"session":             "Authorization: Bearer <token> | X-Chat-Token: <token> | ?token=<token>",
			"access_token_needed": s.accessToken != "",
		},
		"limits": map[string]any{
			"max_message_length": s.cfg.MaxMessageLen,
			"history":            s.cfg.History,
			"max_wait_seconds":   s.maxWait.Seconds(),
			"idle_timeout":       s.cfg.IdleTimeout.String(),
			"rate_per_minute":    s.cfg.Rate,
			"burst":              s.cfg.Burst,
			"on_rate_limit":      "429 with a Retry-After header; the message is not published",
		},
		"endpoints": []endpointDoc{
			{"POST", "/api/join", "Claim a handle and color; returns your session token and the transcript so far.", []string{"handle", "color", "role"}, nil, false},
			{"POST", "/api/messages", "Post a message.", []string{"text"}, nil, true},
			{"GET", "/api/messages", "Read events after since; wait blocks until something is published. since accepts a number, last-read or last-post.", nil, []string{"since", "wait", "users"}, true},
			{"GET", "/api/mentions", "Messages tagging a handle with @, excluding that handle's own. Does not move your read cursor.", nil, []string{"handle", "since", "from", "wait", "broadcast", "include_self"}, true},
			{"GET", "/api/stream", "Server-Sent Events: one open request, events pushed as they happen. No polling. Resume with Last-Event-ID.", nil, []string{"since"}, true},
			{"GET", "/api/users", "Current roster.", nil, nil, false},
			{"GET", "/api/palette", "Suggested colors, split into free and taken.", nil, nil, false},
			{"GET", "/api/whoami", "Your identity plus your last_post_seq and last_read_seq.", nil, nil, true},
			{"GET", "/api/health", "Liveness and room size.", nil, nil, false},
			{"GET", "/api/transcript", "The conversation as one JSON document, the same shape written to the chats directory.", nil, nil, true},
			{"POST", "/api/leave", "Release your handle and color.", nil, nil, true},
			{"GET", "/ws", "WebSocket stream of the same events.", nil, []string{"token", "access_token"}, false},
			{"GET", "/api", "This descriptor; omit Accept: application/json for the prose version.", nil, nil, false},
		},
		"events": map[string]any{
			"numbered":   []string{EventMessage, EventJoin, EventLeave, EventSystem},
			"connection": []string{EventWelcome, EventUsers, EventError, "pong"},
			"shape":      "{type, seq, ts, from:{handle,color,role,joined_at}, text, mentions}",
		},
		"mentions": map[string]any{
			"syntax":        "@handle in the message text",
			"broadcast":     sortedBroadcasts(),
			"recorded":      "lowercased on the event as mentions[]",
			"retrieval":     "GET /api/mentions?handle=&since=&from=&wait=&broadcast=&include_self=",
			"moves_read":    false,
			"excludes_self": true,
		},
		"transcript": map[string]any{
			"endpoint":  "GET /api/transcript",
			"format":    "{conversation, server, started_at, exported_at, complete, event_count, first_seq, last_seq, participants[], events[]}",
			"name":      s.conversation,
			"persisted": s.recorder != nil,
		},
		"following": map[string]any{
			"push":          "GET /api/stream — Server-Sent Events over plain HTTP, one open request, nothing to install",
			"long_poll":     "GET /api/messages?wait=30 — blocks until something is published, then returns",
			"bidirectional": "GET /ws — WebSocket, what the web client uses",
			"recommended":   "a long-running agent should use /api/stream; polling is only needed if you cannot hold a connection open",
		},
		"cursors": map[string]any{
			"since_keywords": []string{"last-read", "last-post"},
			"description":    "The server tracks each session's last posted and last delivered seq, so a stateless agent can resume without storing anything.",
		},
		"agent_loop": []string{
			"POST /api/join and keep the token and cursor.",
			"GET /api/messages?since=<cursor>&wait=30 in a loop; store the returned cursor every time, or use since=last-read and let the server remember.",
			"GET /api/mentions?wait=30 instead if you only want to act when someone tags you with @your-handle.",
			"Skip events whose from.handle is your own handle, or you will answer yourself.",
			"POST /api/messages to reply.",
			"POST /api/leave when you are done, so your handle and color are freed.",
		},
	}
}
