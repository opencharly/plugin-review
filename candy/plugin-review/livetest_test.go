package pluginreview

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	ghkit "github.com/opencharly/plugin-gh/candy/plugin-gh/gh"
)

// livetest_test.go — the LIVE test harness.
//
// House rule (AGENTS.md): a test that exercises a SERVICE must run against the
// REAL service or SKIP. A hand-rolled mock server cannot reproduce the behavior
// under test — measured here: canned SSE always "succeeds" while the real
// reasoning model spun for 13 minutes and exhausted every token budget. So every
// service-dependent test below resolves a LIVE endpoint and SKIPS when it is
// unreachable; none uses a fabricated success response.
//
// Live endpoints are env-overridable so the same suite runs against the local
// ollama server (the default) or the org gateway:
//
//	AI_REVIEW_LIVE_URL    default http://localhost:11434/v1
//	AI_REVIEW_LIVE_KEY    default "ollama" (local ignores it)
//	AI_REVIEW_LIVE_MODEL  default deepseek-v4.1-flash:cloud
//	AI_REVIEW_LIVE_STALL_URL  optional: a provider that accepts then stalls
//	                          (WITHOUT it the stall/timeout tests SKIP — a live
//	                          model cannot be made to stall deterministically)

func liveEnv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// liveLLM resolves the live OpenAI-compatible endpoint and SKIPS when the
// service is unreachable. It never returns a fabricated endpoint.
func liveLLM(t *testing.T) (baseURL, apiKey, model string) {
	t.Helper()
	baseURL = liveEnv("AI_REVIEW_LIVE_URL", "http://localhost:11434/v1")
	apiKey = liveEnv("AI_REVIEW_LIVE_KEY", "ollama")
	model = liveEnv("AI_REVIEW_LIVE_MODEL", "deepseek-v4.1-flash:cloud")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/models", nil)
	if err != nil {
		t.Skipf("live LLM URL invalid: %v", err)
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Skipf("SKIP: live LLM unreachable at %s: %v", baseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		t.Skipf("SKIP: live LLM at %s returned HTTP %d", baseURL, resp.StatusCode)
	}
	return baseURL, apiKey, model
}

// liveReviewConfig builds a reviewConfig pointed at the live endpoint.
func liveReviewConfig(t *testing.T, maxTurns int) reviewConfig {
	t.Helper()
	baseURL, apiKey, model := liveLLM(t)
	return reviewConfig{
		Provider: "live", Model: model, BaseURL: baseURL, APIKey: apiKey,
		MaxTurns: maxTurns, MaxAttempts: 1,
		StreamIdleTimeout: 3 * time.Minute, AttemptTimeout: 10 * time.Minute,
		ToolResultMaxBytes: defaultToolResultMaxBytes, ReasoningEffort: defaultReasoningEffort,
		MaxTokens: defaultMaxTokens, ContextTokens: defaultContextTokens, ContextMarginTokens: defaultContextMarginTokens,
	}
}

// liveGitHubRepo returns a repo/pr the live GitHub client can read, skipping
// when no token is available or the API is unreachable. It uses the CANONICAL
// ghkit client (the same one the engine uses), so a pass proves the real read
// path.
func liveGitHubRepo(t *testing.T) (*ghkit.Client, string, int) {
	t.Helper()
	c, err := ghkit.New()
	if err != nil {
		t.Skipf("SKIP: no GitHub client: %v", err)
	}
	if c.Token == "" {
		t.Skip("SKIP: no GITHUB_TOKEN/GH_TOKEN and no gh auth — live GitHub test skipped")
	}
	repo := liveEnv("AI_REVIEW_LIVE_REPO", "opencharly/plugin-review")
	pr := 13
	if v := os.Getenv("AI_REVIEW_LIVE_PR"); v != "" {
		fmt.Sscanf(v, "%d", &pr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := c.PRMeta(ctx, repo, pr); err != nil {
		t.Skipf("SKIP: live GitHub read of %s#%d failed: %v", repo, pr, err)
	}
	return c, repo, pr
}

// recordProxy forwards every request to the LIVE endpoint and records it. It is
// observability OVER a live service (the response is the real model's), never a
// fabricated one — used where a test must observe the outgoing request (headers,
// turn count) which a live endpoint cannot be asked to report.
type recordedReq struct {
	Body   map[string]any
	Header http.Header
}

type recordProxy struct {
	*httptest.Server
	apiKey string
	mu     sync.Mutex
	reqs   []recordedReq
}

// URL is the proxy endpoint a reviewConfig should point at.
func (p *recordProxy) URL() string { return p.Server.URL }

func newRecordProxy(t *testing.T) *recordProxy {
	t.Helper()
	upstream, apiKey, _ := liveLLM(t)
	p := &recordProxy{}
	p.Server = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		p.mu.Lock()
		p.reqs = append(p.reqs, recordedReq{Body: body, Header: r.Header.Clone()})
		p.mu.Unlock()
		req, err := http.NewRequestWithContext(r.Context(), r.Method, strings.TrimRight(upstream, "/")+r.URL.Path, strings.NewReader(string(raw)))
		if err != nil {
			http.Error(rw, err.Error(), http.StatusBadGateway)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		if apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
		// stream the real response straight through
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			http.Error(rw, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, vs := range resp.Header {
			for _, v := range vs {
				rw.Header().Add(k, v)
			}
		}
		rw.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(rw, resp.Body)
	}))
	t.Cleanup(p.Close)
	return p
}

// requests returns a copy of the recorded requests (body + headers).
func (p *recordProxy) requests() []recordedReq {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]recordedReq, len(p.reqs))
	copy(out, p.reqs)
	return out
}

