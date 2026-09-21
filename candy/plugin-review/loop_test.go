package pluginreview

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/opencharly/sdk/llmkit"
)

// stubChat replaces the chat seam with a scripted sequence of turns, so the
// prime + bounded tool loop in generate() is exercised deterministically: one
// tool-calling turn, then a final verdict turn. This is the coverage the removed
// livetest/e2e suites had, restored as a unit test.
func stubChat(t *testing.T, turns []llmkit.Message) {
	t.Helper()
	orig := chat
	i := 0
	chat = func(ctx context.Context, cfg llmkit.Config, msgs []openai.ChatCompletionMessageParamUnion, tools []openai.ChatCompletionToolUnionParam) (llmkit.Message, error) {
		if len(tools) == 0 {
			t.Error("the model call must be given tools")
		}
		if i >= len(turns) {
			return llmkit.Message{}, errors.New("stubChat: no more scripted turns")
		}
		m := turns[i]
		i++
		return m, nil
	}
	t.Cleanup(func() { chat = orig })
}

func strp(s string) *string { return &s }

// TestGeneratePrimeThenToolThenVerdict drives the loop: turn 1 asks for a tool,
// turn 2 answers with a verdict. It asserts the tool was dispatched and the
// verdict is returned.
func TestGeneratePrimeThenToolThenVerdict(t *testing.T) {
	stubChat(t, []llmkit.Message{
		{Role: "assistant", ToolCalls: []llmkit.ToolCall{{ID: "1", Name: "get_pr_meta", Arguments: "{}"}}},
		{Role: "assistant", Content: strp("## Review — BLOCK\n\nVerdict: BLOCK\n")},
	})
	cfg := Config{Repo: "o/r", PR: 1, MaxTurns: 5, MaxTokens: 10, ContextTokens: 1 << 20, ContextMarginTokens: 10}
	c := &Context{Meta: PRMeta{Title: "t", HeadSHA: "h"}, Files: []ChangedFile{{Path: "a.go", Patch: "x"}}}
	out, err := generate(context.Background(), cfg, c)
	if err != nil {
		t.Fatal(err)
	}
	_, distinct, n := extractVerdict(out)
	if n != 1 || distinct[0] != "BLOCK" {
		t.Fatalf("generate must return the verdict, got %q", out)
	}
}

// TestGenerateExhaustsTurnBudgetReturnsText pins the documented tail: when the
// budget runs out with no verdict, generate returns the text (and Run makes it a
// non-zero, INCONCLUSIVE exit).
func TestGenerateExhaustsTurnBudgetReturnsText(t *testing.T) {
	stubChat(t, []llmkit.Message{
		{Role: "assistant", ToolCalls: []llmkit.ToolCall{{ID: "1", Name: "get_pr_meta", Arguments: "{}"}}},
		{Role: "assistant", ToolCalls: []llmkit.ToolCall{{ID: "2", Name: "get_pr_meta", Arguments: "{}"}}},
	})
	cfg := Config{Repo: "o/r", PR: 1, MaxTurns: 2, MaxTokens: 10, ContextTokens: 1 << 20, ContextMarginTokens: 10}
	out, err := generate(context.Background(), cfg, &Context{Meta: PRMeta{Title: "t"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, n := extractVerdict(out); n != 0 {
		t.Fatalf("a budget-exhausted run must have no verdict, got %q", out)
	}
}

// TestRunFailClosedOnNoVerdict is the critical contract: a verdict-less review is
// a non-zero exit (INCONCLUSIVE — the gate keeps RED), NEVER 0. A missing verdict
// must not read as a pass. verdictExit is the ONE place Run enforces it.
func TestRunFailClosedOnNoVerdict(t *testing.T) {
	exit, err := verdictExit("I could not decide.")
	if exit == 0 {
		t.Fatal("a verdict-less run MUST NOT exit 0 (an unreviewed PR must never pass)")
	}
	if err == nil || !strings.Contains(err.Error(), "inconclusive") {
		t.Fatalf("expected an inconclusive error, got exit=%d err=%v", exit, err)
	}
	exit, err = verdictExit("## Review — PASS\n\nVerdict: PASS\n")
	if exit != 0 || err != nil {
		t.Fatalf("a single verdict must be exit 0, got exit=%d err=%v", exit, err)
	}
	exit, err = verdictExit("Verdict: PASS\nVerdict: BLOCK\n")
	if exit == 0 || err == nil {
		t.Fatalf("an ambiguous verdict must be non-zero, got exit=%d err=%v", exit, err)
	}
}

// TestLowerEffort pins the bounded-reasoning fallback ladder: a shared-budget
// failure retries one step down, so the answer can fit.
func TestLowerEffort(t *testing.T) {
	cases := map[string]string{"max": "high", "high": "medium", "medium": "low", "low": "none", "": "none"}
	for in, want := range cases {
		if got := lowerEffort(in); got != want {
			t.Errorf("lowerEffort(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestGenerateRetriesOnceAtLowerEffortOnBudgetExhaustion drives the fallback: the
// first turn returns an EmptyCompletionError (budget exhausted), the retry (and
// the following turn) succeed. It asserts the retry happened.
func TestGenerateRetriesOnceAtLowerEffortOnBudgetExhaustion(t *testing.T) {
	orig := chat
	calls := 0
	chat = func(ctx context.Context, cfg llmkit.Config, msgs []openai.ChatCompletionMessageParamUnion, tools []openai.ChatCompletionToolUnionParam) (llmkit.Message, error) {
		calls++
		if calls == 1 {
			if cfg.Params.Reasoning_effort != "high" {
				t.Errorf("first call effort = %q, want high", cfg.Params.Reasoning_effort)
			}
			return llmkit.Message{}, &llmkit.EmptyCompletionError{FinishReason: "length", ReasoningBytes: 10}
		}
		if cfg.Params.Reasoning_effort != "medium" {
			t.Errorf("retry effort = %q, want medium (one step down)", cfg.Params.Reasoning_effort)
		}
		return llmkit.Message{Content: strp("Verdict: BLOCK\n")}, nil
	}
	t.Cleanup(func() { chat = orig })

	cfg := Config{Repo: "o/r", PR: 1, MaxTurns: 3, MaxTokens: 10, ContextTokens: 1 << 20, ContextMarginTokens: 10, ReasoningEffort: "high"}
	out, err := generate(context.Background(), cfg, &Context{Meta: PRMeta{Title: "t"}})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("expected ONE step down (2 calls), got %d", calls)
	}
	if _, _, n := extractVerdict(out); n != 1 {
		t.Fatalf("the retry must yield a verdict, got %q", out)
	}
}
