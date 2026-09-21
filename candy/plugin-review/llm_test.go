package pluginreview

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/opencharly/sdk/llmkit"
)

// llm_test.go — coverage for the provider mapping, session id, and the timeout
// classifier. These behaviours existed before the rewrite and MUST keep their
// coverage (R2/R3): the rewrite changed the shape, not the contract.

func TestLLMConfigMapping(t *testing.T) {
	temp := 0.9
	seed := int64(42)
	mt := int64(1234)
	cfg := Config{
		BaseURL: "http://x", Model: "m", APIKey: "k",
		StreamIdleTimeout: 90 * time.Second, AttemptTimeout: 12 * time.Minute,
		ReasoningEffort: "low", MaxTokens: mt, MaxCompletionTokens: &mt,
		Temperature: &temp, Seed: &seed, SessionID: "sess",
	}
	got := llmkitConfig(cfg)
	if got.BaseURL != "http://x" || got.Model != "m" || got.APIKey != "k" {
		t.Errorf("provider knobs not mapped: %+v", got)
	}
	if got.IdleTimeout != 90*time.Second {
		t.Errorf("IdleTimeout = %v, want 90s", got.IdleTimeout)
	}
	if got.Timeout != 12*time.Minute {
		t.Errorf("Timeout = %v, want 12m", got.Timeout)
	}
	if got.Params.Reasoning_effort != "low" {
		t.Errorf("Reasoning_effort = %q, want low", got.Params.Reasoning_effort)
	}
	if got.Params.Max_tokens == nil || *got.Params.Max_tokens != 1234 {
		t.Errorf("Max_tokens not mapped")
	}
	if got.Params.Max_completion_tokens == nil || *got.Params.Max_completion_tokens != 1234 {
		t.Errorf("Max_completion_tokens not mapped")
	}
	if got.Params.Temperature == nil || *got.Params.Temperature != 0.9 {
		t.Errorf("Temperature not mapped")
	}
	if got.Params.Seed == nil || *got.Params.Seed != 42 {
		t.Errorf("Seed not mapped")
	}
	if got.Headers["x-opencode-session"] != "sess" {
		t.Errorf("session header missing: %v", got.Headers)
	}
	if got.Headers["X-Title"] != "action-review" {
		t.Errorf("attribution header missing: %v", got.Headers)
	}
	if got.MaxRetries != 0 {
		t.Errorf("MaxRetries = %d, want 0 (the engine owns terminal classification)", got.MaxRetries)
	}
}

func TestTemperatureDefault(t *testing.T) {
	if got := temperatureOrDefault(nil); got == nil || *got != reviewTemperature {
		t.Errorf("default temperature = %v, want %v", got, reviewTemperature)
	}
	zero := 0.0
	if got := temperatureOrDefault(&zero); got == nil || *got != 0 {
		t.Errorf("explicit 0 must win over the default")
	}
}

func TestSessionIDPinnedAndDisabled(t *testing.T) {
	t.Setenv("AI_REVIEW_SESSION_ID", "pinned-id")
	if got := sessionID(); got != "pinned-id" {
		t.Errorf("sessionID() = %q, want the pinned value", got)
	}
	t.Setenv("AI_REVIEW_SESSION_ID", "")
	if got := sessionID(); got != "" {
		t.Errorf("an explicit empty must disable affinity, got %q", got)
	}
}

func TestSessionIDUniquelyMinted(t *testing.T) {
	// The var is UNSET (not empty) here, so the mint branch runs.
	os.Unsetenv("AI_REVIEW_SESSION_ID")
	a, b := sessionID(), sessionID()
	if a == "" || len(a) != 32 {
		t.Fatalf("minted id = %q, want 32 hex chars", a)
	}
	if a == b {
		t.Errorf("two mints collided: %q", a)
	}
}

func TestTimeoutClassifier(t *testing.T) {
	if !isTimeoutClass(context.DeadlineExceeded) {
		t.Error("a context deadline must classify as a timeout")
	}
	if !isTimeoutClass(&llmkit.StallError{Idle: time.Second}) {
		t.Error("a typed StallError must classify as a timeout")
	}
	if isTimeoutClass(errors.New("a transport blip")) {
		t.Error("an unrelated error must NOT classify as a timeout")
	}
	if !isWholeRequestDeadline(context.DeadlineExceeded) {
		t.Error("a context deadline is a whole-request cap")
	}
	if isWholeRequestDeadline(&llmkit.StallError{Idle: time.Second}) {
		t.Error("a stall is NOT the whole-request cap (only a deadline is)")
	}
}

func TestClassifyEmptyCompletionNamesTheKnob(t *testing.T) {
	err := classify(&llmkit.EmptyCompletionError{FinishReason: "length", ReasoningBytes: 1}, Config{MaxTokens: 5})
	if err == nil || !strings.Contains(err.Error(), "inconclusive") || !strings.Contains(err.Error(), "AI_REVIEW_MAX_TOKENS") {
		t.Fatalf("empty completion must be an actionable inconclusive class, got: %v", err)
	}
}

func TestParseCommandOverridesEnv(t *testing.T) {
	t.Setenv("PR_NUMBER", "1")
	t.Setenv("GITHUB_REPOSITORY", "env/repo")
	cfg, mode, err := parseCommand([]string{"pr", "7", "--repo", "cli/repo", "--out", "/tmp/x"})
	if err != nil {
		t.Fatal(err)
	}
	if mode != "review" {
		t.Errorf("mode = %q, want review", mode)
	}
	if cfg.PR != 7 || cfg.Repo != "cli/repo" || cfg.OutPath != "/tmp/x" {
		t.Errorf("flags did not override env: pr=%d repo=%q out=%q", cfg.PR, cfg.Repo, cfg.OutPath)
	}
	_, mode, _ = parseCommand([]string{"--self-test-verdict"})
	if mode != "self-test-verdict" {
		t.Errorf("mode = %q, want self-test-verdict", mode)
	}
}

func TestPRFromEventPathMissingFileIsZero(t *testing.T) {
	if got := prFromEventPath("/no/such/file"); got != 0 {
		t.Errorf("a missing event path must yield 0, got %d", got)
	}
	if got := prFromEventPath(""); got != 0 {
		t.Errorf("an empty event path must yield 0, got %d", got)
	}
}
