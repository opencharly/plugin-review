package pluginreview

import (
	"fmt"
	"os"
	"path/filepath"
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
		Commits:  []Commit{{SHA: "0123456789abcdef", Message: "msg"}},
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

// TestRenderMarksOmittedPatchExplicitly pins the R1 fix for
// opencharly/plugin-review#20: a file whose patch GitHub omitted must NEVER
// render as a silent empty ```diff``` block (which a reviewer reads as "no
// change"). The file reader recovers such patches from the raw .diff; when it
// cannot, render() must say so in-line.
func TestRenderMarksOmittedPatchExplicitly(t *testing.T) {
	c := &Context{
		Meta: PRMeta{Title: "T", HeadSHA: "h"},
		Files: []ChangedFile{
			{Path: "charly.yml", Status: "modified", Additions: 60, Deletions: 1697, Patch: "", NoPatch: true},
			// A pure rename/binary reports no hunks AND 0/0 — the marker must NOT
			// claim it changed lines.
			{Path: "renamed.bin", Status: "renamed", Additions: 0, Deletions: 0, Patch: "", NoPatch: true},
		},
	}
	got := render(c, Config{Repo: "o/r", PR: 1})
	if strings.Contains(got, "```diff\n\n```") {
		t.Fatalf("rendered an empty diff block for an omitted patch:\n%s", got)
	}
	if !strings.Contains(got, "charly.yml") || !strings.Contains(got, "patch omitted by the GitHub API") {
		t.Errorf("omitted-patch file not marked explicitly:\n%s", got)
	}
	if !strings.Contains(got, "renamed.bin") || !strings.Contains(got, "no textual patch") {
		t.Errorf("0/0 (rename/binary) file not marked with the non-changed wording:\n%s", got)
	}
	if strings.Contains(got, "renamed.bin (renamed, +0/-0)\n\n<!-- patch omitted by the GitHub API") {
		t.Errorf("0/0 file wrongly asserted to have changed lines:\n%s", got)
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
	t.Setenv("AI_REVIEW_MAX_TOKENS", "-5")
	t.Setenv("AI_REVIEW_CONTEXT_TOKENS", "abc")
	t.Setenv("PR_NUMBER", "0")
	c := FromEnv()
	if c.MaxTokens != DefaultMaxTokens {
		t.Errorf("non-positive AI_REVIEW_MAX_TOKENS must fall back to %d, got %d", DefaultMaxTokens, c.MaxTokens)
	}
	if c.ContextTokens != DefaultContextTokens {
		t.Errorf("invalid AI_REVIEW_CONTEXT_TOKENS must fall back to %d, got %d", DefaultContextTokens, c.ContextTokens)
	}
}

// TestOfficialSamplingDefaults pins the model vendor's official recommended
// sampling for deepseek-v4.1-flash and the ROOT FIX for the engine's
// degenerate-repetition collapse: temperature=1.0, top_p=0.95. At the old
// near-greedy 0.2 the model repeated one token ("Hmm.") tens of thousands of
// times and returned no answer; the official values yield a verdict (measured A/B
// on the same PR). Explicit overrides must win; the frequency/presence penalties
// default to the measured-best guard values and are likewise overridable.
func TestOfficialSamplingDefaults(t *testing.T) {
	os.Unsetenv("AI_REVIEW_TEMPERATURE")
	os.Unsetenv("AI_REVIEW_TOP_P")
	os.Unsetenv("AI_REVIEW_FREQUENCY_PENALTY")
	os.Unsetenv("AI_REVIEW_PRESENCE_PENALTY")
	c := FromEnv()
	if got := temperatureOrDefault(c.Temperature); got == nil || *got != 1.0 {
		t.Errorf("temperature default = %v, want 1.0 (the vendor's value)", got)
	}
	if c.TopP == nil || *c.TopP != 0.95 {
		t.Errorf("top_p default = %v, want 0.95 (the vendor's value)", c.TopP)
	}
	if c.FrequencyPenalty == nil || *c.FrequencyPenalty != defaultFrequencyPenalty {
		t.Errorf("frequency penalty default = %v, want %v (measured-best guard)", c.FrequencyPenalty, defaultFrequencyPenalty)
	}
	if c.PresencePenalty == nil || *c.PresencePenalty != defaultPresencePenalty {
		t.Errorf("presence penalty default = %v, want %v (measured-best guard)", c.PresencePenalty, defaultPresencePenalty)
	}
	t.Setenv("AI_REVIEW_TEMPERATURE", "0.3")
	t.Setenv("AI_REVIEW_TOP_P", "0.8")
	t.Setenv("AI_REVIEW_FREQUENCY_PENALTY", "0.4")
	t.Setenv("AI_REVIEW_PRESENCE_PENALTY", "0.6")
	c = FromEnv()
	if c.Temperature == nil || *c.Temperature != 0.3 || c.TopP == nil || *c.TopP != 0.8 {
		t.Errorf("sampling overrides not honoured: t=%v p=%v", c.Temperature, c.TopP)
	}
	if c.FrequencyPenalty == nil || *c.FrequencyPenalty != 0.4 || c.PresencePenalty == nil || *c.PresencePenalty != 0.6 {
		t.Errorf("penalty overrides not honoured: fp=%v pp=%v", c.FrequencyPenalty, c.PresencePenalty)
	}
}

// TestSamplingTrace pins the debug request trace: it must render the RESOLVED
// sampling (so a bare-environment run proves the defaults applied), and must
// print a pointer knob that is genuinely nil — only a bare Config{} leaves
// TopP/FrequencyPenalty/PresencePenalty nil; FromEnv defaults all three — as
// "nil" rather than the default, so "unset" is distinguishable from an explicit
// zero. Temperature always resolves, so it never prints nil. Fails without
// samplingTrace.
func TestSamplingTrace(t *testing.T) {
	t.Setenv("AI_REVIEW_PROVIDER", "p")
	t.Setenv("AI_REVIEW_MODEL", "m")
	t.Setenv("AI_REVIEW_BASE_URL", "http://x")
	os.Unsetenv("AI_REVIEW_TEMPERATURE")
	os.Unsetenv("AI_REVIEW_TOP_P")
	os.Unsetenv("AI_REVIEW_FREQUENCY_PENALTY")
	os.Unsetenv("AI_REVIEW_PRESENCE_PENALTY")
	got := samplingTrace(FromEnv())
	want := "sampling=(temperature=1 top_p=0.95 frequency_penalty=0.5 presence_penalty=1)"
	if got != want {
		t.Errorf("samplingTrace defaults = %q, want %q", got, want)
	}
	// An explicitly-nil penalty set (no defaults applied) must show "nil", and a
	// nil temperature must show the resolved default, not nil.
	bare := Config{}
	if s := samplingTrace(bare); !strings.Contains(s, "frequency_penalty=nil") {
		t.Errorf("samplingTrace(nil penalties) = %q, want frequency_penalty=nil", s)
	} else if !strings.Contains(s, "temperature=1") {
		t.Errorf("samplingTrace(nil temperature) = %q, want the resolved default temperature=1", s)
	}
}

// TestRenderCarriesAuthorshipAndEveryCommentKind pins the authorship contract: the
// review must know WHO opened the PR and WHO wrote EVERY comment (issue, review,
// inline) so an authorship or sign-off check is made from the context alone — and
// it must be told to weigh every comment by the same criteria as the body.
func TestRenderCarriesAuthorshipAndEveryCommentKind(t *testing.T) {
	c := &Context{
		Meta: PRMeta{Title: "T", Author: "atrawog", HeadSHA: "h"},
		Comments: []Comment{
			{ID: 1, Kind: "issue", Author: "aitrawog", CreatedAt: "t1", Body: "issue body"},
			{ID: 2, Kind: "review:APPROVED", Author: "atrawog", CreatedAt: "t2", Body: "review body"},
			{ID: 3, Kind: "review-comment", Author: "someone", CreatedAt: "t3", Body: "inline body"},
		},
	}
	got := render(c, Config{Repo: "o/r", PR: 9})
	for _, want := range []string{
		"Opened by: @atrawog",
		"Comment 1 [issue] by @aitrawog",
		"Comment 2 [review:APPROVED] by @atrawog",
		"Comment 3 [review-comment] by @someone",
		"issue body", "review body", "inline body",
		"Take EVERY comment above into consideration",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("assembled context missing %q", want)
		}
	}
}

// TestRenderThreadBoundKeepsNewestAndSummarisesOmission is the R7 test for the
// measured decode-collapse defect: on a large thread the assembled context is
// bounded and the thread can never dominate it. A thread far larger than the byte
// budget must (a) keep the NEWEST comments in FULL, (b) carry an explicit omission
// marker (never a silent drop), (c) leave the file patches — which the prompt
// reviews LINE BY LINE — untouched, and (d) keep the whole assembled comment
// section under the budget. It FAILS without the bound (the unbounded thread
// renders every 4 KB body).
func TestRenderThreadBoundKeepsNewestAndSummarisesOmission(t *testing.T) {
	// 40 comments of ~4 KB each ≈ 160 KB total, the measured thread size that
	// drove the collapse; the budget is the shipped 48 KiB.
	const n, bodyBytes = 40, 4096
	comments := make([]Comment, n)
	for i := range comments {
		comments[i] = Comment{
			ID: i + 1, Kind: "issue", Author: "u",
			CreatedAt: "t", Body: strings.Repeat("x", bodyBytes) + fmt.Sprintf(" body-%d", i+1),
		}
	}
	bigPatch := strings.Repeat("+ a changed line\n", 500)
	c := &Context{
		Meta:     PRMeta{Title: "T", HeadSHA: "h"},
		Body:     "the body",
		Files:    []ChangedFile{{Path: "a.go", Status: "modified", Additions: 500, Patch: bigPatch}},
		Commits:  []Commit{{SHA: "0123456789abcdef", Message: "msg"}},
		Comments: comments,
	}
	cfg := Config{Repo: "o/r", PR: 42, ThreadMaxBytes: DefaultThreadMaxBytes}
	got := render(c, cfg)

	// (c) every non-comment section is intact — the review must still see every line.
	if !strings.Contains(got, bigPatch) {
		t.Error("the bound dropped or truncated a file patch; only the comment thread may be bounded")
	}
	if !strings.Contains(got, "the body") || !strings.Contains(got, "0123456789ab") {
		t.Error("the bound dropped the body or a commit")
	}

	// (b) the omission is EXPLICIT, never silent.
	marker := fmt.Sprintf("older comments omitted (bodies available on the PR)")
	if !strings.Contains(got, marker) {
		t.Fatalf("no omission marker in the bounded thread; the drop was silent:\n%.400s", got)
	}

	// (a) the NEWEST comments are kept in full.
	if !strings.Contains(got, fmt.Sprintf("body-%d", n)) {
		t.Errorf("the newest comment (body-%d) was not kept", n)
	}
	if !strings.Contains(got, fmt.Sprintf("Comment %d [issue]", n)) {
		t.Errorf("the newest comment's header (Comment %d) was not kept", n)
	}

	// (d1) the assembled context is far under the unbounded size.
	unbounded := render(&Context{Meta: c.Meta, Body: c.Body, Files: c.Files, Commits: c.Commits, Comments: comments},
		Config{Repo: "o/r", PR: 42})
	if len(got) >= len(unbounded) {
		t.Fatalf("bounded render (%d B) is not smaller than the unbounded render (%d B)", len(got), len(unbounded))
	}
	// (d2) the whole bounded output stays well under the budget plus the
	// non-comment sections — the thread itself can never dominate.
	if int64(len(got)) > int64(DefaultThreadMaxBytes)+int64(len(bigPatch))+8192 {
		t.Errorf("bounded output %d B exceeds budget %d + patches %d + slack", len(got), DefaultThreadMaxBytes, len(bigPatch))
	}
	// (d3) the oldest comment is omitted (its body does not appear).
	if strings.Contains(got, "body-1x") {
		t.Errorf("the OLDEST comment survived the bound; the budget was not spent newest-first")
	}
}

// TestRenderThreadSmallUnchanged pins the other half of the contract: a thread
// that fits the budget renders EVERY comment exactly as before — the bound is not
// a silent truncation of small threads. It FAILS without the change too, because
// without the knob a Config that sets ThreadMaxBytes still renders all comments —
// but with the bound a small thread must be byte-identical to the unbounded render.
func TestRenderThreadSmallUnchanged(t *testing.T) {
	comments := []Comment{
		{ID: 1, Kind: "issue", Author: "u1", CreatedAt: "t1", Body: "small one"},
		{ID: 2, Kind: "review:APPROVED", Author: "u2", CreatedAt: "t2", Body: "small two"},
	}
	base := &Context{Meta: PRMeta{Title: "T", HeadSHA: "h"}, Body: "b", Comments: comments}
	bounded := render(base, Config{Repo: "o/r", PR: 3, ThreadMaxBytes: DefaultThreadMaxBytes})
	unbounded := render(base, Config{Repo: "o/r", PR: 3})
	if bounded != unbounded {
		t.Errorf("a thread under the budget must be unchanged:\n bounded=%.300s\nunbounded=%.300s", bounded, unbounded)
	}
	for _, want := range []string{"Comment 1 [issue]", "small one", "Comment 2 [review:APPROVED]", "small two"} {
		if !strings.Contains(bounded, want) {
			t.Errorf("small thread lost %q", want)
		}
	}
	if strings.Contains(bounded, "older comments omitted") {
		t.Error("small thread must not carry an omission marker")
	}
}

// TestThreadBoundDisabled pins the escape hatch: ThreadMaxBytes=0 renders the
// whole thread, so an operator can reproduce the unbounded behaviour for an RCA.
func TestThreadBoundDisabled(t *testing.T) {
	comments := make([]Comment, 30)
	for i := range comments {
		comments[i] = Comment{ID: i + 1, Kind: "issue", Author: "u", Body: strings.Repeat("y", 4096)}
	}
	c := &Context{Meta: PRMeta{Title: "T"}, Comments: comments}
	got := render(c, Config{Repo: "o/r", PR: 1, ThreadMaxBytes: 0})
	if strings.Contains(got, "older comments omitted") {
		t.Error("ThreadMaxBytes=0 must disable the bound (no omission)")
	}
	if !strings.Contains(got, "Comment 30 [issue]") || !strings.Contains(got, "Comment 1 [issue]") {
		t.Error("ThreadMaxBytes=0 must render every comment, oldest and newest")
	}
}

// TestFromEnvReadsThreadBound proves the bound is env-configurable end to end and
// that an explicit 0 is honoured (meaningful, unlike a count).
func TestFromEnvReadsThreadBound(t *testing.T) {
	t.Setenv("AI_REVIEW_CONTEXT_THREAD_MAX_BYTES", "2048")
	if got := FromEnv().ThreadMaxBytes; got != 2048 {
		t.Errorf("AI_REVIEW_CONTEXT_THREAD_MAX_BYTES = %d, want 2048", got)
	}
	os.Unsetenv("AI_REVIEW_CONTEXT_THREAD_MAX_BYTES")
	if got := FromEnv().ThreadMaxBytes; got != DefaultThreadMaxBytes {
		t.Errorf("default ThreadMaxBytes = %d, want %d", got, DefaultThreadMaxBytes)
	}
	t.Setenv("AI_REVIEW_CONTEXT_THREAD_MAX_BYTES", "0")
	if got := FromEnv().ThreadMaxBytes; got != 0 {
		t.Errorf("explicit 0 must disable the bound, got %d", got)
	}
}
