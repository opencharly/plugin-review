package pluginreview

// stream_test.go — the deterministic proof of the review client's new bounds:
// SSE parsing, the streaming happy path, the JSON fallback, the silence /
// time-to-header cuts, the explicit transport, the per-TURN (never per-loop)
// retry and the bounded tool-result payload.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
)

func writeSSEChunk(rw http.ResponseWriter, payload string) {
	fmt.Fprint(rw, "data: "+payload+"\n\n")
	flushSSE(rw)
}

func writeSSEDone(rw http.ResponseWriter) {
	fmt.Fprint(rw, "data: [DONE]\n\n")
	flushSSE(rw)
}

func flushSSE(rw http.ResponseWriter) {
	if f, ok := rw.(http.Flusher); ok {
		f.Flush()
	}
}

func testClient(t *testing.T, baseURL string, total, idle time.Duration) *llmClient {
	t.Helper()
	return newLLMClient(reviewConfig{Provider: "test", Model: "m", BaseURL: baseURL, APIKey: "k", AttemptTimeout: total, StreamIdleTimeout: idle})
}

// stallUntilClientGivesUp models a provider that stops streaming: it blocks
// until the client aborts the request (or the guard expires) and reports whether
// the disconnect was observed. It drains the request body FIRST — the Go server
// only starts its disconnect-detecting background read once the body hits EOF,
// so an unread body would leave this handler blocked long past the test.
func stallUntilClientGivesUp(r *http.Request) bool {
	_, _ = io.Copy(io.Discard, r.Body)
	select {
	case <-r.Context().Done():
		return true
	case <-time.After(20 * time.Second):
		return false
	}
}

func userMsg() []chatMsg {
	u := "review this PR"
	return []chatMsg{{Role: "user", Content: &u}}
}

// ---- the SSE parser / accumulator ----

func TestScanSSEAccumulatesContentAndToolCallDeltas(t *testing.T) {
	fixture := strings.Join([]string{
		": keep-alive comment (ignored)",
		"",
		`data: {"choices":[{"delta":{"content":"## Review"}}]}`,
		"",
		`data: {"choices":[{"delta":{"content":" — one"}}]}`,
		"",
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"get_pr_diff","arguments":"{\"a\""}}]}}]}`,
		"",
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"get_pr_meta","arguments":"{}"}}]}}]}`,
		"",
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":":1}"}}]},"finish_reason":"tool_calls"}]}`,
		"",
		"data: [DONE]",
		"",
		`data: {"choices":[{"delta":{"content":"AFTER-DONE"}}]}`,
		"",
	}, "\n")

	var acc streamAccumulator
	if err := scanSSE(strings.NewReader(fixture), acc.add); err != nil {
		t.Fatalf("scanSSE: %v", err)
	}
	msg, err := acc.message()
	if err != nil {
		t.Fatalf("message: %v", err)
	}
	if msg.Role != "assistant" {
		t.Errorf("role=%q want assistant", msg.Role)
	}
	if msg.Content == nil || *msg.Content != "## Review — one" {
		t.Fatalf("content=%v want the concatenated deltas", msg.Content)
	}
	if strings.Contains(*msg.Content, "AFTER-DONE") {
		t.Fatalf("chunks after [DONE] must be ignored: %q", *msg.Content)
	}
	if len(msg.ToolCalls) != 2 {
		t.Fatalf("tool calls=%d want 2", len(msg.ToolCalls))
	}
	if msg.ToolCalls[0].ID != "call_a" || msg.ToolCalls[0].Function.Name != "get_pr_diff" || msg.ToolCalls[0].Type != "function" {
		t.Fatalf("call 0 = %+v", msg.ToolCalls[0])
	}
	if msg.ToolCalls[0].Function.Arguments != `{"a":1}` {
		t.Fatalf("call 0 arguments not reassembled from fragments: %q", msg.ToolCalls[0].Function.Arguments)
	}
	if msg.ToolCalls[1].ID != "call_b" || msg.ToolCalls[1].Function.Name != "get_pr_meta" || msg.ToolCalls[1].Function.Arguments != "{}" {
		t.Fatalf("call 1 = %+v", msg.ToolCalls[1])
	}
	if acc.finish != "tool_calls" {
		t.Errorf("finish_reason=%q want tool_calls", acc.finish)
	}
}

