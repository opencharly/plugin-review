package pluginreview

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/opencharly/sdk/llmkit"
)

// review.go — the review engine. ONE path, from first principles:
//
//	assemble the COMPLETE PR context  →  ONE LLM call  →  extract the verdict  →  emit effects.
//
// There is no tool loop, no plan executor, no fixture branch and no pass retry.
// Each of those was accidental complexity that produced the runaway thinking
// (fragmented context + discarded reasoning) or the confusing "what is actually
// running" problem (a --plan path that wrapped a single review step; env read in
// several places). This file is the whole engine:
//
//   - Review(ctx, cfg) runs assemble -> generate -> verdict.
//   - generate makes the SINGLE model call (with the shared llmkit client).
//   - The context guard fails HARD if the assembled input + output reserve cannot
//     fit the window, so a review never silently truncates.
//   - A generation failure is classified (deterministic / whole-request cap /
//     idle stall) and NEVER retried: re-issuing the same request is the measured
//     amplifier of long runs.

// Review is the pure engine: it assembles the context, makes ONE model call, and
// returns the review text. It has NO side effects (no comment, no file write) —
// emitting those is Emit's single job, so the command path and any embedder share
// ONE engine and fire each effect exactly once.
func Review(ctx context.Context, cfg Config) (string, error) {
	if err := cfg.Validate(); err != nil {
		return "", err
	}
	gh := newGHClient()
	c, err := assemble(ctx, cfg, gh)
	if err != nil {
		return "", err
	}
	return generate(ctx, cfg, c)
}

// generate runs the review: it PRIMES the conversation with the complete PR
// context (ONE message), then runs a bounded tool loop in which the read-only
// tools remain available for verification/follow-up.
//
// Why prime AND tools (RCA): the old engine assembled the context turn by turn
// through the tools, and message reasoning is NOT re-sent between turns, so the
// synthesis turn re-derived everything from partial tool results — measured at
// 371 KB of reasoning over three runaway synthesis turns (8m25s) on a 25-file
// PR. Delivering the whole context in ONE priming message collapsed that to
// 50 KB with a verdict (82.6s). Keeping the tools means the agent can still
// verify a count or re-read a fact; because it already HAS the full context it
// has no need to reconstruct it, so the loop converges fast.
func generate(ctx context.Context, cfg Config, c *Context) (string, error) {
	prompt := cfg.EffectivePrompt()
	user := c.Assembled

	if err := checkBudget(cfg, len(prompt)+len(user)); err != nil {
		return "", err
	}
	llm := llmkitConfig(cfg)
	gh := newGHClient()
	messages := []llmkit.Message{
		{Role: "system", Content: llmkit.Strptr(prompt)},
		{Role: "user", Content: llmkit.Strptr(user)},
	}
	loopStart := time.Now()
	dbg(cfg, "loop start — turns_max=%d reasoning_effort=%q max_tokens=%d attempt_timeout=%v idle=%v primed=%v context_bytes=(system=%d user=%d) files=%d comments=%d",
		cfg.MaxTurns, cfg.ReasoningEffort, cfg.MaxTokens, cfg.AttemptTimeout, cfg.StreamIdleTimeout, true,
		len(prompt), len(user), len(c.Files), len(c.Comments))

	for turn := 0; turn < cfg.MaxTurns; turn++ {
		contextBytes := conversationBytes(messages)
		start := time.Now()
		msg, err := chatTurn(ctx, cfg, llm, messages)
		elapsed := time.Since(start)
		if err != nil {
			dbg(cfg, "turn %d FAILED after %v (context=%d bytes): %v", turn+1, elapsed, contextBytes, err)
			debugFailedReasoning(cfg, err)
			return "", classify(err, cfg)
		}
		content := ""
		if msg.Content != nil {
			content = *msg.Content
		}
		dbg(cfg, "turn %d done in %v — context_in=%d bytes content=%d reasoning=%d bytes finish_reason=%q usage=%s tool_calls=%d",
			turn+1, elapsed, contextBytes, len(content), len(msg.Reasoning), msg.FinishReason, usageString(msg.Usage), len(msg.ToolCalls))
		if msg.Reasoning != "" {
			dbg(cfg, "turn %d REASONING\n%s\n[plugin-review debug: end reasoning %d bytes]", turn+1, msg.Reasoning, len(msg.Reasoning))
		}
		messages = append(messages, msg)

		if len(msg.ToolCalls) == 0 {
			dbg(cfg, "loop end — %d turn(s) in %v", turn+1, time.Since(loopStart))
			return content, nil
		}
		for _, tc := range msg.ToolCalls {
			out, terr := gh.callTool(ctx, cfg.Repo, cfg.PR, cfg, tc.Name, tc.Arguments)
			if terr != nil {
				out = "{\"error\": " + jsonQuote(terr.Error()) + "}"
			}
			dbg(cfg, "  tool %q args=%d bytes -> result=%d bytes", tc.Name, len(tc.Arguments), len(out))
			messages = append(messages, llmkit.Message{Role: "tool", ToolCallID: tc.ID, Content: llmkit.Strptr(out)})
		}
	}
	// Turn budget exhausted: return the last assistant content, if any.
	dbg(cfg, "loop end — turn budget exhausted after %v (MaxTurns=%d)", time.Since(loopStart), cfg.MaxTurns)
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "assistant" && messages[i].Content != nil {
			return *messages[i].Content, nil
		}
	}
	return "Conversation exceeded the turn budget without a verdict.", nil
}

