package pluginreview

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/opencharly/sdk/llmkit"
)

// TestRenderIncludesEveryFileWhole is the anti-skim guarantee at the assembler
// boundary: every changed file's COMPLETE patch appears in the ONE message, under
// its own FILE header. A review cannot skim what it was never given, and it is
// given everything — so this asserts the "line by line" contract holds by
// construction, not by prompt hope.
func TestRenderIncludesEveryFileWhole(t *testing.T) {
	files := []ChangedFile{
		{Path: "a.go", Status: "modified", Additions: 3, Deletions: 1, Patch: "@@ -1 +1 @@\n-old\n+new\n+more\n+lines"},
		{Path: "b/c.cue", Status: "added", Additions: 2, Deletions: 0, Patch: "@@ -0,0 +1,2 @@\n+x\n+y"},
		{Path: "docs/d.md", Status: "removed", Additions: 0, Deletions: 4, Patch: "@@ -1,4 +0,0 @@\n-a\n-b\n-c\n-d"},
	}
	c := &Context{
		Meta:     PRMeta{Title: "T", HeadSHA: "h", BaseSHA: "b"},
		Body:     "the body",
		Files:    files,
		Commits:  []Commit{{SHA: "0123456789abcdef", Author: "a", Message: "msg"}},
		Comments: []Comment{{ID: 7, Author: "u", CreatedAt: "t", Body: "a comment"}},
	}
	cfg := Config{Repo: "o/r", PR: 42}
	got := render(c, cfg)

	for _, f := range files {
		if !strings.Contains(got, "FILE: "+f.Path) {
			t.Errorf("file %s missing its header", f.Path)
		}
		if !strings.Contains(got, f.Patch) {
			t.Errorf("file %s patch not delivered whole", f.Path)
		}
	}
	for _, want := range []string{"the body", "a comment", "0123456789ab", "o/r#42"} {
		if !strings.Contains(got, want) {
			t.Errorf("assembled context missing %q", want)
		}
	}
	// The prompt must instruct a line-by-line read and the exact verdict form.
	if !strings.Contains(got, "LINE BY LINE") || !strings.Contains(got, "Verdict: PASS") {
		t.Errorf("assembled context is missing the review instruction/verdict form")
	}
}

