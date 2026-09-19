package pluginreview

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/opencharly/sdk/llmkit"
)

// review_test.go — the review gate's OWN contract, everything llmkit does NOT
// own: the knob parsing, the pass/turn budget, the turn-retry policy (retry the
// FAILED TURN, never restart the loop), the bounded tool-result payload, the
// inconclusive class, and the session-affinity header. The OpenAI wire format,
// SSE decoding, tool-call assembly and the idle bound are llmkit's contract and
// are tested there (sdk/llmkit) — not re-tested here.

// sseChunk writes one streamed OpenAI chunk and flushes it.
func sseChunk(rw http.ResponseWriter, payload string) {
	fmt.Fprint(rw, "data: "+payload+"\n\n")
	if f, ok := rw.(http.Flusher); ok {
		f.Flush()
	}
}

func sseDone(rw http.ResponseWriter) {
	fmt.Fprint(rw, "data: [DONE]\n\n")
	if f, ok := rw.(http.Flusher); ok {
		f.Flush()
	}
}

// stallAfterHeaders models a provider that accepts the request then stops
// streaming. It drains the body first so the server starts its disconnect-
// detecting read.
func stallAfterHeaders(rw http.ResponseWriter, r *http.Request) {
	rw.Header().Set("Content-Type", "text/event-stream")
	rw.WriteHeader(http.StatusOK)
	if f, ok := rw.(http.Flusher); ok {
		f.Flush()
	}
	<-r.Context().Done()
}

// ---- knob parsing ----

func TestConfigDefaults(t *testing.T) {
	cfg, _, err := parseReviewArgs(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StreamIdleTimeout != defaultStreamIdleTimeout {
		t.Errorf("StreamIdleTimeout default = %v, want %v", cfg.StreamIdleTimeout, defaultStreamIdleTimeout)
	}
	if cfg.ToolResultMaxBytes != defaultToolResultMaxBytes {
		t.Errorf("ToolResultMaxBytes default = %d, want %d", cfg.ToolResultMaxBytes, defaultToolResultMaxBytes)
	}
	if cfg.MaxAttempts != defaultMaxAttempts {
		t.Errorf("MaxAttempts default = %d, want %d", cfg.MaxAttempts, defaultMaxAttempts)
	}
	if cfg.MaxTurns != defaultMaxTurns {
		t.Errorf("MaxTurns default = %d, want %d", cfg.MaxTurns, defaultMaxTurns)
	}
}

func TestConfigEnvKnobs(t *testing.T) {
	t.Setenv("AI_REVIEW_STREAM_IDLE_TIMEOUT", "45")
	t.Setenv("AI_REVIEW_TOOL_RESULT_MAX_BYTES", "1234")
	t.Setenv("AI_REVIEW_MAX_TURNS", "7")
	t.Setenv("AI_REVIEW_MAX_ATTEMPTS", "1")
	cfg, _, err := parseReviewArgs(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StreamIdleTimeout != 45*time.Second {
		t.Errorf("StreamIdleTimeout = %v, want 45s", cfg.StreamIdleTimeout)
	}
	if cfg.ToolResultMaxBytes != 1234 {
		t.Errorf("ToolResultMaxBytes = %d, want 1234", cfg.ToolResultMaxBytes)
	}
	if cfg.MaxTurns != 7 {
		t.Errorf("MaxTurns = %d, want 7", cfg.MaxTurns)
	}
	if cfg.MaxAttempts != 1 {
		t.Errorf("MaxAttempts = %d, want 1", cfg.MaxAttempts)
	}
}

func TestConfigInvalidEnvFallsBack(t *testing.T) {
	t.Setenv("AI_REVIEW_STREAM_IDLE_TIMEOUT", "bogus")
	t.Setenv("AI_REVIEW_TOOL_RESULT_MAX_BYTES", "0")
	t.Setenv("AI_REVIEW_MAX_ATTEMPTS", "0")
	cfg, _, err := parseReviewArgs(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StreamIdleTimeout != defaultStreamIdleTimeout || cfg.ToolResultMaxBytes != defaultToolResultMaxBytes || cfg.MaxAttempts != defaultMaxAttempts {
		t.Errorf("invalid values must fall back to defaults: %v/%d/%d", cfg.StreamIdleTimeout, cfg.ToolResultMaxBytes, cfg.MaxAttempts)
	}
}

// ---- the pass / turn budget ----

// TestOnePassFailsFast drives AI_REVIEW_MAX_ATTEMPTS=1: a verdict-less but
// COMPLETED pass is not re-run, so the request count equals the pass count.
func TestOnePassFailsFast(t *testing.T) {
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		sseChunk(rw, `{"choices":[{"delta":{"content":"a review with no verdict line"}}]}`)
		sseDone(rw)
	}))
	defer srv.Close()

	cfg := reviewConfig{Provider: "test", Model: "m", BaseURL: srv.URL, APIKey: "k",
		MaxTurns: 1, MaxAttempts: 1, StreamIdleTimeout: 2 * time.Second, RetryBackoff: time.Millisecond}
	_, err := runAgentLoop(context.Background(), cfg, "prompt", toolSet{})
	if err == nil {
		t.Fatal("expected an error from the single-pass loop")
	}
	if got := atomic.LoadInt32(&requests); got != 1 {
		t.Errorf("AI_REVIEW_MAX_ATTEMPTS=1: got %d turn request(s), want exactly 1", got)
	}
}

