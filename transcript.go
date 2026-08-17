package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Transcript is the JSON document written to chats/ and served by
// GET /api/transcript. The two are byte-for-byte the same shape on purpose: a
// download and a saved file are interchangeable.
type Transcript struct {
	Conversation string        `json:"conversation"`
	Server       string        `json:"server"`
	StartedAt    time.Time     `json:"started_at"`
	ExportedAt   time.Time     `json:"exported_at"`
	Complete     bool          `json:"complete"`
	EventCount   int           `json:"event_count"`
	FirstSeq     int64         `json:"first_seq"`
	LastSeq      int64         `json:"last_seq"`
	Participants []Participant `json:"participants"`
	Events       []*Event      `json:"events"`
}

// Participant summarizes someone who appears in the transcript. It is derived
// from the events, so it needs no state of its own and stays true for people
// who have already left.
type Participant struct {
	Handle    string    `json:"handle"`
	Color     string    `json:"color"`
	Role      Role      `json:"role"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	Messages  int       `json:"messages"`
}

// BuildTranscript assembles the document. complete says whether events is the
// whole conversation or only what survived the in-memory history.
func BuildTranscript(conversation string, events []*Event, complete bool) Transcript {
	t := Transcript{
		Conversation: conversation,
		Server:       "llmchat",
		ExportedAt:   time.Now().UTC(),
		Complete:     complete,
		EventCount:   len(events),
		Participants: summarize(events),
		Events:       events,
	}
	if t.Events == nil {
		t.Events = []*Event{}
	}
	if len(events) > 0 {
		t.StartedAt = events[0].TS
		t.FirstSeq = events[0].Seq
		t.LastSeq = events[len(events)-1].Seq
	}
	return t
}

func summarize(events []*Event) []Participant {
	byHandle := map[string]*Participant{}
	var order []string
	for _, ev := range events {
		if ev.From == nil {
			continue
		}
		key := HandleKey(ev.From.Handle)
		p, ok := byHandle[key]
		if !ok {
			p = &Participant{
				Handle:    ev.From.Handle,
				Color:     ev.From.Color,
				Role:      ev.From.Role,
				FirstSeen: ev.TS,
			}
			byHandle[key] = p
			order = append(order, key)
		}
		// A handle can be re-claimed with a different color; the last sighting
		// is the one a reader will recognise.
		p.Color, p.Role, p.LastSeen = ev.From.Color, ev.From.Role, ev.TS
		if ev.Type == EventMessage {
			p.Messages++
		}
	}

	out := make([]Participant, 0, len(order))
	for _, key := range order {
		out = append(out, *byHandle[key])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].FirstSeen.Before(out[j].FirstSeen) })
	return out
}

// ---------- naming ----------

var conversationNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

const maxConversationName = 80

// ValidateConversationName keeps a name usable as a plain filename: no path
// separators, no traversal, nothing that needs quoting.
func ValidateConversationName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", fmt.Errorf("conversation name is empty")
	}
	if len(name) > maxConversationName {
		return "", fmt.Errorf("conversation name is longer than %d characters", maxConversationName)
	}
	if strings.HasSuffix(name, ".json") {
		name = strings.TrimSuffix(name, ".json")
	}
	if !conversationNameRE.MatchString(name) {
		return "", fmt.Errorf("conversation name %q may only contain letters, digits, dot, dash and underscore, and must start with a letter or digit", raw)
	}
	if name == "." || name == ".." || strings.Contains(name, "..") {
		return "", fmt.Errorf("conversation name %q is not allowed", raw)
	}
	return name, nil
}

// GenerateConversationName builds "2026-08-17T14-35-02-a1b2c3": the date first
// so that a directory listing sorts chronologically, then random bytes so two
// servers started in the same second cannot collide.
func GenerateConversationName(now time.Time) string {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return now.UTC().Format("2006-01-02T15-04-05") + "-" + hex.EncodeToString(b[:])
}

// ---------- recorder ----------

// Recorder keeps every event of the conversation and writes the transcript to
// disk. It holds the full history, not just the hub's ring buffer: persisting a
// conversation that has already been trimmed would defeat the point.
type Recorder struct {
	name   string
	path   string
	logger *log.Logger

	mu     sync.Mutex
	events []*Event
	dirty  bool
	writes int
}

// NewRecorder creates the chats directory if needed and claims the file. An
// existing file is never overwritten: a transcript is somebody's record.
func NewRecorder(dir, name string, logger *log.Logger) (*Recorder, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating %s: %w", dir, err)
	}
	path := filepath.Join(dir, name+".json")
	if _, err := os.Stat(path); err == nil {
		return nil, fmt.Errorf("%s already exists; pick another -name or move it out of the way", path)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("checking %s: %w", path, err)
	}
	return &Recorder{name: name, path: path, logger: logger}, nil
}

func (r *Recorder) Name() string { return r.name }
func (r *Recorder) Path() string { return r.path }

// Record takes an event. It only appends and marks the file dirty — the disk
// write happens in Run, so publishing a message never waits on I/O.
func (r *Recorder) Record(ev *Event) {
	r.mu.Lock()
	r.events = append(r.events, ev)
	r.dirty = true
	r.mu.Unlock()
}

// Snapshot returns every event recorded so far. The second value is always
// true: a recorder has seen the whole conversation by construction.
func (r *Recorder) Snapshot() ([]*Event, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Event, len(r.events))
	copy(out, r.events)
	return out, true
}

// Flush writes the transcript if anything changed since the last write.
func (r *Recorder) Flush() error {
	r.mu.Lock()
	if !r.dirty {
		r.mu.Unlock()
		return nil
	}
	events := make([]*Event, len(r.events))
	copy(events, r.events)
	r.dirty = false
	r.mu.Unlock()

	if err := writeTranscript(r.path, BuildTranscript(r.name, events, true)); err != nil {
		r.mu.Lock()
		r.dirty = true // try again on the next tick
		r.mu.Unlock()
		return err
	}
	r.mu.Lock()
	r.writes++
	r.mu.Unlock()
	return nil
}

// Run flushes on a timer until the context is cancelled, then once more.
func (r *Recorder) Run(ctx context.Context, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			if err := r.Flush(); err != nil {
				r.logger.Printf("writing %s: %v", r.path, err)
			}
			return
		case <-ticker.C:
			if err := r.Flush(); err != nil {
				r.logger.Printf("writing %s: %v", r.path, err)
			}
		}
	}
}

// writeTranscript writes to a temporary file and renames it over the target, so
// a reader never sees a half-written document and a crash cannot corrupt one.
func writeTranscript(path string, t Transcript) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once the rename succeeds

	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(t); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
