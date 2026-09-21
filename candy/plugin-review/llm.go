package pluginreview

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"time"

	"github.com/opencharly/sdk/llmkit"
	"github.com/opencharly/spec/spec"
)

// llm.go — mapping the review Config onto the SHARED sdk/llmkit client. There is
// exactly ONE OpenAI-compatible client in the org (R3): llmkit owns the wire
// format, the SSE decoder, tool-call assembly, the idle bound, the ollama
// `reasoning` read, and the empty-completion guard. This file owns only the map
// from the review's Config to llmkit.Config.
func llmkitConfig(cfg Config) llmkit.Config {
	c := llmkit.Config{
		BaseURL:     cfg.BaseURL,
		Model:       cfg.Model,
		APIKey:      cfg.APIKey,
		IdleTimeout: cfg.StreamIdleTimeout,
		Timeout:     cfg.AttemptTimeout,
		// The review's transport is a single POST; the SDK transport must not
		// retry a non-idempotent generation. Retries are owned here (none: a
		// failed generation is terminal and classified).
		MaxRetries: 0,
		Params: spec.LLMParams{
			Temperature: temperatureOrDefault(cfg.Temperature),
			Top_p:       cfg.TopP,
			Seed:        cfg.Seed,
		},
	}
	if len(cfg.Stop) > 0 {
		c.Params.Stop = cfg.Stop
	}
	if cfg.ReasoningEffort != "" {
		c.Params.Reasoning_effort = cfg.ReasoningEffort
	}
	if cfg.MaxTokens > 0 {
		c.Params.Max_tokens = &cfg.MaxTokens
	}
	if cfg.MaxCompletionTokens != nil {
		c.Params.Max_completion_tokens = cfg.MaxCompletionTokens
	}
	if cfg.FrequencyPenalty != nil {
		c.Params.Frequency_penalty = cfg.FrequencyPenalty
	}
	if cfg.PresencePenalty != nil {
		c.Params.Presence_penalty = cfg.PresencePenalty
	}
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

// temperatureOrDefault returns the configured temperature, or the review default
// when unset (an explicit AI_REVIEW_TEMPERATURE always wins, including 0).
func temperatureOrDefault(t *float64) *float64 {
	if t != nil {
		return t
	}
	def := reviewTemperature
	return &def
}

// sessionID resolves the session-affinity header value:
//   - AI_REVIEW_SESSION_ID unset → mint a unique id;
//   - set to a value            → use it verbatim;
//   - set to ""                 → "" (affinity disabled).
func sessionID() string {
	if v, ok := os.LookupEnv("AI_REVIEW_SESSION_ID"); ok {
		return v
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Never a constant/empty fallback: every run would share one session.
		return fmt.Sprintf("review-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
