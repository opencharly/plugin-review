package pluginreview

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// The gateways that REQUIRE session affinity must get it. opencode's Go gateway answers
// HTTP 400 MissingSessionID without `x-opencode-session` and HTTP 200 with it (verified
// against the live gateway on 2026-09-12), so a review that omits the header can never
// produce a verdict. This pins both halves: the header is SENT, and every request of ONE
// run carries the SAME value (a per-request id would defeat the routing it exists for).
func TestOpencodeSessionHeaderSentAndStablePerRun(t *testing.T) {
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		mu.Lock()
		got = append(got, req.Header.Get("x-opencode-session"))
		mu.Unlock()
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusBadRequest)
		_, _ = rw.Write([]byte("no session"))
	}))
	defer srv.Close()
	cfg := reviewConfig{
		Provider: "opencode", Model: "m", BaseURL: srv.URL, APIKey: "k",
		MaxTurns: 1, AttemptTimeout: 3 * time.Second,
	}
	_, _ = runAgentLoop(context.Background(), cfg, "prompt", toolSet{})
	mu.Lock()
	defer mu.Unlock()
	if len(got) == 0 {
		t.Fatal("no request reached the test server")
	}
	first := got[0]
	if first == "" {
		t.Fatal("x-opencode-session must be sent: the gateway 400s MissingSessionID without it")
	}
	if len(first) != 32 {
		t.Fatalf("session id = %q, want 32 hex chars", first)
	}
	for i, s := range got {
		if s != first {
			t.Fatalf("attempt %d sent session %q; every request of a run must share ONE session (%q)", i, s, first)
		}
	}
	t.Logf("captured %d request(s), session=%s", len(got), first)
}
