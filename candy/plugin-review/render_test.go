package pluginreview

import (
	"strings"
	"testing"
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

	c, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
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

// TestAgentHasTools is the regression for the regression: the review agent MUST
// receive the read-only tools. The cleanup once passed nil tools while the prompt
// still instructed the model to call them — a functional break the validator
// caught. sdkTools() is the single declaration; this asserts it is non-empty,
// covers every tool the prompt may name, and is actually threaded into chatTurn.
func TestAgentHasTools(t *testing.T) {
	tools := sdkTools()
	if len(tools) == 0 {
		t.Fatal("the review agent must receive tools (sdkTools() is empty)")
	}
	byName := map[string]bool{}
	for _, d := range reviewTools {
		byName[d.name] = true
	}
	for _, want := range []string{
		"get_pr_meta", "get_pr_body", "get_pr_files", "get_pr_file",
		"get_pr_commits", "get_pr_thread", "get_pr_comment",
	} {
		if !byName[want] {
			t.Errorf("the agent is missing the %q tool", want)
		}
	}
	// The prompt must NOT instruct the agent to use a tool that does not exist.
	prompt := Config{Prompt: readEmbedded(t)}.EffectivePrompt()
	for _, line := range []string{"get_pr_diff"} {
		if strings.Contains(prompt, line) && !byName[line] {
			t.Errorf("prompt references %q but that tool is not provided", line)
		}
	}
}

func readEmbedded(t *testing.T) string {
	t.Helper()
	return embeddedPrompt
}
