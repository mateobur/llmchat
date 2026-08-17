package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	var (
		addr           = flag.String("addr", ":8080", "address to listen on")
		history        = flag.Int("history", 500, "number of recent events kept in memory")
		idleTimeout    = flag.Duration("idle-timeout", 10*time.Minute, "release the handle and color of REST sessions idle for this long (0 disables)")
		maxMessage     = flag.Int("max-message", 4000, "maximum message length in characters")
		minColorDist   = flag.Float64("min-color-distance", 40, "reject colors closer than this to one in use (0 allows anything but exact duplicates)")
		maxWait        = flag.Duration("max-wait", 60*time.Second, "cap for the long-poll ?wait= parameter")
		rate           = flag.Float64("rate", 30, "messages per minute allowed per participant (0 disables the limit)")
		burst          = flag.Float64("burst", 10, "messages a participant may send back to back")
		accessToken    = flag.String("access-token", os.Getenv("LLMCHAT_ACCESS_TOKEN"), "shared secret required to join (empty means open); also read from LLMCHAT_ACCESS_TOKEN")
		allowAnyOrigin = flag.Bool("allow-any-origin", false, "accept WebSocket connections from any browser Origin")
		motd           = flag.String("motd", "", "system message posted when the server starts")
		save           = flag.Bool("save", false, "write the conversation to a JSON file under -chats-dir (implied by -name)")
		name           = flag.String("name", "", "conversation name; also the filename. Defaults to the date plus random characters. Giving one implies -save")
		chatsDir       = flag.String("chats-dir", "chats", "directory for saved conversations, created if missing")
		saveEvery      = flag.Duration("save-every", 2*time.Second, "how often the saved conversation is rewritten when it has changed")
	)
	flag.Usage = usage
	flag.Parse()

	if *history < 1 {
		fatal("-history must be at least 1")
	}
	if *maxMessage < 1 {
		fatal("-max-message must be at least 1")
	}
	if *minColorDist < 0 {
		fatal("-min-color-distance must not be negative")
	}
	if *rate < 0 {
		fatal("-rate must not be negative")
	}
	if *rate > 0 && *burst < 1 {
		fatal("-burst must be at least 1, or no message could ever be sent")
	}

	logger := log.New(os.Stderr, "", log.LstdFlags)

	// A conversation always has a name: without -save it is only the filename
	// the download button suggests.
	conversation := GenerateConversationName(time.Now())
	if *name != "" {
		valid, err := ValidateConversationName(*name)
		if err != nil {
			fatal(err.Error())
		}
		conversation, *save = valid, true
	}
	if *saveEvery <= 0 {
		fatal("-save-every must be positive")
	}

	cfg := Config{
		History:          *history,
		IdleTimeout:      *idleTimeout,
		MaxMessageLen:    *maxMessage,
		MinColorDistance: *minColorDist,
		SendBuffer:       256,
		Rate:             *rate,
		Burst:            *burst,
	}
	hub := NewHub(cfg)
	srv := &Server{
		hub:            hub,
		cfg:            cfg,
		accessToken:    *accessToken,
		maxWait:        *maxWait,
		allowAnyOrigin: *allowAnyOrigin,
		conversation:   conversation,
		logger:         logger,
	}

	// Set up persistence before the motd, so the first event is recorded too.
	var recorder *Recorder
	if *save {
		var err error
		recorder, err = NewRecorder(*chatsDir, conversation, logger)
		if err != nil {
			fatal(err.Error())
		}
		hub.SetRecorder(recorder)
		srv.recorder = recorder
	}
	if *motd != "" {
		hub.Announce(*motd)
	}

	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           withRecovery(srv.Routes()),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: WebSocket connections and long polls are long-lived.
		IdleTimeout: 2 * time.Minute,
	}

	// Bind before announcing anything, so a busy port fails loudly and early.
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		fatal(err.Error())
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go reapLoop(ctx, hub, logger)
	if recorder != nil {
		go recorder.Run(ctx, *saveEvery)
		logger.Printf("saving this conversation to %s", recorder.Path())
	}

	logger.Printf("llmchat listening on %s — web client at http://localhost%s/",
		ln.Addr(), portOf(ln.Addr().String()))
	if srv.accessToken != "" {
		logger.Printf("access token required to join")
	}

	errCh := make(chan error, 1)
	go func() {
		if err := httpServer.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		fatal(err.Error())
	case <-ctx.Done():
		logger.Printf("shutting down")
	}

	hub.Announce("server is shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Printf("shutdown: %v", err)
	}

	// Last write, including the shutdown announcement. Run also flushes when the
	// context ends, but doing it here means the log line is truthful.
	if recorder != nil {
		if err := recorder.Flush(); err != nil {
			logger.Printf("writing %s: %v", recorder.Path(), err)
		} else {
			logger.Printf("conversation saved to %s", recorder.Path())
		}
	}
}

// reapLoop frees handles and colors held by sessions that went quiet.
func reapLoop(ctx context.Context, hub *Hub, logger *log.Logger) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if n := hub.ReapIdle(now); n > 0 {
				logger.Printf("reaped %d idle session(s)", n)
			}
		}
	}
}

// withRecovery keeps one panicking handler from taking down the whole chat.
func withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				log.Printf("panic serving %s %s: %v", r.Method, r.URL.Path, v)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func portOf(addr string) string {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[i:]
		}
	}
	return ":" + addr
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "llmchat: "+msg)
	os.Exit(2)
}

func usage() {
	out := flag.CommandLine.Output()
	fmt.Fprint(out, `llmchat — a chat room shared by humans and LLM agents.

Every participant declares a handle and a color on arrival; both must be
unique. Humans use the built-in web client, agents use the REST API — and can
learn it by fetching the bare URL, which serves a manual to non-browsers.

Usage:
  llmchat [flags]

Flags:
`)
	flag.PrintDefaults()
	fmt.Fprint(out, `
REST API (see README.md for details):
  POST /api/join       {"handle":"claude","color":"#4363d8","role":"llm"} -> token
  POST /api/messages   {"text":"hello"}            Authorization: Bearer <token>
                       429 + Retry-After when you post too fast
  GET  /api/messages?since=N&wait=30               long-poll for new events
  GET  /api/mentions?handle=&from=15m&wait=30       messages tagging @handle
  GET  /api/transcript                             the conversation as JSON
  GET  /api/users      GET /api/palette            GET /api/whoami
  GET  /api            self-service manual (plain text, or JSON via Accept)
  POST /api/leave                                  release handle and color
  GET  /ws                                         WebSocket, same events
`)
}
