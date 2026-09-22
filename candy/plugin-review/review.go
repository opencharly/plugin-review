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

// chat is the ONE model call. It is a package var so a test can drive generate()
// deterministically without a network call (the chat seam).
var chat = llmkit.Chat

// review.go — the review engine. ONE path, from first principles:
//
//	assemble the COMPLETE PR context  →  ONE message  →  ONE model call
//	                                  →  extract the verdict  →  emit effects.
//
// A review is a pure function of (rulebook, complete PR, model): all input is
// read ONCE and sent ONCE, so there is nothing for the model to fetch and no
// tool loop — the model either has the input or the run fails closed. Each
// removed path (the tool loop, the --plan executor, fixture mode, retries) was
// accidental complexity that produced the runaway thinking (fragmented context +
// discarded reasoning) or the confusing "what is actually running" problem.
//
//   - Review(ctx, cfg) runs assemble -> generate -> verdict.
//   - generate makes ONE llmkit call (no tools).
//   - The context guard fails HARD if the assembled input + output reserve cannot
//     fit the window, so a review never silently truncates.
//   - A generation failure is classified (deterministic / whole-request cap /
//     idle stall) and NEVER retried: re-issuing the same request is the measured
//     amplifier of long runs.

// Review is the pure engine: it assembles the context, runs the primed review,
// and returns the review text. It has NO side effects (no comment, no file write)
// — emitting those is Emit's single job, so the command path and any embedder
// share ONE engine and fire each effect exactly once.
func Review(ctx context.Context, cfg Config) (string, error) {
	if err := cfg.Validate(); err != nil {
		return "", err
	}
	// FULL resolved-config dump FIRST, so a debug run shows exactly how every
	// AI_REVIEW_* / GITHUB_* input resolved before anything else happens.
	if cfg.Debug {
		fmt.Println("plugin-review[debug]: resolved config:")
		for _, line := range cfg.DebugDump() {
			fmt.Println("  " + line)
		}
	}
	gh := newGHClient()
	c, err := assemble(ctx, cfg, gh)
	if err != nil {
		return "", err
	}
	return generate(ctx, cfg, c)
}

// generate runs the review: assemble the complete context into ONE message and
// make ONE model call. There is no tool loop — the model is given everything up
// front (body, every changed file's full diff, commits, every comment), so there
// is nothing to fetch. The model either has the input or the run fails closed.
func generate(ctx context.Context, cfg Config, c *Context) (string, error) {
	prompt := cfg.EffectivePrompt()
	user := c.Assembled

	// FAIL-CLOSED size guard: the input + the output reserve must fit the window.
	// A review is never silently truncated, so an over-cap PR is a hard,
	// actionable class ("split the PR"), never a partial read.
	if err := checkBudget(cfg, len(prompt)+len(user)); err != nil {
		return "", err
	}

	llm := llmkitConfig(cfg)
	msgs := []llmkit.Message{
		{Role: "system", Content: llmkit.Strptr(prompt)},
		{Role: "user", Content: llmkit.Strptr(user)},
	}
	dbg(cfg, "request — reasoning_effort=%q max_tokens=%d attempt_timeout=%v idle=%v %s context_bytes=(system=%d user=%d) files=%d comments=%d",
		cfg.ReasoningEffort, cfg.MaxTokens, cfg.AttemptTimeout, cfg.StreamIdleTimeout,
		samplingTrace(cfg),
		len(prompt), len(user), len(c.Files), len(c.Comments))

	start := time.Now()
	msg, err := chat(ctx, llm, llmkit.ToSDKMessages(msgs), nil)
	elapsed := time.Since(start)
	if err != nil {
		dbg(cfg, "FAILED after %v: %v", elapsed, err)
		debugFailedReasoning(cfg, err)
		return "", classify(err, cfg)
	}
	content := ""
	if msg.Content != nil {
		content = *msg.Content
	}
	// FAIL-CLOSED (exact): if the provider reported the REAL prompt-token count,
	// enforce the window against it — exact, where the pre-call estimate is not.
	if msg.Usage != nil && msg.Usage.PromptTokens > 0 {
		if err := realBudgetCheck(cfg, msg.Usage.PromptTokens); err != nil {
			return "", err
		}
	}
	dbg(cfg, "response in %v — content=%d bytes reasoning=%d bytes finish_reason=%q usage=%s",
		elapsed, len(content), len(msg.Reasoning), msg.FinishReason, usageString(msg.Usage))
	if msg.Reasoning != "" {
		dbg(cfg, "REASONING\n%s\n[plugin-review debug: end reasoning %d bytes]", msg.Reasoning, len(msg.Reasoning))
	}
	return content, nil
}

