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
// There is NO hand-rolled HTTP client, no SSE scanner, no tool-delta
// accumulator, and no whole-generation deadline here. The pre-cutover llm.go was
// a 456-line duplicate of llmkit built on raw net/http; it is deleted. That
// duplication was the root cause of the review gate's measured stalls — a second
// client with its own non-streaming request path under a 5-minute whole-request
// deadline, drifting from the client every other consumer uses.

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
		// The review gate's transport is a handful of POSTs per run; a retry
		// inside the SDK's transport would re-run a non-idempotent generation,
		// so retries are owned by the review loop (chatTurn), not the client.
		MaxRetries: 0,
		// The review's generation parameters, pinned as DATA (the retired client
		// hardcoded temperature 0.2 + tool_choice auto in the request literal).
		Params: spec.LLMParams{
			Temperature: &reviewTemperature,
			Tool_choice: "auto",
		},
	}
	if sid := reviewSessionID(); sid != "" {
		c.Headers = map[string]string{"x-opencode-session": sid}
	}
	return c.Normalize()
}

// reviewTemperature is the review gate's generation temperature — low for a
// deterministic verdict. It is a named constant, not a literal in the request.
var reviewTemperature = 0.2

// reviewSessionID returns the per-run session-affinity id (or "" when session
// affinity is explicitly disabled). opencode's Go gateway REJECTS a request
// without the header (HTTP 400 MissingSessionID); gateways that do not use it
// ignore an unknown header.
func reviewSessionID() string {
	if v, ok := os.LookupEnv("AI_REVIEW_SESSION_ID"); ok && v == "" {
		return "" // explicit empty = disable session affinity (a non-opencode gateway)
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
