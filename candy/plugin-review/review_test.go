package pluginreview

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/opencharly/sdk/llmkit"
)

// review_test.go — the fail-closed verdict contract and the size guard. These are
// the engine's new control paths, so they carry their own coverage (the removed
// loop_test/e2e/livetest suites were the only ones that drove the engine).

// TestVerdictExitIsFailClosed pins the contract Run enforces: exit 0 ONLY for a
// single unambiguous verdict; a verdict-less review is exit 1 (the INCONCLUSIVE
// class the gate keeps RED); a mixed PASS+BLOCK is exit 2. An unreviewed PR must
// never read as a pass.
func TestVerdictExitIsFailClosed(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantExit int
		wantErr  bool
	}{
		{"single PASS", "## Review — PASS\n\nVerdict: PASS\n", 0, false},
		{"single BLOCK", "Verdict: BLOCK\n", 0, false},
		{"no verdict", "I could not decide.", 1, true},
		{"empty", "", 1, true},
		{"ambiguous", "Verdict: PASS\nVerdict: BLOCK\n", 2, true},
		{"prose mention is not a verdict (line-anchored)", "see Verdict: PASS above", 1, true},
	}
	for _, c := range cases {
		exit, err := verdictExit(c.body)
		if exit != c.wantExit {
			t.Errorf("%s: verdictExit exit=%d want %d", c.name, exit, c.wantExit)
		}
		if (err != nil) != c.wantErr {
			t.Errorf("%s: verdictExit err=%v wantErr=%v", c.name, err, c.wantErr)
		}
		if c.wantErr && err != nil && !strings.Contains(err.Error(), "inconclusive") {
			t.Errorf("%s: the error must be the inconclusive class, got %v", c.name, err)
		}
	}
}

// TestReviewValidatesBeforeNetwork pins that a misconfigured run fails before any
// network call (the required identity is checked up front).
func TestReviewValidatesBeforeNetwork(t *testing.T) {
	if _, err := Review(t.Context(), Config{PR: 0, Repo: ""}); err == nil {
		t.Fatal("an empty config must be rejected before any call")
	}
	if _, err := Review(t.Context(), Config{PR: 1, Repo: "no-slash"}); err == nil {
		t.Fatal("a repo without owner/ must be rejected before any call")
	}
}

// chatStub replaces the chat seam with a canned reply, so generate()'s full path
// (message build -> checkBudget -> the ONE call -> verdict) is driven
// deterministically without a network call.
func chatStub(t *testing.T, reply string, err error) *int {
	t.Helper()
	orig := chat
	calls := 0
	chat = func(ctx context.Context, cfg llmkit.Config, msgs []openai.ChatCompletionMessageParamUnion, tools []openai.ChatCompletionToolUnionParam) (llmkit.Message, error) {
		calls++
		if len(tools) != 0 {
			t.Errorf("the review must pass NO tools, got %d", len(tools))
		}
		if len(msgs) != 2 {
			t.Errorf("the review must send exactly system+user, got %d messages", len(msgs))
		}
		if err != nil {
			return llmkit.Message{}, err
		}
		return llmkit.Message{Content: &reply, FinishReason: "stop"}, nil
	}
	t.Cleanup(func() { chat = orig })
	return &calls
}

// TestGenerateOneCallReturnsVerdict drives the shipped engine end to end with a
// stubbed model: ONE call, no tools, the verdict returned.
func TestGenerateOneCallReturnsVerdict(t *testing.T) {
	calls := chatStub(t, "## Review — BLOCK\n\nVerdict: BLOCK\n", nil)
	cfg := Config{Repo: "o/r", PR: 1, MaxTokens: 10, ContextTokens: 1 << 20, ContextMarginTokens: 10}
	out, err := generate(context.Background(), cfg, &Context{Meta: PRMeta{Title: "t", HeadSHA: "h"}, Files: []ChangedFile{{Path: "a.go", Patch: "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	if *calls != 1 {
		t.Fatalf("the engine must make exactly ONE call, made %d", *calls)
	}
	if _, distinct, n := extractVerdict(out); n != 1 || distinct[0] != "BLOCK" {
		t.Fatalf("generate must return the verdict, got %q", out)
	}
}

// TestGeneratePropagatesModelFailure pins that a model failure is classified and
// returned (never swallowed).
func TestGeneratePropagatesModelFailure(t *testing.T) {
	chatStub(t, "", errors.New("boom"))
	cfg := Config{Repo: "o/r", PR: 1, MaxTokens: 10, ContextTokens: 1 << 20, ContextMarginTokens: 10}
	if _, err := generate(context.Background(), cfg, &Context{Meta: PRMeta{Title: "t"}}); err == nil {
		t.Fatal("a model failure must be returned")
	}
}
