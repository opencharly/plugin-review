package pluginreview

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/opencharly/sdk/llmkit"
)

// review_test.go — the review gate's PURE-UNIT contract: knob parsing, the
// llmConfig mapping, the timeout-class predicate, the per-message truncation
// helper, and the prFromEventPath parser. Everything that exercises a SERVICE
// (the LLM, GitHub) lives in livetest_test.go and runs LIVE-or-SKIP; nothing
// here fabricates a service response.

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

// ---- the turn-retry policy ----

// ---- the inconclusive class ----

func TestTimeoutClassPositive(t *testing.T) {
	if !isTimeoutClass(context.DeadlineExceeded) {
		t.Error("context.DeadlineExceeded must classify as the timeout class")
	}
	if isTimeoutClass(context.Canceled) {
		t.Error("context.Canceled must NOT classify as the timeout class")
	}
	// llmkit's TYPED stall class must classify — via errors.As, not string matching
	// of the library's wording (which llmkit is free to change).
	if !isTimeoutClass(&llmkit.StallError{Idle: 3 * time.Minute, ContentBytes: 12}) {
		t.Error("llmkit's typed StallError must classify as the timeout class")
	}
	// A wrapped StallError must also classify (errors.As unwraps).
	if !isTimeoutClass(fmt.Errorf("turn failed: %w", &llmkit.StallError{Idle: time.Second})) {
		t.Error("a wrapped StallError must classify as the timeout class")
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

// ---- session affinity ----

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

// TestSessionIDPinned honors an explicit AI_REVIEW_SESSION_ID value verbatim
// (not just the empty-disable case).
func TestSessionIDPinned(t *testing.T) {
	t.Setenv("AI_REVIEW_SESSION_ID", "abc123")
	rc, _, err := parseReviewArgs(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rc.SessionID != "abc123" {
		t.Fatalf("SessionID = %q, want the pinned value abc123", rc.SessionID)
	}
	if got := llmConfig(rc).Headers["x-opencode-session"]; got != "abc123" {
		t.Errorf("header = %q, want abc123", got)
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

// TestGenerationBounds pins the RCA fix: a bounded reasoning effort and output
// token cap are set by default (the unbounded request is what produced the
// 1.75 MB / 786 s generation), and each is env-overridable.
func TestGenerationBounds(t *testing.T) {
	rc, _, err := parseReviewArgs(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The QUALITY default is high thinking; max_tokens is the SHARED
	// reasoning+answer budget and must be large enough that thinking cannot
	// consume it (measured: a 16384 cap produced an empty completion with
	// finish_reason=length). This pins both.
	if rc.ReasoningEffort != "high" {
		t.Errorf("ReasoningEffort default = %q, want high (the quality default)", rc.ReasoningEffort)
	}
	if rc.MaxTokens != defaultMaxTokens {
		t.Errorf("MaxTokens default = %d, want %d", rc.MaxTokens, defaultMaxTokens)
	}
	if defaultMaxTokens <= 65536 {
		t.Errorf("defaultMaxTokens = %d, must be comfortably above what high thinking consumes", defaultMaxTokens)
	}
	lc := llmConfig(rc)
	if lc.Params.Reasoning_effort != defaultReasoningEffort {
		t.Errorf("llmkit Reasoning_effort = %q, want %q", lc.Params.Reasoning_effort, defaultReasoningEffort)
	}
	if lc.Params.Max_tokens == nil || *lc.Params.Max_tokens != defaultMaxTokens {
		t.Errorf("llmkit Max_tokens = %v, want %d", lc.Params.Max_tokens, defaultMaxTokens)
	}
}

func TestGenerationBoundsOverride(t *testing.T) {
	t.Setenv("AI_REVIEW_REASONING_EFFORT", "none")
	t.Setenv("AI_REVIEW_MAX_TOKENS", "32000")
	rc, _, err := parseReviewArgs(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rc.ReasoningEffort != "none" || rc.MaxTokens != 32000 {
		t.Fatalf("overrides not applied: effort=%q max=%d", rc.ReasoningEffort, rc.MaxTokens)
	}
	lc := llmConfig(rc)
	if lc.Params.Reasoning_effort != "none" || lc.Params.Max_tokens == nil || *lc.Params.Max_tokens != 32000 {
		t.Errorf("overrides not mapped: %+v", lc.Params)
	}
}

// TestGenerationBoundsDisable: an explicit empty effort / zero tokens disables
// THAT bound (the operator accepts the unbounded behaviour).
func TestGenerationBoundsDisable(t *testing.T) {
	t.Setenv("AI_REVIEW_REASONING_EFFORT", "")
	t.Setenv("AI_REVIEW_MAX_TOKENS", "0")
	rc, _, err := parseReviewArgs(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rc.ReasoningEffort != "" {
		t.Errorf("empty effort must disable the bound, got %q", rc.ReasoningEffort)
	}
	if rc.MaxTokens != defaultMaxTokens {
		t.Errorf("invalid max tokens must keep the default, got %d", rc.MaxTokens)
	}
}

// TestTemperatureConfigurable pins that AI_REVIEW_TEMPERATURE overrides the
// default and reaches the wire params, and that an explicit 0 wins (0 is a
// legitimate deterministic setting, not "unset").
func TestTemperatureConfigurable(t *testing.T) {
	rc, _, err := parseReviewArgs(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if lc := llmConfig(rc); lc.Params.Temperature == nil || *lc.Params.Temperature != reviewTemperature {
		t.Fatalf("default temperature not applied: %v", lc.Params.Temperature)
	}
	t.Setenv("AI_REVIEW_TEMPERATURE", "0.7")
	rc, _, _ = parseReviewArgs(nil, nil)
	if lc := llmConfig(rc); lc.Params.Temperature == nil || *lc.Params.Temperature != 0.7 {
		t.Fatalf("AI_REVIEW_TEMPERATURE not applied: %v", lc.Params.Temperature)
	}
	t.Setenv("AI_REVIEW_TEMPERATURE", "0")
	rc, _, _ = parseReviewArgs(nil, nil)
	if lc := llmConfig(rc); lc.Params.Temperature == nil || *lc.Params.Temperature != 0 {
		t.Fatalf("explicit 0 must win, got %v", lc.Params.Temperature)
	}
}

// TestPRFromEventPathNoPullRequestDoesNotPanic: a workflow_dispatch event has no
// `pull_request` object. Dereferencing it SIGSEGV'd during the bootstrap
// self-test (measured live). A missing PR context returns 0, never a panic.
func TestPRFromEventPathNoPullRequestDoesNotPanic(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "event.json")
	if err := os.WriteFile(p, []byte(`{"action":"workflow_dispatch","inputs":{"pr-number":"1"},"repository":{"name":"x","owner":{"login":"o"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := prFromEventPath(p); got != 0 {
		t.Fatalf("workflow_dispatch event must yield 0, got %d", got)
	}
	// A pull_request event still yields its number.
	p2 := filepath.Join(dir, "pr.json")
	if err := os.WriteFile(p2, []byte(`{"pull_request":{"number":42,"head":{"sha":"a"},"base":{"sha":"b"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := prFromEventPath(p2); got != 42 {
		t.Fatalf("pull_request event must yield 42, got %d", got)
	}
}

// TestDeterministicClassIsTyped pins the fail-hard classification WITHOUT a
// service: an EmptyCompletionError is deterministic (not retryable), a plain
// error is not. The loop's "fail hard, do not blind-retry" branch keys on this.
func TestDeterministicClassIsTyped(t *testing.T) {
	det := &llmkit.EmptyCompletionError{ReasoningBytes: 10, FinishReason: "length"}
	if !isDeterministicClass(det) {
		t.Fatal("an EmptyCompletionError must classify as deterministic (non-retryable)")
	}
	if !isDeterministicClass(fmt.Errorf("wrapped: %w", det)) {
		t.Fatal("a wrapped EmptyCompletionError must still classify (errors.As unwraps)")
	}
	if isDeterministicClass(fmt.Errorf("transport blip")) {
		t.Fatal("a plain error must NOT be deterministic")
	}
	if !strings.Contains(det.Error(), "budget") {
		t.Fatalf("a length-truncated empty completion must name the budget, got: %v", det)
	}
}