func TestScanSSEFlushesAtEOFWithoutDone(t *testing.T) {
	var got []string
	err := scanSSE(strings.NewReader("data: {\"a\":1}\n\ndata: {\"b\":2}"), func(p string) error {
		got = append(got, p)
		return nil
	})
	if err != nil {
		t.Fatalf("scanSSE: %v", err)
	}
	if len(got) != 2 || got[0] != `{"a":1}` || got[1] != `{"b":2}` {
		t.Fatalf("payloads=%q want the trailing event flushed at EOF", got)
	}
}

func TestScanSSEMultiLineEvent(t *testing.T) {
	var got []string
	if err := scanSSE(strings.NewReader("data: line1\ndata: line2\n\n"), func(p string) error {
		got = append(got, p)
		return nil
	}); err != nil {
		t.Fatalf("scanSSE: %v", err)
	}
	if len(got) != 1 || got[0] != "line1\nline2" {
		t.Fatalf("payloads=%q want the multi-line event joined with \\n", got)
	}
}

func TestStreamAccumulatorRejectsNoChoices(t *testing.T) {
	var acc streamAccumulator
	if _, err := acc.message(); err == nil {
		t.Fatal("an empty stream must not produce a message")
	}
}

// ---- the live streaming path ----

func TestChatStreamsCompletion(t *testing.T) {
	var mu sync.Mutex
	sawStream := false
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		var cr chatRequest
		if err := json.NewDecoder(r.Body).Decode(&cr); err != nil {
			t.Errorf("decode chat request: %v", err)
		}
		mu.Lock()
		sawStream = cr.Stream
		mu.Unlock()
		rw.Header().Set("Content-Type", "text/event-stream")
		writeSSEChunk(rw, `{"choices":[{"delta":{"content":"hello "}}]}`)
		writeSSEChunk(rw, `{"choices":[{"delta":{"content":"world"}}]}`)
		writeSSEChunk(rw, `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"get_pr_meta","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
		writeSSEDone(rw)
	}))
	defer srv.Close()

	c := testClient(t, srv.URL, 10*time.Second, 2*time.Second)
	msg, err := c.chat(context.Background(), userMsg())
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if msg.Content == nil || *msg.Content != "hello world" {
		t.Fatalf("content=%v", msg.Content)
	}
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].ID != "c1" {
		t.Fatalf("tool calls=%+v", msg.ToolCalls)
	}
	mu.Lock()
	defer mu.Unlock()
	if !sawStream {
		t.Error("the request must ask for stream:true — the whole-RCA fix depends on it")
	}
}

func TestChatAcceptsNonStreamingJSONReply(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		// A provider/gateway that ignores stream:true answers with whole JSON.
		rw.Header().Set("Content-Type", "application/json")
		fmt.Fprint(rw, `{"choices":[{"message":{"content":"Verdict: PASS"}}]}`)
	}))
	defer srv.Close()

	c := testClient(t, srv.URL, 10*time.Second, 2*time.Second)
	msg, err := c.chat(context.Background(), userMsg())
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if msg.Content == nil || *msg.Content != "Verdict: PASS" {
		t.Fatalf("fallback content=%v", msg.Content)
	}
}

// ---- the bounds ----

func TestChatCutsAnIdleStreamAtTheIdleBound(t *testing.T) {
	aborted := make(chan bool, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "text/event-stream")
		writeSSEChunk(rw, `{"choices":[{"delta":{"content":"partial"}}]}`)
		aborted <- stallUntilClientGivesUp(r) // the provider stalls: nothing more ever arrives
	}))
	defer srv.Close()

	const idle = 250 * time.Millisecond
	c := testClient(t, srv.URL, 30*time.Second, idle)
	begin := time.Now()
	_, err := c.chat(context.Background(), userMsg())
	elapsed := time.Since(begin)
	if err == nil {
		t.Fatal("a stalled stream must fail, not return a partial message")
	}
	if !isTimeoutClass(err) {
		t.Fatalf("a stalled stream must classify as the provider-unanswered class, got: %v", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("client hung %v past the %v idle bound: %v", elapsed, idle, err)
	}
	select {
	case ok := <-aborted:
		if !ok {
			t.Error("the server never observed the client aborting the stalled stream — the connection was not released")
		}
	case <-time.After(25 * time.Second):
		t.Error("the stalled handler never observed the client giving up")
	}
}

func TestChatBoundsTimeToFirstByte(t *testing.T) {
	aborted := make(chan bool, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		aborted <- stallUntilClientGivesUp(r) // a blackholed peer: never writes response headers
	}))
	defer srv.Close()

	const total = 300 * time.Millisecond
	c := testClient(t, srv.URL, total, time.Minute)
	begin := time.Now()
	_, err := c.chat(context.Background(), userMsg())
	elapsed := time.Since(begin)
	if err == nil {
		t.Fatal("a peer that never writes headers must fail")
	}
	if !isTimeoutClass(err) {
		t.Fatalf("a dead peer must classify as the provider-unanswered class, got: %v", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("client hung %v past the %v bound: %v", elapsed, total, err)
	}
	select {
	case ok := <-aborted:
		if !ok {
			t.Error("the server never observed the client giving up on the headerless response")
		}
	case <-time.After(25 * time.Second):
		t.Error("the dead-peer handler never observed the client giving up")
	}
}

// ---- the transport ----

func TestHTTPClientTransportWiring(t *testing.T) {
	const total = 15 * time.Minute
	c := newHTTPClient(total)
	if c.Timeout != 0 {
		t.Errorf("client-wide Timeout=%v: the bound must be per-request so a streamed generation is not cut", c.Timeout)
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport must be an explicit *http.Transport, got %T", c.Transport)
	}
	if !tr.DisableKeepAlives {
		t.Error("DisableKeepAlives must be set: a reused idle POST after a long tool gap can hang to the deadline")
	}
	if tr.IdleConnTimeout <= 0 || tr.IdleConnTimeout > time.Minute {
		t.Errorf("IdleConnTimeout=%v want a short, explicit bound", tr.IdleConnTimeout)
	}
	if tr.ResponseHeaderTimeout != total {
		t.Errorf("ResponseHeaderTimeout=%v want %v (time-to-first-byte bound)", tr.ResponseHeaderTimeout, total)
	}
	if tr.DialContext == nil || tr.TLSHandshakeTimeout <= 0 {
		t.Error("connection establishment must be bounded explicitly")
	}
	if !tr.ForceAttemptHTTP2 {
		t.Error("ForceAttemptHTTP2 must be set on an explicit transport")
	}
}

func TestChatDialsFreshConnectionPerRequest(t *testing.T) {
	var mu sync.Mutex
	var addrs []string
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		mu.Lock()
		addrs = append(addrs, r.RemoteAddr)
		mu.Unlock()
		rw.Header().Set("Content-Type", "text/event-stream")
		writeSSEChunk(rw, `{"choices":[{"delta":{"content":"ok"}}]}`)
		writeSSEDone(rw)
	}))
	defer srv.Close()

	c := testClient(t, srv.URL, 10*time.Second, 2*time.Second)
	for i := 0; i < 2; i++ {
		if _, err := c.chat(context.Background(), userMsg()); err != nil {
			t.Fatalf("chat %d: %v", i, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(addrs) != 2 {
		t.Fatalf("requests=%d want 2", len(addrs))
	}
	if addrs[0] == addrs[1] {
		t.Fatalf("the second POST reused the connection (%s): the transport must dial fresh per request", addrs[0])
	}
}

// ---- the retry policy: the TURN, not the loop ----

func TestFailedTurnIsRetriedWithoutRestartingTheLoop(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "fixtures"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fixtures", "fx-get_pr_meta.json"), []byte(`{"title":"t"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	var mu sync.Mutex
	var reqs []chatRequest
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		var cr chatRequest
		_ = json.NewDecoder(r.Body).Decode(&cr)
		mu.Lock()
		reqs = append(reqs, cr)
		mu.Unlock()
		switch atomic.AddInt32(&n, 1) {
		case 1: // turn 1: one tool call
			rw.Header().Set("Content-Type", "text/event-stream")
			writeSSEChunk(rw, `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"get_pr_meta","arguments":"{}"}}]}}]}`)
			writeSSEDone(rw)
		case 2, 3: // turn 2, attempts 1 and 2: the stream stalls
			rw.Header().Set("Content-Type", "text/event-stream")
			rw.WriteHeader(http.StatusOK)
			flushSSE(rw)
			stallUntilClientGivesUp(r)
		default: // turn 2, attempt 3: the verdict
			rw.Header().Set("Content-Type", "text/event-stream")
			writeSSEChunk(rw, `{"choices":[{"delta":{"content":"## Review\n\nVerdict: PASS\n"}}]}`)
			writeSSEDone(rw)
		}
	}))
	defer srv.Close()

	cfg := reviewConfig{
		Provider: "test", Model: "m", BaseURL: srv.URL, APIKey: "k", MaxTurns: 5,
		AttemptTimeout: 20 * time.Second, StreamIdleTimeout: 150 * time.Millisecond, RetryBackoff: time.Millisecond,
	}
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
		t.Fatalf("requests=%d want 4 (turn 1 once + three turn-2 attempts) — a loop restart shows up as extra turn-1 requests", len(reqs))
	}
	for i, m := range reqs[0].Messages {
		if m.Role == "tool" {
			t.Fatalf("first request (turn 1) must carry no tool result, message %d did", i)
		}
	}
	turn2 := 0
	for _, r := range reqs {
		for _, m := range r.Messages {
			if m.Role == "tool" {
				turn2++
				break
			}
		}
	}
	if turn2 != 3 {
		t.Fatalf("turn-2 requests carrying the tool result=%d want 3 — the retry must resume the FAILED TURN", turn2)
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
		t.Fatalf("the head must be preserved verbatim")
	}
	if len(got) > cap+200 {
		t.Fatalf("truncated payload=%d bytes, want ~%d", len(got), cap)
	}
	if !strings.Contains(got, "truncated by the review harness") || !strings.Contains(got, "INCOMPLETE") {
		t.Fatalf("the cut must be announced clearly: %q", got[len(got)-120:])
	}
	// a cut inside a multi-byte rune must not emit invalid UTF-8
	multi := truncateToolResult(strings.Repeat("é", 4096), 101)
	if !utf8.ValidString(multi) {
		t.Fatalf("truncation produced invalid UTF-8: %q", multi)
	}
	if multi[len(multi)-1] == 0xc3 {
		t.Fatalf("a partial rune was kept: %q", multi)
	}
	// max<=0 falls back to the default budget
	if !strings.Contains(truncateToolResult(strings.Repeat("b", defaultToolResultMaxBytes+1), 0), "truncated by the review harness") {
		t.Fatal("a zero cap must fall back to the default budget")
	}
}