func TestZeroPassesClampsToOne(t *testing.T) {
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		sseChunk(rw, `{"choices":[{"delta":{"content":"no verdict"}}]}`)
		sseDone(rw)
	}))
	defer srv.Close()
	cfg := reviewConfig{Provider: "test", Model: "m", BaseURL: srv.URL, APIKey: "k",
		MaxTurns: 1, MaxAttempts: 0, StreamIdleTimeout: 2 * time.Second}
	if _, err := runAgentLoop(context.Background(), cfg, "prompt", toolSet{}); err == nil {
		t.Fatal("expected an error from the clamped single-pass loop")
	}
	if got := atomic.LoadInt32(&requests); got != 1 {
		t.Errorf("MaxAttempts=0 clamp: got %d turn request(s), want exactly 1", got)
	}
}

// ---- the turn-retry policy ----

// TestFailedTurnRetriesWithoutRestartingLoop is the central policy test: turn 1
// succeeds (one tool call), turn 2's first two attempts stall, the third
// succeeds. A loop restart would show up as EXTRA turn-1 requests.
func TestFailedTurnRetriesWithoutRestartingLoop(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "fixtures"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fixtures", "fx-get_pr_meta.json"), []byte(`{"title":"t"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	var mu sync.Mutex
	var reqs []map[string]any
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		reqs = append(reqs, body)
		mu.Unlock()
		switch atomic.AddInt32(&n, 1) {
		case 1:
			sseChunk(rw, `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"get_pr_meta","arguments":"{}"}}]}}]}`)
			sseDone(rw)
		case 2, 3:
			stallAfterHeaders(rw, r)
		default:
			sseChunk(rw, `{"choices":[{"delta":{"content":"## Review\n\nVerdict: PASS\n"}}]}`)
			sseDone(rw)
		}
	}))
	defer srv.Close()

	cfg := reviewConfig{Provider: "test", Model: "m", BaseURL: srv.URL, APIKey: "k", MaxTurns: 5,
		StreamIdleTimeout: 150 * time.Millisecond, RetryBackoff: time.Millisecond}
	review, err := runAgentLoop(context.Background(), cfg, "prompt", toolSet{fixture: "fx"})
	if err != nil {
		t.Fatalf("runAgentLoop: %v", err)
	}
	if !strings.Contains(review, "Verdict: PASS") {
		t.Fatalf("no verdict recovered after the stalled turn: %q", review)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reqs) != 4 {
		t.Fatalf("requests=%d want 4 (turn 1 once + 3 turn-2 attempts); a loop restart shows as extra turn-1 requests", len(reqs))
	}
	turnWithTool := 0
	for _, r := range reqs {
		if msgs, ok := r["messages"].([]any); ok {
			for _, m := range msgs {
				if mm, ok := m.(map[string]any); ok && mm["role"] == "tool" {
					turnWithTool++
					break
				}
			}
		}
	}
	if turnWithTool != 3 {
		t.Fatalf("requests carrying the tool result=%d want 3 — the retry must resume the FAILED TURN", turnWithTool)
	}
}

// ---- the inconclusive class ----

func TestTimeoutClassExhaustion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		stallAfterHeaders(rw, r)
	}))
	defer srv.Close()
	cfg := reviewConfig{Provider: "test", Model: "m", BaseURL: srv.URL, APIKey: "k",
		MaxTurns: 3, StreamIdleTimeout: 50 * time.Millisecond, RetryBackoff: time.Millisecond}
	_, err := runAgentLoop(context.Background(), cfg, "prompt", toolSet{})
	if err == nil {
		t.Fatal("expected an error from the exhausted loop")
	}
	if !strings.Contains(err.Error(), "inconclusive") {
		t.Errorf("exhaustion must yield the INCONCLUSIVE marker, got: %v", err)
	}
}