// liveStallEndpoint returns a stall endpoint URL or SKIPS. A live model cannot
// be made to stall deterministically, so these tests only run when an operator
// provides a controllable endpoint via AI_REVIEW_LIVE_STALL_URL.
func liveStallEndpoint(t *testing.T) string {
	t.Helper()
	u := os.Getenv("AI_REVIEW_LIVE_STALL_URL")
	if u == "" {
		t.Skip("SKIP: needs AI_REVIEW_LIVE_STALL_URL (a provider that accepts then stalls); a live model cannot be stalled deterministically")
	}
	return u
}

// ---- live service tests (live-or-skip; no fabricated responses) ----

// TestLivePassBudget drives AI_REVIEW_MAX_ATTEMPTS=1 against the REAL model: a
// completed pass with no verdict is not re-run, so the request count equals the
// pass count. Recorded through the proxy, which forwards to the live model.
func TestLivePassBudget(t *testing.T) {
	p := newRecordProxy(t)
	baseURL, apiKey, model := p.URL(), p.apiKey, liveEnv("AI_REVIEW_LIVE_MODEL", "deepseek-v4.1-flash:cloud")
	cfg := reviewConfig{Provider: "live", Model: model, BaseURL: baseURL, APIKey: apiKey,
		MaxTurns: 1, MaxAttempts: 1, StreamIdleTimeout: 3 * time.Minute,
		ReasoningEffort: defaultReasoningEffort, MaxTokens: defaultMaxTokens}
	_, err := runAgentLoop(context.Background(), cfg, "Reply with exactly: NOVERDICT (no Verdict line).", toolSet{})
	if err == nil {
		t.Fatal("expected an error from the single-pass loop (the model gave no verdict)")
	}
	if got := len(p.requests()); got != 1 {
		t.Errorf("AI_REVIEW_MAX_ATTEMPTS=1: got %d request(s), want exactly 1 (one pass)", got)
	}
}