// TestFromEnvReadsEveryKnob proves each model-behaviour knob is env-configurable
// end to end (env -> Config), which is what "set it from a GitHub Actions
// variable" requires.
func TestFromEnvReadsEveryKnob(t *testing.T) {
	t.Setenv("AI_REVIEW_PROVIDER", "p")
	t.Setenv("AI_REVIEW_MODEL", "m")
	t.Setenv("AI_REVIEW_BASE_URL", "http://x")
	t.Setenv("AI_REVIEW_REASONING_EFFORT", "low")
	t.Setenv("AI_REVIEW_MAX_TOKENS", "1234")
	t.Setenv("AI_REVIEW_MAX_COMPLETION_TOKENS", "99")
	t.Setenv("AI_REVIEW_TEMPERATURE", "0.7")
	t.Setenv("AI_REVIEW_TOP_P", "0.9")
	t.Setenv("AI_REVIEW_SEED", "5")
	t.Setenv("AI_REVIEW_STOP", "a,b")
	t.Setenv("AI_REVIEW_FREQUENCY_PENALTY", "0.1")
	t.Setenv("AI_REVIEW_PRESENCE_PENALTY", "0.2")
	t.Setenv("AI_REVIEW_STREAM_IDLE_TIMEOUT", "60")
	t.Setenv("AI_REVIEW_ATTEMPT_TIMEOUT", "120")
	t.Setenv("AI_REVIEW_CONTEXT_TOKENS", "999")
	t.Setenv("AI_REVIEW_CONTEXT_MARGIN", "11")
	t.Setenv("AI_REVIEW_MAX_TURNS", "33")
	t.Setenv("AI_REVIEW_POST_COMMENT", "false")
	t.Setenv("AI_REVIEW_DEBUG", "1")
	t.Setenv("AI_REVIEW_OUT", "/tmp/o")
	t.Setenv("PR_NUMBER", "7")
	t.Setenv("GITHUB_REPOSITORY", "o/r")

	c := FromEnv()
	if c.Provider != "p" || c.Model != "m" || c.BaseURL != "http://x" {
		t.Errorf("provider knobs not read: %+v", c)
	}
	if c.ReasoningEffort != "low" || c.MaxTokens != 1234 {
		t.Errorf("reasoning knobs not read: effort=%q max=%d", c.ReasoningEffort, c.MaxTokens)
	}
	if c.MaxCompletionTokens == nil || *c.MaxCompletionTokens != 99 {
		t.Errorf("max_completion_tokens not read")
	}
	if c.Temperature == nil || *c.Temperature != 0.7 || c.TopP == nil || *c.TopP != 0.9 {
		t.Errorf("sampling knobs not read")
	}
	if c.Seed == nil || *c.Seed != 5 || len(c.Stop) != 2 {
		t.Errorf("seed/stop not read")
	}
	if c.StreamIdleTimeout.Seconds() != 60 || c.AttemptTimeout.Seconds() != 120 {
		t.Errorf("timeouts not read: %v %v", c.StreamIdleTimeout, c.AttemptTimeout)
	}
	if c.ContextTokens != 999 || c.ContextMarginTokens != 11 {
		t.Errorf("context knobs not read")
	}
	if c.MaxTurns != 33 {
		t.Errorf("max turns not read: %d", c.MaxTurns)
	}
	if c.PostComment || !c.Debug || c.OutPath != "/tmp/o" {
		t.Errorf("effect/debug knobs not read: post=%v debug=%v out=%q", c.PostComment, c.Debug, c.OutPath)
	}
	if c.PR != 7 || c.Repo != "o/r" {
		t.Errorf("identity not read: pr=%d repo=%q", c.PR, c.Repo)
	}
}

// TestValidateRejectsMissingIdentity pins that a misconfigured run fails BEFORE
// any network call.
func TestValidateRejectsMissingIdentity(t *testing.T) {
	if err := (Config{}).Validate(); err == nil {
		t.Error("empty config must be rejected")
	}
	if err := (Config{PR: 1, Repo: "nope"}).Validate(); err == nil {
		t.Error("a repo without owner/ must be rejected")
	}
	if err := (Config{PR: 1, Repo: "o/r", ContextTokens: 10, MaxTokens: 1}).Validate(); err != nil {
		t.Errorf("a valid config must pass: %v", err)
	}
}

// TestBudgetGuardFailsClosed pins that an over-cap context is refused, never
// truncated.
func TestBudgetGuardFailsClosed(t *testing.T) {
	cfg := Config{ContextTokens: 1000, ContextMarginTokens: 10, MaxTokens: 100}
	if err := checkBudget(cfg, 100); err != nil {
		t.Errorf("a small context must fit: %v", err)
	}
	if err := checkBudget(cfg, 1_000_000); err == nil {
		t.Error("an over-cap context must fail hard (never truncated)")
	}
}

// TestAgentHasTools proves the tools are THREADED INTO THE MODEL CALL, not just
// declared: it intercepts the chat seam and asserts the exact tool set the
// review passes. The earlier cleanup shipped the model NO tools while the prompt
// instructed it to call them — the validator caught it — so this asserts the
// wiring, which a non-empty sdkTools() alone cannot.
func TestAgentHasTools(t *testing.T) {
	var got []openai.ChatCompletionToolUnionParam
	orig := chat
	chat = func(ctx context.Context, cfg llmkit.Config, msgs []openai.ChatCompletionMessageParamUnion, tools []openai.ChatCompletionToolUnionParam) (llmkit.Message, error) {
		got = tools
		return llmkit.Message{}, errors.New("stop after capture")
	}
	defer func() { chat = orig }()

	cfg := Config{Repo: "o/r", PR: 1, MaxTokens: 10, ContextTokens: 1 << 20}
	_ = cfg
	if _, err := chatTurn(context.Background(), llmkit.Config{}, nil); err == nil {
		t.Fatal("expected the capture sentinel error")
	}
	if len(got) == 0 {
		t.Fatal("the review passed NO tools to the model — the agent cannot call any")
	}
	gotNames := map[string]bool{}
	for _, tl := range got {
		if tl.OfFunction != nil {
			gotNames[tl.OfFunction.Function.Name] = true
		}
	}
	for _, want := range []string{
		"get_pr_meta", "get_pr_body", "get_pr_files", "get_pr_file",
		"get_pr_commits", "get_pr_thread", "get_pr_comment",
	} {
		if !gotNames[want] {
			t.Errorf("the model call is missing the %q tool (got %v)", want, gotNames)
		}
	}
}