// Emit is the ONE place a review result reaches the outside world: the --out file,
// $GITHUB_OUTPUT, and (optionally) ONE PR comment. It is called exactly once.
func Emit(ctx context.Context, cfg Config, body string) error {
	_, distinct, n := extractVerdict(body)
	if n > 0 && len(distinct) != 1 {
		return fmt.Errorf("ambiguous verdict: multiple distinct Verdict lines: %v", distinct)
	}
	writeGHOutputs(body, n == 1 && len(distinct) == 1, distinct)
	fmt.Println("plugin-review: verdict_lines=" + fmt.Sprint(n) + " distinct=" + fmt.Sprint(distinct))
	if cfg.OutPath != "" {
		if err := os.WriteFile(cfg.OutPath, []byte(body), 0o644); err != nil {
			fmt.Println("plugin-review: out write failed (non-fatal): " + err.Error())
		}
	}
	if cfg.PostComment && cfg.PR > 0 && cfg.Repo != "" {
		footer := fmt.Sprintf("\n\n---\n%s/%s — action-review.\n\n[View action run](%s/%s/actions/runs/%s)",
			cfg.Provider, cfg.Model, cfg.ServerURL, cfg.RepoEnv, cfg.RunID)
		if err := newGHClient().postComment(ctx, cfg.Repo, cfg.PR, body+footer); err != nil {
			fmt.Println("plugin-review: comment post failed (non-fatal): " + err.Error())
		}
	}
	return nil
}

// Run is the top-level command effect: Review then Emit. It returns the process
// exit code the charly host maps (0 = a verdict was produced and emitted).
func Run(ctx context.Context, cfg Config) (int, error) {
	body, err := Review(ctx, cfg)
	if err != nil {
		return 1, err
	}
	if err := Emit(ctx, cfg, body); err != nil {
		return 2, err
	}
	return 0, nil
}

// checkBudget is the fail-closed context guard. It estimates the assembled
// context in tokens (using the provider's own byte ratio when a response reported
// one, else a conservative default) and fails if input + output reserve exceeds
// the window.
func checkBudget(cfg Config, contextBytes int) error {
	// Conservative tokens-per-byte for prose+code on this family (measured ~0.29
	// tokens/byte; use 0.30 so the guard trips BEFORE the provider rejects).
	const tokensPerByte = 0.30
	inputTokens := int(float64(contextBytes) * tokensPerByte)
	used := inputTokens + int(cfg.MaxTokens) + cfg.ContextMarginTokens
	if used > cfg.ContextTokens {
		return fmt.Errorf("inconclusive: PR too large to review in one context — the assembled input is ~%d tokens and the output reserve is %d, exceeding the %d-token window (margin %d). This is NOT a review verdict; split the PR into smaller PRs, or raise AI_REVIEW_CONTEXT_TOKENS if the model's window is larger",
			inputTokens, cfg.MaxTokens, cfg.ContextTokens, cfg.ContextMarginTokens)
	}
	return nil
}