// TestLiveSessionHeaderStable proves the review sends x-opencode-session and
// EVERY request of one run shares ONE value — against the real gateway (the
// gateway 400s MissingSessionID without it). Live-or-skip.
func TestLiveSessionHeaderStable(t *testing.T) {
	p := newRecordProxy(t)
	_, apiKey, model := liveLLM(t)
	cfg := reviewConfig{Provider: "opencode", Model: model, BaseURL: p.URL(), APIKey: apiKey,
		MaxTurns: 2, StreamIdleTimeout: 3 * time.Minute, ReasoningEffort: defaultReasoningEffort,
		MaxTokens: defaultMaxTokens, SessionID: newSessionID()}
	_, _ = runAgentLoop(context.Background(), cfg, "Reply with exactly: OK", toolSet{})
	reqs := p.requests()
	if len(reqs) == 0 {
		t.Fatal("no request reached the live endpoint")
	}
	first := reqs[0].Header.Get("x-opencode-session")
	if first == "" {
		t.Fatal("x-opencode-session must be sent (the gateway 400s without it)")
	}
	if len(first) != 32 {
		t.Fatalf("session id = %q, want 32 hex chars", first)
	}
	for i, r := range reqs {
		if got := r.Header.Get("x-opencode-session"); got != first {
			t.Fatalf("request %d sent session %q; every request of a run must share ONE session (%q)", i, got, first)
		}
	}
}

// TestLiveDeterministicFailureFailsHard drives the REAL model into the measured
// failure shape (a tiny max_tokens against a thinking model, so reasoning
// exhausts the budget and finish_reason=length yields an empty completion) and
// proves the engine FAILS HARD after ONE attempt instead of blind-retrying.
// Live-or-skip.
func TestLiveDeterministicFailureFailsHard(t *testing.T) {
	p := newRecordProxy(t)
	_, apiKey, model := liveLLM(t)
	// A tiny output budget against a thinking model deterministically truncates
	// the reasoning into an empty completion (finish_reason=length) — the exact
	// measured prod shape.
	cfg := reviewConfig{Provider: "live", Model: model, BaseURL: p.URL(), APIKey: apiKey,
		MaxTurns: 4, StreamIdleTimeout: 3 * time.Minute, ReasoningEffort: "high",
		MaxTokens: 256}
	_, err := runAgentLoop(context.Background(), cfg,
		"Think step by step for a very long time about every possible interpretation of this request, then answer.", toolSet{})
	// A live model does not deterministically reproduce the empty-completion
	// shape on demand, so when it does NOT, SKIP — the unit test pins the
	// fail-hard branch precisely (isDeterministicClass + one attempt); this test
	// only asserts the branch holds against a REAL deterministic failure.
	if err == nil || !isDeterministicClass(err) {
		t.Skipf("SKIP: the live model did not reproduce the deterministic empty-completion shape (%v)", err)
	}
	if got := len(p.requests()); got != 1 {
		t.Fatalf("the model was called %d times; a deterministic empty completion must be attempted ONCE, not blind-retried: %v", got, err)
	}
	if !strings.Contains(err.Error(), "AI_REVIEW_MAX_TOKENS") {
		t.Fatalf("the failure must name the knob that fixes it, got: %v", err)
	}
}

// TestLiveTimeoutClass drives a provider that accepts then stalls. A live model
// cannot be stalled deterministically, so this SKIPS unless the operator
// provides AI_REVIEW_LIVE_STALL_URL. Live-or-skip.
func TestLiveTimeoutClass(t *testing.T) {
	u := liveStallEndpoint(t)
	cfg := reviewConfig{Provider: "stall", Model: "m", BaseURL: u, APIKey: "x",
		MaxTurns: 1, StreamIdleTimeout: 2 * time.Second, ReasoningEffort: defaultReasoningEffort}
	_, err := runAgentLoop(context.Background(), cfg, "prompt", toolSet{})
	if err == nil {
		t.Fatal("expected an error from the stalled provider")
	}
	if !strings.Contains(err.Error(), "inconclusive") {
		t.Errorf("a stall must yield the INCONCLUSIVE marker, got: %v", err)
	}
}
