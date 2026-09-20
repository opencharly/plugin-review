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
// llmkit.Config and the six read-only review tools onto SDK tool schemas.
//
// There is NO hand-rolled HTTP client, no SSE scanner, and no tool-delta
// accumulator here. The pre-cutover llm.go was a 456-line duplicate of llmkit
// built on raw net/http; it is deleted. The duplication was the defect (R3): the
// private copy lacked llmkit's `reasoning`-delta read, its empty-completion
// guard, and its unified idle-bound semantics, so every client fix had to be
// made twice. (The gate's measured slow runs had the SAME engine-side cause as
// the duplication: an unbounded reasoning generation. See the generation-bound
// comment on llmConfig.Params below.)

// reviewToolSpec is one read-only review tool. The name IS the dispatch key the
// loop passes to toolSet.call, so there is no separate mapping table.
type reviewToolSpec struct {
	name        string
	description string
	// params is the JSON-Schema argument object (nil = a zero-arg read). Only
	// get_pr_comment takes an argument (the comment id from the thread index).
	params map[string]any
}

// reviewTools is the SINGLE declaration of the read-only PR tools the review
// loop exposes. The descriptions are the model-facing contract (they tell the
// model WHEN to call each tool), so they live with the tool, not in the loop.
//
// THREAD DELIVERY (RCA: the 64 KiB aggregate cap): get_pr_thread returns the
// current body + a compact comment INDEX (ids, authors, dates, sizes, short
// previews) — NEVER the aggregate bodies. The model reads a comment by calling
// get_pr_comment with the id from the index. This keeps every tool result small
// enough to be delivered complete: the index is O(#comments) metadata, and each
// comment body travels as its own message, bounded once at the per-message cap.
var reviewTools = []reviewToolSpec{
	{name: "get_pr_meta", description: "PR metadata: title, state, mergeable, head/base sha, file counts. Call this FIRST."},
	{name: "get_pr_body", description: "The CURRENT live PR/issue body as its own message — authoritative; it supersedes anything an older comment said. Read as a single unit, never bundled with the comments."},
	{name: "get_pr_files", description: "The changed-FILE INDEX: path, status, additions, deletions and patch size for EVERY changed file, plus file_count and total_patch_bytes. The DIFFS are NOT here — call get_pr_file for EACH path to read its full patch. Read this index FIRST, then read EVERY file it lists (the run is refused a verdict until you have)."},
	{name: "get_pr_file", description: "Read ONE changed file's FULL patch (its unified diff) by path, as its own message. This is how you read the diff: ONE FILE PER CALL, for EVERY file in the get_pr_files index. Reading one file per message is required so a large multi-file diff is never truncated.", params: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string", "description": "A path from the get_pr_files files[] index (exact string)."},
		},
		"required": []string{"path"},
	}},
	{name: "get_pr_commits", description: "Commit history of this PR (sha, message, author) — read commit messages since the last review here."},
	{name: "get_pr_thread", description: "The comment INDEX: id/author/date/size/preview for every comment, plus the per-comment byte cap. Comment BODIES are NOT included — call get_pr_comment with a row's id to read one comment as its own message. Older comments are stale until re-verified."},
	{name: "get_pr_comment", description: "Read ONE comment by id (from get_pr_thread's index) as its own message: its full body plus author and date. Reading comments ONE AT A TIME is the intended path — it keeps each message small so nothing is truncated.", params: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id": map[string]any{"type": "integer", "description": "The comment id from get_pr_thread's comments[] index."},
		},
		"required": []string{"id"},
	}},
}

// sdkTools renders the review tools as the SDK tool union llmkit.Chat takes.
// get_pr_comment carries an `id` argument schema; the rest are zero-arg reads.
func sdkTools() []openai.ChatCompletionToolUnionParam {
	out := make([]openai.ChatCompletionToolUnionParam, 0, len(reviewTools))
	for _, t := range reviewTools {
		out = append(out, llmkit.FunctionTool(t.name, t.description, t.params))
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
		// THINKING + BUDGET. reasoning_effort selects the thinking depth (the
		// QUALITY default is `high`) and max_tokens is the SHARED reasoning+answer
		// budget, so it is set well above what thinking consumes (see the
		// defaultReasoningEffort/defaultMaxTokens note above for the measurement).
		// temperature is configurable (env AI_REVIEW_TEMPERATURE). Every one of
		// these is env-overridable; "" / 0 disables that knob.
		//
		// tool_choice is data for providers that use it (OpenRouter); Ollama
		// Cloud's OpenAI-compatible endpoint documents it as UNSUPPORTED and
		// ignores it, so the engine must not depend on it.
		Params: spec.LLMParams{
			Temperature: temperatureOrDefault(cfg.Temperature),
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

// reviewTemperature is the review gate's DEFAULT generation temperature — low
// for a deterministic verdict. It is a named constant, not a literal in the
// request, and is overridable via env AI_REVIEW_TEMPERATURE.
var reviewTemperature = 0.2

// temperatureOrDefault returns the configured temperature, or the default when
// unset (an explicit AI_REVIEW_TEMPERATURE always wins, including 0).
func temperatureOrDefault(t *float64) *float64 {
	if t != nil {
		return t
	}
	return &reviewTemperature
}

// Generation bounds — the fix for the gate's measured ~13-minute runs (RCA
// 2026-09-19, CORRECTED 2026-09-20 by measurement).
//
// deepseek-v4.1-flash is a REASONING model. On Ollama Cloud's OpenAI-compatible
// endpoint (https://ollama.com/v1) `reasoning_effort` selects the thinking depth
// (high|medium|low|max|none) and `max_tokens` is a SINGLE SHARED BUDGET for
// reasoning + answer — NOT an answer-only cap. Measured consequences:
//
//   - `high` thinking can spend more than a SMALL budget on reasoning and then
//     return an empty completion with finish_reason=length: a 16384-token cap
//     produced 57363 / 66536 / 64949 bytes of reasoning on three consecutive
//     attempts with content=0. The engine then re-issued the SAME doomed request
//     3x (2m52s wasted) — the measured amplifier of the long runs.
//   - With a budget that leaves room ABOVE the thinking, `high` completes: a
//     262144-token cap produced a BLOCK verdict with zero empty completions in
//     49–123 s across repeat runs.
//
// So the QUALITY default is `high`, and defaultMaxTokens is set well above what
// thinking consumes (the model's own maximum output is 393216; the provider
// rejects a value above it). Never lower reasoning_effort to save time — raise
// max_tokens instead. Both are env-overridable; "" / 0 disables that knob.
const (
	defaultReasoningEffort = "high"
	defaultMaxTokens       = 262144
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
