package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// get issues a GET with an explicit Accept header, which is what decides
// whether a caller is treated as a browser or as an agent.
func get(t *testing.T, ts *httptest.Server, path, accept string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest("GET", ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(raw)
}

const browserAccept = "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"

func TestBareURLServesTheGuideToNonBrowsers(t *testing.T) {
	ts, _ := newTestServer(t, nil)

	// curl sends */*; Go's http.Get sends no Accept at all. Both are agents.
	for _, accept := range []string{"*/*", ""} {
		resp, body := get(t, ts, "/", accept)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("Accept %q: status %d", accept, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
			t.Errorf("Accept %q: content type %q; want text/plain", accept, ct)
		}
		for _, want := range []string{"QUICKSTART", "POST /api/join", "Bearer", "wait=30", ts.URL} {
			if !strings.Contains(body, want) {
				t.Errorf("Accept %q: guide is missing %q", accept, want)
			}
		}
		if strings.Contains(body, "<!DOCTYPE html>") {
			t.Errorf("Accept %q: got the web client instead of the guide", accept)
		}
	}
}

func TestBareURLServesTheWebClientToBrowsers(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	resp, body := get(t, ts, "/", browserAccept)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if !strings.Contains(body, "<!DOCTYPE html>") {
		t.Errorf("a browser did not get the web client: %.80s", body)
	}
}

func TestGuideIsServedAtAPIRegardlessOfAccept(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	// Even a browser following the "API" link gets the prose version.
	for _, accept := range []string{"*/*", browserAccept} {
		resp, body := get(t, ts, "/api", accept)
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, "QUICKSTART") {
			t.Errorf("Accept %q: status %d, body %.60s", accept, resp.StatusCode, body)
		}
	}
}

func TestGuideReflectsLiveState(t *testing.T) {
	ts, _ := newTestServer(t, nil)

	_, body := get(t, ts, "/api", "*/*")
	if !strings.Contains(body, "Nobody is in the room yet") {
		t.Error("empty room not reported")
	}
	if !strings.Contains(body, Palette[0]) {
		t.Errorf("free palette not listed: %s", body)
	}

	restJoin(t, ts, "ada", Palette[0], "human")
	_, body = get(t, ts, "/api", "*/*")
	if !strings.Contains(body, "ada") {
		t.Error("roster missing from the guide after a join")
	}
	// The color ada took must no longer be advertised as free.
	free := body[strings.Index(body, "Free colors right now:"):]
	free = free[:strings.Index(free, "\n")]
	if strings.Contains(free, Palette[0]) {
		t.Errorf("taken color still advertised as free: %s", free)
	}
	// The quickstart example must suggest a color that would be accepted. Only
	// the join line matters: the EVENTS section quotes a fixed sample event.
	joinLine := lineContaining(t, body, `"handle":"your-name"`)
	if strings.Contains(joinLine, Palette[0]) {
		t.Errorf("the quickstart suggests a taken color: %s", joinLine)
	}
	if !strings.Contains(joinLine, Palette[1]) {
		t.Errorf("the quickstart does not suggest the first free color: %s", joinLine)
	}
}

func lineContaining(t *testing.T, body, needle string) string {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	t.Fatalf("no line containing %q in:\n%s", needle, body)
	return ""
}

func TestGuideMentionsLimitsAndAccessToken(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	_, body := get(t, ts, "/api", "*/*")
	if !strings.Contains(body, "100 characters") || !strings.Contains(body, "last 100 events") {
		t.Errorf("configured limits not in the guide")
	}
	if !strings.Contains(body, "40 units") {
		t.Error("color distance rule not in the guide")
	}
	if strings.Contains(body, "X-Access-Token") {
		t.Error("guide asks for an access token this server does not require")
	}

	gated, _ := newTestServer(t, func(s *Server) { s.accessToken = "s3cret" })
	_, body = get(t, gated, "/api", "*/*")
	if !strings.Contains(body, "X-Access-Token") {
		t.Error("guide does not mention the required access token")
	}
	if strings.Contains(body, "s3cret") {
		t.Error("guide leaks the access token value")
	}
}

func TestGuideDisclosesRelaxedColorRule(t *testing.T) {
	ts, _ := newTestServer(t, func(s *Server) { s.cfg.MinColorDistance = 0 })
	_, body := get(t, ts, "/api", "*/*")
	if !strings.Contains(body, "similarity check is disabled") {
		t.Error("guide does not say the distance check is off")
	}
}

func TestAPIDescriptorJSON(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	resp, body := get(t, ts, "/api", "application/json")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content type %q", ct)
	}

	var d struct {
		Service   string `json:"service"`
		BaseURL   string `json:"base_url"`
		Endpoints []struct {
			Method string `json:"method"`
			Path   string `json:"path"`
		} `json:"endpoints"`
		Identity struct {
			Handle struct {
				Pattern  string   `json:"pattern"`
				Reserved []string `json:"reserved"`
			} `json:"handle"`
			Color struct {
				Free        []string `json:"free"`
				MinDistance float64  `json:"min_distance"`
			} `json:"color"`
		} `json:"identity"`
		AgentLoop []string `json:"agent_loop"`
	}
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		t.Fatalf("descriptor is not valid JSON: %v", err)
	}
	if d.Service != "llmchat" || d.BaseURL != ts.URL {
		t.Errorf("service=%q base_url=%q", d.Service, d.BaseURL)
	}
	if len(d.Endpoints) < 9 {
		t.Errorf("only %d endpoints documented", len(d.Endpoints))
	}
	if len(d.Identity.Color.Free) != len(Palette) || d.Identity.Color.MinDistance != 40 {
		t.Errorf("identity.color = %+v", d.Identity.Color)
	}
	if d.Identity.Handle.Pattern == "" || len(d.Identity.Handle.Reserved) == 0 {
		t.Error("handle rules missing from the descriptor")
	}
	if len(d.AgentLoop) == 0 {
		t.Error("agent_loop missing from the descriptor")
	}

	// Every documented endpoint must actually route somewhere.
	for _, e := range d.Endpoints {
		req, _ := http.NewRequest(e.Method, ts.URL+e.Path, strings.NewReader("{}"))
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
			t.Errorf("documented endpoint %s %s answers %d", e.Method, e.Path, resp.StatusCode)
		}
	}
}