func TestTimeoutClassPositive(t *testing.T) {
	if !isTimeoutClass(context.DeadlineExceeded) {
		t.Error("context.DeadlineExceeded must classify as the timeout class")
	}
	if isTimeoutClass(context.Canceled) {
		t.Error("context.Canceled must NOT classify as the timeout class")
	}
	// llmkit's named stall class must classify (its wording is "stream stalled").
	if !isTimeoutClass(fmt.Errorf("LLM stream stalled: no chunk for 3m0s after 12 byte(s) of content")) {
		t.Error("llmkit's stream-stall error must classify as the timeout class")
	}
}

// ---- the bounded tool-result payload ----

func TestTruncateToolResult(t *testing.T) {
	if got := truncateToolResult("short", 1024); got != "short" {
		t.Fatalf("a payload under the cap must pass through: %q", got)
	}
	const cap = 4096
	got := truncateToolResult(strings.Repeat("a", cap+1000), cap)
	if !strings.HasPrefix(got, strings.Repeat("a", cap)) {
		t.Fatal("the head must be preserved verbatim")
	}
	if len(got) > cap+200 {
		t.Fatalf("truncated payload=%d bytes, want ~%d", len(got), cap)
	}
	if !strings.Contains(got, "truncated by the review harness") || !strings.Contains(got, "INCOMPLETE") {
		t.Fatalf("the cut must be announced clearly: %q", got[len(got)-120:])
	}
	multi := truncateToolResult(strings.Repeat("é", 4096), 101)
	if !utf8.ValidString(multi) {
		t.Fatalf("truncation produced invalid UTF-8: %q", multi)
	}
	if !strings.Contains(truncateToolResult(strings.Repeat("b", defaultToolResultMaxBytes+1), 0), "truncated by the review harness") {
		t.Fatal("a zero cap must fall back to the default budget")
	}
}

func TestToolResultPayloadIsBoundedInTheConversation(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "fixtures"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fixtures", "fx-get_pr_diff.json"), []byte(strings.Repeat("x", 300*1024)), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	const cap = 4096
	var mu sync.Mutex
	var second map[string]any
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if atomic.AddInt32(&n, 1) == 1 {
			sseChunk(rw, `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"get_pr_diff","arguments":"{}"}}]}}]}`)
			sseDone(rw)
			return
		}
		mu.Lock()
		second = body
		mu.Unlock()
		sseChunk(rw, `{"choices":[{"delta":{"content":"Verdict: PASS\n"}}]}`)
		sseDone(rw)
	}))
	defer srv.Close()

	cfg := reviewConfig{Provider: "test", Model: "m", BaseURL: srv.URL, APIKey: "k", MaxTurns: 4,
		StreamIdleTimeout: 2 * time.Second, RetryBackoff: time.Millisecond, ToolResultMaxBytes: cap}
	if _, err := runAgentLoop(context.Background(), cfg, "prompt", toolSet{fixture: "fx"}); err != nil {
		t.Fatalf("runAgentLoop: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	var toolContent string
	if msgs, ok := second["messages"].([]any); ok {
		for _, m := range msgs {
			if mm, ok := m.(map[string]any); ok && mm["role"] == "tool" {
				if c, ok := mm["content"].(string); ok {
					toolContent = c
				}
			}
		}
	}
	if toolContent == "" {
		t.Fatal("the turn-2 request must carry the tool result")
	}
	if len(toolContent) > cap+200 {
		t.Fatalf("tool result handed to the model is %d bytes, want ~%d", len(toolContent), cap)
	}
	if !strings.Contains(toolContent, "truncated by the review harness") {
		t.Fatal("the truncation marker is missing from the tool message")
	}
}

// ---- session affinity ----

// TestSessionHeaderSentAndStablePerRun proves the review sends x-opencode-session
// and that every request of ONE run carries the SAME value (a per-request id
// would defeat the routing it exists for). llmkit forwards Config.Headers; this
// test pins the review's contribution.
func TestSessionHeaderSentAndStablePerRun(t *testing.T) {
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Header.Get("x-opencode-session"))
		mu.Unlock()
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusBadRequest)
		_, _ = rw.Write([]byte("no session"))
	}))
	defer srv.Close()
	cfg := reviewConfig{Provider: "opencode", Model: "m", BaseURL: srv.URL, APIKey: "k",
		MaxTurns: 1, StreamIdleTimeout: 2 * time.Second, RetryBackoff: time.Millisecond,
		SessionID: newSessionID()}
	_, _ = runAgentLoop(context.Background(), cfg, "prompt", toolSet{})
	mu.Lock()
	defer mu.Unlock()
	if len(got) == 0 {
		t.Fatal("no request reached the test server")
	}
	if got[0] == "" {
		t.Fatal("x-opencode-session must be sent: the gateway 400s MissingSessionID without it")
	}
	if len(got[0]) != 32 {
		t.Fatalf("session id = %q, want 32 hex chars", got[0])
	}
	for i, s := range got {
		if s != got[0] {
			t.Fatalf("attempt %d sent session %q; every request of a run must share ONE session (%q)", i, s, got[0])
		}
	}
}

