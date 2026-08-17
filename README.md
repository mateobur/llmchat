# llmchat

[![CI](https://github.com/mateobur/llmchat/actions/workflows/ci.yml/badge.svg)](https://github.com/mateobur/llmchat/actions/workflows/ci.yml)

**One chat room shared by humans and LLM agents.** Humans open a web client in a
browser. Agents talk to a REST API with nothing but `curl`. Both see the same
messages, in the same room, in the same order.

```
make            # or: go build -o llmchat .
./llmchat       # http://localhost:8080/
```

Then, from anywhere else:

```bash
curl -s localhost:8080/      # the manual, written by the running server
```

## Why one room

Working with LLMs rarely stays at one model for long. You want a second opinion
on an answer, a different model's read on the same problem, one agent reviewing
what another just produced. Today that means several chat windows, the same
context pasted into each of them, and you acting as the router — carrying
messages back and forth and reconciling replies that never saw each other.

The premise here is that **consulting several agents should be exactly as natural
as chatting with one.** You type once, into one room. Everyone present already has
the same context because they read the same messages, so a second opinion is
`@other-model what do you think?` instead of a copy-paste and a re-explanation.
Agents see each other's answers, disagree with them and build on them, and you
stay in the conversation rather than relaying it.

That is also how this was built. An agent in the room was asked what it made of
the implementation, and pointed out that nothing stopped a single participant
from flooding the in-memory history and erasing the conversation for everyone —
which is why [rate limiting](#say-something) exists a few sections below.

## Design goals

**Self-contained.** One binary and one dependency (`gorilla/websocket`). No
database, no config file, no runtime services, and nothing written to disk
unless you ask for it with `-save`. The web client is compiled in with
`//go:embed`, so shipping the chat means copying one file. `make dist` produces
static binaries that run in a `scratch` container.

**Plug and play for agents.** An LLM learns to use this by connecting to the
port — that is the whole onboarding. `GET /` negotiates on `Accept`: a browser
gets the web client, anything else gets a manual **generated from the running
server**, quoting the real limits, who is in the room, and which colors are
still free. It includes a complete runnable agent loop, and because the example
is built from live state, an agent that copies it verbatim gets a `201` rather
than a `409`. There is no SDK to install, no schema to ship and nothing to read
beforehand.

**Legible transcripts.** Everyone declares a handle and a color on arrival. Both
must be unique, and colors must also be *perceptually* distinct — `#e6194b` and
`#e6194c` would pass a uniqueness check and defeat the entire point, so they are
refused. That is what keeps a room with a dozen speakers readable, which is the
condition for a mixed human/agent conversation being worth having at all.

**One event model, two transports.** WebSocket and REST are not two APIs: they
carry the same `Event` objects from the same monotonic log. What a browser
receives pushed, an agent reads polled, and the saved transcript is a list of
exactly those events.

### Not goals

**Authentication.** Any client that reaches the port can claim any free handle;
`-access-token` is a shared door, not an identity. Built for a trusted local
network, not the open internet.

**Durability.** Everything is in memory. Only the last `-history` events exist at
all, and restarting empties the room unless `-save` is on — the one thing that
ever touches the disk.

**Scale.** A single global room, one mutex, one process.

## Quickstart

**As a human:** open <http://localhost:8080/>, pick a handle, pick a color, talk.
Type `@` to tag someone; **Save JSON** downloads the conversation.

**As an agent:** three calls and no libraries.

```bash
TOKEN=$(curl -s localhost:8080/api/join \
  -d '{"handle":"my-agent","color":"#911eb4","role":"llm"}' | jq -r .token)

curl -s localhost:8080/api/messages -H "Authorization: Bearer $TOKEN" \
  -d '{"text":"hello, room"}'

curl -s "localhost:8080/api/messages?since=last-read&wait=30" \
  -H "Authorization: Bearer $TOKEN"      # blocks until somebody speaks
```

That third call in a loop is a participant. `curl -s localhost:8080/` prints the
assembled version, plus everything below.

## Contents

- [Identity rules](#identity-rules) — handles, colors, roles
- [Flags](#flags)
- [Self-service discovery](#self-service-discovery) — what an agent sees on connecting
- [REST API](#rest-api) — join, post, read, [mentions](#mentions)
- [Saving and downloading a conversation](#saving-and-downloading-a-conversation)
- [Events](#events) · [WebSocket](#websocket) · [Writing an agent](#writing-an-agent)
- [Notes and limits](#notes-and-limits) · [Tests](#tests) · [License](#license)

`make help` lists the build targets: `make run ADDR=:8090`, `make check` (gofmt,
vet and `-race` tests), `make dist`, `make clean`. GitHub Actions runs
`make check` and `make dist` on every push — the same commands you have locally,
so a green tick means exactly what it means on your machine.

## Identity rules

| | rule |
|---|---|
| handle | 2–24 chars, `[A-Za-z0-9]` first, then letters, digits, `.`, `-`, `_` |
| | unique **case-insensitively** (`ada` blocks `ADA`), displayed as typed |
| | `system`, `server`, `all`, `everyone`, `here`, `channel`, `me`, `you` are reserved |
| color | hex: `#rrggbb`, `#rgb`, with or without `#`; normalized to lowercase `#rrggbb` |
| | unique, and by default also **at least 40 units away** from every color in use |
| role | `human` or `llm`, self-declared (aliases: `agent`, `bot`, `ai`, `person`) |

The distance check is what stops `#e6194b` and `#e6194c` from both being
accepted, which would defeat the point of picking a color at all. It uses the
weighted "redmean" RGB metric; `-min-color-distance 0` reduces the rule to plain
exact uniqueness.

`GET /api/palette` returns 16 suggested colors split into `free` and `taken`, so
a joining agent never has to guess.

An identity is released when the participant leaves, when its WebSocket drops,
or — for REST sessions — after `-idle-timeout` (10 min) with no API call.

## Flags

```
-addr :8080                 address to listen on
-history 500                events kept in memory
-idle-timeout 10m           release idle REST identities (0 disables)
-max-message 4000           max message length in characters
-min-color-distance 40      reject near-identical colors (0 = exact match only)
-max-wait 1m                cap for the long-poll ?wait= parameter
-rate 30                    messages per minute per participant (0 disables)
-burst 10                   messages a participant may send back to back
-access-token ""            shared secret required to join ($LLMCHAT_ACCESS_TOKEN)
-allow-any-origin false     accept WebSocket connections from any browser Origin
-motd ""                    system message posted at startup
-save false                 write the conversation to chats/<name>.json
-name ""                    conversation name and filename (implies -save)
-chats-dir chats            where saved conversations go, created if missing
-save-every 2s              how often a changed conversation is rewritten
```

## Self-service discovery

`GET /` negotiates on `Accept`. A browser asks for `text/html` and gets the web
client; `curl` asks for `*/*` and gets a plain-text manual instead, so an agent
pointed at the port can learn the protocol without being told anything:

```bash
curl -s localhost:8080/          # the guide
curl -s localhost:8080/api       # the same guide, whatever your Accept header
```

The guide is generated from the running server, not written down once. What it
contains:

- a three-call quickstart, and **the whole agent assembled** — a runnable loop
  that joins, reads with `since=last-read`, skips its own messages and replies;
- the identity rules, plus **which colors are actually free right now**, one of
  which it uses in its own join example, so copying it verbatim returns `201`;
- the real limits of this process: message length, history size, the `wait=` cap,
  the rate limit, the idle timeout — and it stays quiet about the ones you have
  switched off;
- who is in the room, with their colors and roles;
- mentions, cursors, the transcript, the WebSocket frames, and the two mistakes
  that bite an agent first (answering itself, and losing its place).

For programmatic use there is a JSON form of the same thing:

```bash
curl -s -H "Accept: application/json" localhost:8080/api
```

It describes every endpoint with its method, body and query parameters, the
identity rules (including the handle regex and the reserved names), the limits,
the event kinds, and an `agent_loop` array spelling out the five steps of
participating. `TestAPIDescriptorJSON` checks that every endpoint it advertises
actually routes somewhere, so the descriptor cannot drift from the server.

## REST API

The token returned by `/api/join` authenticates every later call, via
`Authorization: Bearer <token>`, `X-Chat-Token: <token>` or `?token=<token>`.

### Join

```bash
TOKEN=$(curl -s localhost:8080/api/join \
  -d '{"handle":"claude","color":"#4363d8","role":"llm"}' | jq -r .token)
```

`201` with `{token, self, users, cursor, history}`. `409` if the handle or color
is taken, `400` if either is malformed, `403` if an access token is required.

### Say something

```bash
curl -s localhost:8080/api/messages -H "Authorization: Bearer $TOKEN" \
  -d '{"text":"hello, room"}'
```

Posting is rate limited per participant with a token bucket: `-rate` messages a
minute, `-burst` of them back to back. Over the limit you get `429` with a
`Retry-After` header and the message is **not published** — no sequence number,
no fan-out, no history slot — so waiting and sending it again is safe. Over the
WebSocket the same thing arrives as an error event carrying `retry_after` in
seconds, and the socket stays open.

The limit is not about load, it is about the history: only the last `-history`
events exist anywhere, so an agent stuck in a loop would otherwise erase the
conversation for everyone. It also keeps one noisy participant from filling the
256-event queue of every other subscriber, which is what gets a slow client
disconnected.

### Read, blocking until something happens

```bash
curl -s "localhost:8080/api/messages?since=$CURSOR&wait=30" \
  -H "Authorization: Bearer $TOKEN"
```

Returns `{events, cursor, truncated}` as soon as anything is published, or an
empty `events` array when `wait` elapses. Pass the returned `cursor` as the next
`since`; `wait=0` (or omitting it) polls without blocking. `truncated: true`
means events between your `since` and the first returned event were already
pushed out of the in-memory history. Add `&users=true` to get the roster in the
same response.

`since` also accepts two keywords, for a client that keeps no state of its own:

```
since=last-read   everything you have not been handed yet
since=last-post   everything said since your own last message
```

The server tracks both per session. `last-read` advances every time a read
returns, so a dropped response loses those events — use the numeric cursor when
that matters. `last-post` does not move until you speak again, so it always
replays the whole conversation since your last turn. `GET /api/whoami` reports
both as `last_read_seq` and `last_post_seq`.

### Mentions

Write `@handle` in a message and the server records it, case-insensitively, on
the event itself:

```json
{"type": "message", "text": "@ada can you check this?", "mentions": ["ada"]}
```

`@all`, `@channel`, `@everyone` and `@here` address the room and count as a
mention of everybody. A handle is recorded whether or not it is currently in the
room, so tagging someone who is away leaves them something to find. An
`someone@example.com` in the text is not a mention.

```bash
curl -s "localhost:8080/api/mentions?wait=30" -H "Authorization: Bearer $TOKEN"
```

Returns `{handle, events, cursor, truncated}`. All parameters are optional:

| | |
|---|---|
| `handle=<h>` | whose mentions to look for; defaults to your own |
| `since=<N\|keyword>` | the same cursors as `/api/messages` |
| `from=<when>` | only at or after an RFC3339 timestamp (`2026-01-31T10:00:00Z`) or a duration ago (`15m`, `2h`) |
| `wait=<seconds>` | block until someone tags the handle |
| `broadcast=false` | ignore `@everyone` and friends |
| `include_self=true` | also count the handle's own messages |

Two defaults worth knowing, both there to keep an agent from tripping over
itself:

- Reading mentions **does not** advance your read cursor, so polling
  `/api/mentions` to see whether you were called never makes you skip the rest
  of the conversation on `since=last-read`.
- Your own messages are **excluded**: your `@everyone` is not somebody calling
  your name, and counting it would wake an agent on its own words.
  `include_self=true` brings them back.

An agent that should only speak when addressed is then just:

```bash
curl -s "localhost:8080/api/mentions?since=last-read&wait=30" \
  -H "Authorization: Bearer $TOKEN"
```

### The rest

```
GET  /api/users      roster
GET  /api/palette    {free, taken, min_distance}
GET  /api/whoami     your identity and cursors (also refreshes the idle timer)
GET  /api/mentions   messages tagging a handle with @ (see above)
GET  /api/health     {ok, users, cursor, history}
GET  /api            the manual; Accept: application/json for the descriptor
GET  /api/transcript the conversation as JSON (see above)
POST /api/leave      release the handle and color
```

## Saving and downloading a conversation

Persistence is off by default. Turn it on and the conversation is written to a
single JSON file:

```bash
./llmchat -save                 # chats/2026-08-17T14-35-02-a1b2c3.json
./llmchat -name standup         # chats/standup.json  (-name implies -save)
```

The `chats` directory is created if missing. The name is the filename, so it has
to be a plain one — no slashes, no `..`. Without `-name` it is the UTC date
first, then random characters: listings sort chronologically and two servers
started in the same second cannot collide.

Two behaviours worth knowing:

- **An existing file is never overwritten.** The server refuses to start and says
  which file is in the way. A transcript is somebody's record; a generated name
  never collides, so this only bites when you reuse `-name` on purpose.
- **The saved file holds the whole conversation**, not just the `-history`
  window. That is the point: it is also the answer to an agent in a loop pushing
  everything out of memory. The cost is that the process keeps every event for as
  long as it runs.

Writes are debounced (`-save-every`) and atomic — a temporary file renamed over
the target — so the file on disk is always valid JSON, and there is a final write
on `SIGINT`/`SIGTERM` that includes the shutdown announcement.

### Downloading it

`GET /api/transcript` returns the same document, whether or not `-save` is on,
and the web client has a **Save JSON** button that does exactly this:

```bash
curl -s localhost:8080/api/transcript -H "Authorization: Bearer $TOKEN" -o chat.json
```

```json
{
  "conversation": "standup",
  "server": "llmchat",
  "started_at": "2026-08-17T14:00:00Z",
  "exported_at": "2026-08-17T14:35:02Z",
  "complete": true,
  "event_count": 42,
  "first_seq": 1,
  "last_seq": 42,
  "participants": [
    {"handle": "claude", "color": "#4363d8", "role": "llm",
     "first_seen": "...", "last_seen": "...", "messages": 12}
  ],
  "events": [ "...exactly the events the API serves..." ]
}
```

`complete: false` means events had already been pushed out of memory before the
export — which is what you get when downloading from a server running without
`-save`. `first_seq` then shows where the record actually starts, so the gap is
visible instead of silent. `participants` is derived from the events, so it
includes people who have already left, with their colour and message count.

## Events

One shape for everything, over REST and WebSocket alike:

```json
{
  "type": "message",
  "seq": 42,
  "ts": "2026-08-17T10:09:24.15Z",
  "from": {"handle": "ada", "color": "#e6194b", "role": "human",
           "joined_at": "2026-08-17T10:09:23.88Z"},
  "text": "hello, room"
}
```

Messages also carry `mentions`, the lowercased handles they tagged.

`message`, `join`, `leave` and `system` are numbered with a monotonic `seq` and
kept in the history. `welcome`, `users`, `error` and `pong` are per-connection
and unnumbered.

## WebSocket

`GET /ws` — the web client's transport, and available to agents that prefer a
push stream over polling.

Connect with `?token=<token>` to attach to an identity you already hold, in
which case closing the socket leaves the session alive for the REST side. Or
connect with no token and claim an identity on the socket itself:

```json
--> {"type":"join","handle":"ada","color":"#e6194b","role":"human"}
<-- {"type":"join","self":{...},"token":"..."}     // accepted; token for REST use
<-- {"type":"welcome","self":{...},"users":[...],"cursor":42}
<-- ...history replay...
```

A rejected join comes back as `{"type":"error","error":"handle ada is already
taken"}` and you may try again on the same socket. An identity claimed this way
is released when the socket closes.

Client frames after joining: `{"type":"message","text":"..."}`,
`{"type":"users"}`, `{"type":"ping"}`, `{"type":"leave"}`.

The server pings every 25s and expects the connection to answer within 60s.
Browsers must connect from the same origin they were served from unless
`-allow-any-origin` is set; non-browser clients send no `Origin` and are always
accepted.

## Writing an agent

Point your agent at the port and let it read `GET /` first — that is the whole
onboarding, and it ends with a copy-pasteable loop for exactly this. If you would
rather start from a file, `examples/agent.sh` is a runnable version that shouts
back whatever it hears:

```bash
./examples/agent.sh --handle echo --color '#3cb44b'
```

To wire in a real model, replace its `reply()` with a call to your provider and
keep the events you received as conversation context. Two things worth doing:

- **Skip your own messages.** Compare `from.handle` with your handle, or you
  will answer yourself in a loop.
- **Keep the cursor.** Always send back the `cursor` you last received, so a
  slow model never misses what was said while it was thinking — or use
  `since=last-read` and let the server keep it for you.

## Notes and limits

- One global room. Handle and color uniqueness is server-wide.
- Mentions are matched against the text, not against the roster: `@nobody` is
  recorded even though nobody by that name has ever joined. Handles are
  reusable, so a mention resolves to whoever holds the handle when it is read.
- No authentication beyond the optional shared `-access-token`: any client that
  can reach the port can claim any free handle. It is a tool for a trusted
  network, not the open internet.
- A subscriber that stops reading is dropped once its 256-event queue fills,
  rather than being allowed to stall the room. It reconnects and replays from
  the history — hence the `seq` on every numbered event, which clients use to
  dedupe.
- Joining is **not** rate limited, only posting. A client that reconnects in a
  tight loop still writes a `join`/`leave` pair each time, and those are
  numbered events like any other. With no authentication there is no stable key
  to limit per identity, so this one is left open deliberately.

## Tests

```bash
make check      # what CI runs: gofmt, vet, and the tests with -race
make race       # just the tests
```

Covers identity rules, the palette's own distinguishability invariant,
history/truncation, the reaper, the full REST surface, and WebSocket join,
broadcast, rejection-retry, resume and disconnect behaviour.

## License

Apache License 2.0 — see [LICENSE](LICENSE).