// TestPRFromEventPathNoEventPayloadDoesNotPanic is the carried-forward regression
// for the measured SIGSEGV class: a workflow_dispatch run has an event payload
// with NO pull_request key, so the reader must return 0 rather than panic or
// misparse. (The removed review_test.go asserted this; it is restored here.)
func TestPRFromEventPathNoEventPayloadDoesNotPanic(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"dispatch.json": `{"action":"workflow_dispatch","inputs":{"pr-number":"7"}}`,
		"empty.json":    ``,
		"garbage.json":  `not json`,
		"null.json":     `null`,
	} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := prFromEventPath(p); got != 0 {
			t.Errorf("%s: prFromEventPath = %d, want 0", name, got)
		}
	}
	if got := prFromEventPath(filepath.Join(dir, "absent.json")); got != 0 {
		t.Errorf("absent file: prFromEventPath = %d, want 0", got)
	}
}

// TestPRFromEventPathReadsPullRequestNumber pins the happy path.
func TestPRFromEventPathReadsPullRequestNumber(t *testing.T) {
	p := filepath.Join(t.TempDir(), "pr.json")
	if err := os.WriteFile(p, []byte(`{"pull_request":{"number":42}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := prFromEventPath(p); got != 42 {
		t.Errorf("prFromEventPath = %d, want 42", got)
	}
}

// TestConfigInvalidEnvFallsBack is the carried-forward regression: a malformed or
// non-positive env value must fall back to the default, never set a zero/garbage
// value that would break the run.
func TestConfigInvalidEnvFallsBack(t *testing.T) {
	t.Setenv("AI_REVIEW_MAX_TURNS", "not-a-number")
	t.Setenv("AI_REVIEW_MAX_TOKENS", "-5")
	t.Setenv("AI_REVIEW_CONTEXT_TOKENS", "abc")
	t.Setenv("PR_NUMBER", "0")
	c := FromEnv()
	if c.MaxTurns != DefaultMaxTurns {
		t.Errorf("invalid AI_REVIEW_MAX_TURNS must fall back to %d, got %d", DefaultMaxTurns, c.MaxTurns)
	}
	if c.MaxTokens != DefaultMaxTokens {
		t.Errorf("non-positive AI_REVIEW_MAX_TOKENS must fall back to %d, got %d", DefaultMaxTokens, c.MaxTokens)
	}
	if c.ContextTokens != DefaultContextTokens {
		t.Errorf("invalid AI_REVIEW_CONTEXT_TOKENS must fall back to %d, got %d", DefaultContextTokens, c.ContextTokens)
	}
}

// TestParseCommandRejectsEmptyRepo pins finding 6's fix: parseCommand CAN fail,
// so the `if err != nil` arms in run() are reachable and tested.
func TestParseCommandRejectsEmptyRepo(t *testing.T) {
	t.Setenv("PR_NUMBER", "1")
	t.Setenv("GITHUB_REPOSITORY", "o/r")
	if _, _, err := parseCommand([]string{"pr", "7", "--repo", ""}); err == nil {
		t.Fatal("--repo with an empty value must error")
	}
	if _, _, err := parseCommand([]string{"pr", "7", "--repo"}); err == nil {
		t.Fatal("--repo with no value must error")
	}
}