// TestSessionAffinityDisabled: an explicit empty AI_REVIEW_SESSION_ID suppresses
// the header for a non-opencode gateway.
func TestSessionAffinityDisabled(t *testing.T) {
	t.Setenv("AI_REVIEW_SESSION_ID", "")
	rc, _, err := parseReviewArgs(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rc.SessionID != "" {
		t.Fatalf("AI_REVIEW_SESSION_ID=\"\" must mint no session id, got %q", rc.SessionID)
	}
	if _, ok := llmConfig(rc).Headers["x-opencode-session"]; ok {
		t.Error("an empty SessionID must suppress the session-affinity header")
	}
}

// TestSessionIDStableAcrossPasses: the session id is minted ONCE per run, so
// every pass/turn of a multi-pass run carries the SAME value.
func TestSessionIDStableAcrossPasses(t *testing.T) {
	rc, _, err := parseReviewArgs(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rc.SessionID == "" {
		t.Fatal("a normal run must mint a session id")
	}
	if a, b := llmConfig(rc), llmConfig(rc); a.Headers["x-opencode-session"] != b.Headers["x-opencode-session"] {
		t.Errorf("llmConfig minted different session ids across calls: %q vs %q",
			a.Headers["x-opencode-session"], b.Headers["x-opencode-session"])
	}
}

// ---- llmConfig mapping ----

func TestLLMConfigMapping(t *testing.T) {
	c := llmConfig(reviewConfig{BaseURL: "https://h/v1/", Model: "m", APIKey: "k", StreamIdleTimeout: 7 * time.Second})
	if c.BaseURL != "https://h/v1" {
		t.Errorf("BaseURL = %q, want trailing slash trimmed", c.BaseURL)
	}
	if c.IdleTimeout != 7*time.Second {
		t.Errorf("IdleTimeout = %v, want 7s", c.IdleTimeout)
	}
	if c.MaxRetries != 0 {
		t.Errorf("MaxRetries = %d, want 0 (the review loop owns retries)", c.MaxRetries)
	}
	if c.Params.Temperature == nil || *c.Params.Temperature != reviewTemperature {
		t.Errorf("Temperature = %v, want %v", c.Params.Temperature, reviewTemperature)
	}
	if c.Params.Tool_choice != "auto" {
		t.Errorf("Tool_choice = %v, want auto", c.Params.Tool_choice)
	}
}

// TestLLMConfigIdleDefault: an unset idle window normalizes to llmkit's default.
func TestLLMConfigIdleDefault(t *testing.T) {
	c := llmConfig(reviewConfig{BaseURL: "http://x", Model: "m"})
	if c.IdleTimeout != llmkit.DefaultIdleTimeout {
		t.Errorf("IdleTimeout = %v, want llmkit default %v", c.IdleTimeout, llmkit.DefaultIdleTimeout)
	}
}

// TestAttemptTimeoutMapped pins the AI_REVIEW_ATTEMPT_TIMEOUT contract: the knob
// still parses and reaches llmkit.Config.Timeout (the SDK request timeout), so
// removing the private client changed the mechanism, not the contract.
func TestAttemptTimeoutMapped(t *testing.T) {
	t.Setenv("AI_REVIEW_ATTEMPT_TIMEOUT", "45")
	rc, _, err := parseReviewArgs(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rc.AttemptTimeout != 45*time.Second {
		t.Errorf("AttemptTimeout = %v, want 45s", rc.AttemptTimeout)
	}
	if got := llmConfig(rc).Timeout; got != 45*time.Second {
		t.Errorf("llmkit Timeout = %v, want 45s", got)
	}
}

// TestAttemptTimeoutDefault is separate so the env override above does not leak.
func TestAttemptTimeoutDefault(t *testing.T) {
	rc, _, err := parseReviewArgs(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rc.AttemptTimeout != defaultAttemptTimeout {
		t.Errorf("default AttemptTimeout = %v, want %v", rc.AttemptTimeout, defaultAttemptTimeout)
	}
}

// TestAttributionHeaders pins the OpenRouter attribution headers survive.
func TestAttributionHeaders(t *testing.T) {
	rc, _, _ := parseReviewArgs(nil, nil)
	h := llmConfig(rc).Headers
	if h["HTTP-Referer"] == "" || h["X-Title"] != "action-review" {
		t.Errorf("attribution headers missing: %v", h)
	}
}