func TestTruncateStrKeepsItsFullByteBudgetOnQuotedPayload(t *testing.T) {
	in := "diff --git a/x.go b/x.go\n" + strings.Repeat("+\tfmt.Println(\"hi\")\n", 500)
	const budget = 2048
	got := truncateStr(in, budget)
	if len(got) < budget {
		t.Fatalf("truncateStr returned %d bytes, below the %d budget: the rune-boundary check ate a quoted payload", len(got), budget)
	}
	if len(got) > budget+len("\n[…truncated…]") {
		t.Fatalf("truncateStr exceeded the budget: %d bytes", len(got))
	}
}

func TestToolResultPayloadIsBoundedInTheConversation(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "fixtures"), 0o755); err != nil {
		t.Fatal(err)
	}
	// a diff far larger than the cap
	if err := os.WriteFile(filepath.Join(dir, "fixtures", "fx-get_pr_diff.json"), []byte(strings.Repeat("x", 300*1024)), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	const cap = 4096
	var mu sync.Mutex
	var second []chatMsg
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		var cr chatRequest
		_ = json.NewDecoder(r.Body).Decode(&cr)
		rw.Header().Set("Content-Type", "text/event-stream")
		if atomic.AddInt32(&n, 1) == 1 {
			writeSSEChunk(rw, `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"get_pr_diff","arguments":"{}"}}]}}]}`)
			writeSSEDone(rw)
			return
		}
		mu.Lock()
		second = cr.Messages
		mu.Unlock()
		writeSSEChunk(rw, `{"choices":[{"delta":{"content":"Verdict: PASS\n"}}]}`)
		writeSSEDone(rw)
	}))
	defer srv.Close()

	cfg := reviewConfig{
		Provider: "test", Model: "m", BaseURL: srv.URL, APIKey: "k", MaxTurns: 4,
		AttemptTimeout: 10 * time.Second, StreamIdleTimeout: 2 * time.Second, RetryBackoff: time.Millisecond,
		ToolResultMaxBytes: cap,
	}
	if _, err := runAgentLoop(context.Background(), cfg, "prompt", toolSet{fixture: "fx"}); err != nil {
		t.Fatalf("runAgentLoop: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	var toolContent string
	for _, m := range second {
		if m.Role == "tool" && m.Content != nil {
			toolContent = *m.Content
		}
	}
	if toolContent == "" {
		t.Fatalf("the turn-2 request must carry the tool result")
	}
	if len(toolContent) > cap+200 {
		t.Fatalf("the tool result handed to the model is %d bytes, want ~%d — unbounded context growth is the RCA trigger", len(toolContent), cap)
	}
	if !strings.Contains(toolContent, "truncated by the review harness") {
		t.Fatalf("the truncation marker is missing from the tool message")
	}
}

// ---- the knobs ----

func TestStreamTuningKnobs(t *testing.T) {
	base, _, err := parseReviewArgs(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if base.AttemptTimeout != 15*time.Minute || base.StreamIdleTimeout != 3*time.Minute || base.ToolResultMaxBytes != defaultToolResultMaxBytes {
		t.Fatalf("defaults = %v/%v/%d", base.AttemptTimeout, base.StreamIdleTimeout, base.ToolResultMaxBytes)
	}

	t.Setenv("AI_REVIEW_STREAM_IDLE_TIMEOUT", "45")
	t.Setenv("AI_REVIEW_TOOL_RESULT_MAX_BYTES", "1234")
	cfg, _, err := parseReviewArgs(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StreamIdleTimeout != 45*time.Second {
		t.Errorf("StreamIdleTimeout: got %v, want 45s", cfg.StreamIdleTimeout)
	}
	if cfg.ToolResultMaxBytes != 1234 {
		t.Errorf("ToolResultMaxBytes: got %d, want 1234", cfg.ToolResultMaxBytes)
	}

	t.Setenv("AI_REVIEW_STREAM_IDLE_TIMEOUT", "bogus")
	t.Setenv("AI_REVIEW_TOOL_RESULT_MAX_BYTES", "0")
	cfg2, _, err := parseReviewArgs(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.StreamIdleTimeout != defaultStreamIdleTimeout || cfg2.ToolResultMaxBytes != defaultToolResultMaxBytes {
		t.Errorf("invalid values must fall back to the defaults: %v/%d", cfg2.StreamIdleTimeout, cfg2.ToolResultMaxBytes)
	}
}