// classify maps a generation failure to the gate's terminal class. It NEVER
// returns a retryable error: each class is terminal and names its knob.
func classify(err error, cfg Config) error {
	var ece *llmkit.EmptyCompletionError
	if errors.As(err, &ece) {
		return fmt.Errorf("inconclusive: the model produced no answer and this is not retryable (%w); raise AI_REVIEW_MAX_TOKENS (currently %d) so the reasoning budget leaves room for the answer, or lower AI_REVIEW_REASONING_EFFORT", err, cfg.MaxTokens)
	}
	if isWholeRequestDeadline(err) {
		return fmt.Errorf("inconclusive: the turn exceeded AI_REVIEW_ATTEMPT_TIMEOUT=%s (whole-request cap; not retried); this is NOT a review verdict — raise AI_REVIEW_ATTEMPT_TIMEOUT for a legitimately long turn, or lower AI_REVIEW_REASONING_EFFORT", cfg.AttemptTimeout)
	}
	if isTimeoutClass(err) {
		return fmt.Errorf("inconclusive: the LLM provider never answered or stopped streaming (idle bound %v); this is NOT a review verdict — re-run the gate", cfg.StreamIdleTimeout)
	}
	return err
}

// debugFailedReasoning dumps the FAILED call's reasoning text — the diagnostic a
// runaway RCA needs (a byte count cannot tell a loop from a long deliberation).
// The typed llmkit errors carry the full text for exactly this purpose.
func debugFailedReasoning(cfg Config, err error) {
	if !cfg.Debug {
		return
	}
	var ece *llmkit.EmptyCompletionError
	var stall *llmkit.StallError
	switch {
	case errors.As(err, &ece):
		dbg(cfg, "FAILED REASONING\n%s\n[plugin-review debug: end failed reasoning %d bytes]", ece.Reasoning, len(ece.Reasoning))
	case errors.As(err, &stall):
		dbg(cfg, "FAILED REASONING\n%s\n[plugin-review debug: end failed reasoning %d bytes]", stall.Reasoning, len(stall.Reasoning))
	default:
		dbg(cfg, "FAILED: no reasoning text on the error (class=%T)", err)
	}
}

// dbg emits one debug line. Debug is the SINGLE switch: it always includes the
// reasoning text, because an RCA always needs it (there is no second knob).
func dbg(cfg Config, format string, a ...any) {
	if cfg.Debug {
		fmt.Printf("plugin-review[debug]: "+format+"\n", a...)
	}
}

func isWholeRequestDeadline(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded)
}

// isTimeoutClass reports a provider-latency failure: a whole-request deadline, an
// idle stall, or a peer that never wrote response headers.
func isTimeoutClass(err error) bool {
	if isWholeRequestDeadline(err) {
		return true
	}
	var stall *llmkit.StallError
	if errors.As(err, &stall) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "context deadline exceeded") ||
		strings.Contains(msg, "Client.Timeout exceeded") ||
		strings.Contains(msg, "timeout awaiting response headers") ||
		strings.Contains(msg, "i/o timeout")
}

// ---- verdict extraction (line-anchored; PASS|BLOCK only) ----

var verdictRe = regexp.MustCompile(`^Verdict:\s*(PASS|BLOCK)\s*$`)

func extractVerdict(text string) (found []string, distinct []string, n int) {
	for _, line := range strings.Split(text, "\n") {
		if m := verdictRe.FindStringSubmatch(strings.TrimRight(line, "\r")); m != nil {
			found = append(found, m[1])
		}
	}
	seen := map[string]bool{}
	for _, f := range found {
		if !seen[f] {
			seen[f] = true
			distinct = append(distinct, f)
		}
	}
	sort.Strings(distinct)
	return found, distinct, len(found)
}