// Emit is the ONE place a review result reaches the outside world: the --out file,
// $GITHUB_OUTPUT, and (optionally) ONE PR comment. It is called exactly once, by
// Run, AFTER Run has validated the verdict — so it writes, it does not re-judge.
//
// A failure to write --out is a REAL error: the workflow reads that file to
// extract the verdict, so a silent write failure would turn a produced verdict
// into an INCONCLUSIVE run. The PR comment is deliberately NON-fatal (a network
// hiccup must not discard a valid verdict); $GITHUB_OUTPUT is best-effort.
func Emit(ctx context.Context, cfg Config, body string) error {
	_, distinct, n := extractVerdict(body)
	writeGHOutputs(body, n == 1 && len(distinct) == 1, distinct)
	fmt.Println("plugin-review: verdict_lines=" + fmt.Sprint(n) + " distinct=" + fmt.Sprint(distinct))
	if cfg.OutPath != "" {
		if err := os.WriteFile(cfg.OutPath, []byte(body), 0o644); err != nil {
			return fmt.Errorf("write --out %s: %w", cfg.OutPath, err)
		}
	}
	if cfg.PostComment && cfg.PR > 0 && cfg.Repo != "" {
		footer := fmt.Sprintf("\n\n---\n%s/%s — action-review.\n\n[View action run](%s/%s/actions/runs/%s)",
			cfg.Provider, cfg.Model, cfg.ServerURL, cfg.RepoEnv, cfg.RunID)
		if err := newGHClient().postComment(ctx, cfg.Repo, cfg.PR, body+footer); err != nil {
			fmt.Println("plugin-review: comment post failed (non-fatal): " + err.Error())
		}
	} else {
		// Make the NOT-posting decision observable: an operator debugging "the
		// reviewer ran but no comment appeared" needs to see WHY. The three inputs
		// are printed so an unset AI_REVIEW_POST_COMMENT (off by default) is
		// distinguishable from a missing PR/repo identity.
		fmt.Printf("plugin-review: no PR comment (post_comment=%v pr=%d repo=%q)\n",
			cfg.PostComment, cfg.PR, cfg.Repo)
	}
	return nil
}

// Run is the top-level command effect: Review then Emit. It returns the process
// exit code the charly host maps. The contract is FAIL-CLOSED and matches the
// gate's taxonomy: exit 0 ONLY when a single unambiguous verdict was produced and
// emitted; a verdict-less review is exit 1 (an INCONCLUSIVE class the gate keeps
// RED) so a missing verdict can never read as a pass.
func Run(ctx context.Context, cfg Config) (int, error) {
	body, err := Review(ctx, cfg)
	if err != nil {
		return 1, err
	}
	exit, err := verdictExit(body)
	if exit != 0 || err != nil {
		return exit, err
	}
	if err := Emit(ctx, cfg, body); err != nil {
		return 2, err
	}
	return 0, nil
}

// verdictExit is the FAIL-CLOSED verdict contract in ONE place: exit 0 ONLY for a
// single unambiguous verdict; no verdict is exit 1 (the INCONCLUSIVE class the
// gate keeps RED); a mixed PASS+BLOCK is exit 2. Run uses it, and its test drives
// it directly.
func verdictExit(body string) (int, error) {
	_, distinct, n := extractVerdict(body)
	if n == 0 {
		return 1, fmt.Errorf("inconclusive: the review produced no Verdict line — this is NOT a review verdict (the required check stays RED)")
	}
	if len(distinct) != 1 {
		return 2, fmt.Errorf("inconclusive: ambiguous verdict — multiple distinct Verdict lines: %v", distinct)
	}
	return 0, nil
}

// checkBudget is the fail-closed context guard. It estimates the ONE request's
// size in tokens (a conservative hardcoded ratio — see below) and fails if the
// input + output reserve exceeds the window, so an over-cap PR is refused rather
// than truncated.
//
// The ratio is a deliberate constant, not the provider's reported count: the
// guard must trip BEFORE a request is sent, when no usage is available yet. Once
// a turn HAS reported usage, realBudgetCheck uses the provider's own count.
func checkBudget(cfg Config, contextBytes int) error {
	// Conservative tokens-per-byte for prose+code on this family (measured ~0.29
	// tokens/byte; use 0.30 so the guard trips BEFORE the provider rejects).
	const tokensPerByte = 0.30
	return budgetError(cfg, int(float64(contextBytes)*tokensPerByte))
}

// realBudgetCheck is the fail-closed guard using the provider's OWN prompt-token
// count from a completed turn — exact, where checkBudget is an estimate. Called
// after every turn that reported usage.
func realBudgetCheck(cfg Config, promptTokens int) error {
	return budgetError(cfg, promptTokens)
}

func budgetError(cfg Config, inputTokens int) error {
	used := inputTokens + int(cfg.MaxTokens) + cfg.ContextMarginTokens
	if used > cfg.ContextTokens {
		return fmt.Errorf("inconclusive: PR too large to review in one context — the input is ~%d tokens and the output reserve is %d, exceeding the %d-token window (margin %d). This is NOT a review verdict; split the PR into smaller PRs, or raise AI_REVIEW_CONTEXT_TOKENS if the model's window is larger",
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

// f64 dereferences an optional float for the debug trace; a nil pointer prints as
// "nil" so an unset knob is distinguishable from an explicit zero.
func f64(p *float64) any {
	if p == nil {
		return "nil"
	}
	return *p
}

// samplingTrace renders the RESOLVED sampling the request will carry, so a debug
// run shows exactly which values the engine sent (a bare-environment run proves
// the FromEnv defaults resolved). Temperature always prints its resolved value
// (temperatureOrDefault never yields nil). A pointer knob that is genuinely nil —
// only a bare Config{} leaves TopP/FrequencyPenalty/PresencePenalty nil, never
// FromEnv, which defaults all three — prints as "nil", never as a default, so
// "unset" stays distinguishable from an explicit zero.
func samplingTrace(cfg Config) string {
	return fmt.Sprintf("sampling=(temperature=%v top_p=%v frequency_penalty=%v presence_penalty=%v)",
		f64(temperatureOrDefault(cfg.Temperature)), f64(cfg.TopP), f64(cfg.FrequencyPenalty), f64(cfg.PresencePenalty))
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
