package pluginreview

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/opencharly/sdk/llmkit"
	"github.com/opencharly/spec/spec"
)

// llm.go — the review engine's LLM access, delegated to the SHARED sdk/llmkit
// client. There is exactly ONE OpenAI-compatible client in the org (R3): llmkit
// owns the wire format, the SSE decoder, tool-call assembly, the idle bound, the
// ollama `reasoning` read, and the empty-completion guard. This file owns only
// what is SPECIFIC to the review gate: mapping the reviewConfig onto an
// llmkit.Config and the four read-only review tools onto SDK tool schemas.
//
// There is NO hand-rolled HTTP client, no SSE scanner, and no tool-delta
// accumulator here. The pre-cutover llm.go was a 456-line duplicate of llmkit
// built on raw net/http; it is deleted. The duplication was the defect (R3): the
// private copy lacked llmkit's `reasoning`-delta read, its empty-completion
// guard, and its unified idle-bound semantics, so every client fix had to be
// made twice. (The gate's measured slow runs were a separate, workflow-side
// defect — a hardcoded whole-request cap — fixed in opencharly/.github#102.)

// reviewToolSpec is one read-only review tool. The name IS the dispatch key the
// loop passes to toolSet.call, so there is no separate mapping table.
type reviewToolSpec struct {
	name        string
	description string
}

// reviewTools is the SINGLE declaration of the four read-only PR tools the review
// loop exposes. The descriptions are the model-facing contract (they tell the
// model WHEN to call each tool), so they live with the tool, not in the loop.
var reviewTools = []reviewToolSpec{
	{name: "get_pr_diff", description: "CURRENT unified diff (head vs base)."},
	{name: "get_pr_commits", description: "Commit history of this PR (sha, message, author) — read commit messages since the last review here."},
	{name: "get_pr_thread", description: "CURRENT live issue body plus all prior comments (older comments are stale until re-verified)."},
	{name: "get_pr_meta", description: "PR metadata: title, state, mergeable, head/base sha, file counts."},
}

// sdkTools renders the review tools as the SDK tool union llmkit.Chat takes.
// Every review tool has an empty argument schema — they are zero-arg reads.
func sdkTools() []openai.ChatCompletionToolUnionParam {
	out := make([]openai.ChatCompletionToolUnionParam, 0, len(reviewTools))
	for _, t := range reviewTools {
		out = append(out, llmkit.FunctionTool(t.name, t.description, nil))
	}
	return out
}

// llmConfig maps the resolved reviewConfig onto an llmkit.Config. This is the ONE
// place the review's env vocabulary (AI_REVIEW_*) meets the shared client's
// config: the provider/base_url/model/api key, the per-turn idle bound, and the
// optional session-affinity header.
//
// The review gate's knobs map as:
//   - AI_REVIEW_BASE_URL / _MODEL / _API_KEY → llmkit BaseURL/Model/APIKey
//   - AI_REVIEW_STREAM_IDLE_TIMEOUT          → llmkit IdleTimeout (the silence
//     bound; llmkit deliberately has NO whole-generation deadline, so a slow but
//     progressing turn is never cut off)
//   - AI_REVIEW_MAX_TURNS                     → the caller's turn budget (review.go)
//   - AI_REVIEW_MAX_ATTEMPTS                  → the caller's pass budget (review.go)
func llmConfig(cfg reviewConfig) llmkit.Config {
	c := llmkit.Config{
		BaseURL:     cfg.BaseURL,
		Model:       cfg.Model,
		APIKey:      cfg.APIKey,
		IdleTimeout: cfg.StreamIdleTimeout,
		// Timeout is the OPTIONAL whole-request cap (env
		// AI_REVIEW_ATTEMPT_TIMEOUT). llmkit's default is 0 = no whole-request
		// cap, only the idle bound — which is the right default for a streaming
		// review (a progressing generation is never cut off). An operator may
		// still impose a hard ceiling here; llmkit applies it via the SDK's
		// request timeout, not a hand-rolled http.Client.
		Timeout: cfg.AttemptTimeout,
		// The review gate's transport is a handful of POSTs per run; a retry
		// inside the SDK's transport would re-run a non-idempotent generation,
		// so retries are owned by the review loop (chatTurn), not the client.
		MaxRetries: 0,
		// The review's generation parameters, pinned as DATA (the retired client
		// hardcoded temperature 0.2 + tool_choice auto in the request literal).
		//
		// BOUND THE GENERATION. The measured root cause of the gate's ~13-minute
		// runs: the engine sent NO reasoning cap and NO max_tokens, so against the
		// REAL validator context (28 KB rulebook + 79 KB PR thread + 120 KB diff)
		// deepseek-v4.1-flash generated 1.75 MB of reasoning over 786 s before any
		// answer, which the whole-request cap then killed mid-generation. Setting
		// reasoning_effort=none (env AI_REVIEW_REASONING_EFFORT) and a max_tokens
		// ceiling (env AI_REVIEW_MAX_TOKENS) collapses that to seconds with the
		// same verdict. Defaults are the bounded values; "" / 0 disables each.
		Params: spec.LLMParams{
			Temperature: &reviewTemperature,
			Tool_choice: "auto",
		},
	}
	if cfg.ReasoningEffort != "" {
		c.Params.Reasoning_effort = cfg.ReasoningEffort
	}
	if cfg.MaxTokens > 0 {
		c.Params.Max_tokens = &cfg.MaxTokens
	}
	// Provider attribution headers (OpenRouter ranks/attributes by these; other
	// gateways ignore them) plus the optional session-affinity token. llmkit
	// forwards Config.Headers verbatim.
	headers := map[string]string{
		"HTTP-Referer": "https://github.com/opencharly/action-review",
		"X-Title":      "action-review",
	}
	if cfg.SessionID != "" {
		headers["x-opencode-session"] = cfg.SessionID
	}
	c.Headers = headers
	return c.Normalize()
}

// reviewTemperature is the review gate's generation temperature — low for a
// deterministic verdict. It is a named constant, not a literal in the request.
var reviewTemperature = 0.2

// Generation bounds — the fix for the gate's measured ~13-minute runs (RCA
// 2026-09-19). deepseek-v4.1-flash is a REASONING model: given the real
// validator context (28 KB rulebook + 79 KB PR thread + 120 KB diff) with NO
// cap it generated 1.75 MB of reasoning over 786 s before answering, which the
// workflow's whole-request cap killed mid-generation; a too-tight cap instead
// yields an empty completion (`turn 2: final content len=0`). Capping the OUTPUT
// (max_tokens) and the reasoning depth (reasoning_effort=low) reliably produces a
// verdict in ~1–2 minutes. Both are env-overridable; a zero/empty value disables
// that knob (the operator accepts the unbounded behaviour).
const (
	defaultReasoningEffort = "low"
	defaultMaxTokens       = 65536
)

// reviewSessionID returns the session-affinity id for a run. `AI_REVIEW_SESSION_ID`:
//   - UNSET            → mint one random id (the normal case);
//   - set to a value   → use that value verbatim (an operator pinning a known id);
//   - set to ""        → return "" (session affinity DISABLED — a non-opencode
//     gateway that has no use for the header).
func reviewSessionID() string {
	if v, ok := os.LookupEnv("AI_REVIEW_SESSION_ID"); ok {
		return v // pinned value, or "" to disable
	}
	return newSessionID()
}

// newSessionID mints a per-run session-affinity id (32 lowercase hex chars) with
// no new dependency — crypto/rand + hex is the whole implementation. A rand
// failure must not fall back to a CONSTANT id (every concurrent run would share
// one session, and an empty value would reproduce the very 400 this exists to
// prevent), so it falls back to a value that is still unique per process/call.
func newSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("review-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
