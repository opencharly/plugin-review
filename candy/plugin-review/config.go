package pluginreview

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// config.go — THE single configuration surface for the review engine.
//
// One struct, one constructor (FromEnv), one place every knob is named. There is
// no second config path: the command, the (removed) plan executor and the tests
// all build the SAME Config. Every field maps to exactly one AI_REVIEW_* env var,
// and every var is DECLARED in the candy's charly.yml (env_accept) so the charly
// host passes it through and an operator can see the full surface in one file.
//
// Design rules (R3): a knob exists here ONLY if the engine reads it; a var exists
// in charly.yml ONLY if it is read here. The two lists are kept identical, and
// TestConfigSurfaceMatchesCharlyYML asserts it mechanically, so a new knob cannot
// silently fail to reach the engine (the defect that made CI runs undebuggable:
// AI_REVIEW_DEBUG was set in the workflow but absent from env_accept, so it never
// arrived).

// Config is the fully-resolved review configuration.
type Config struct {
	// ── identity ────────────────────────────────────────────────────────────
	PR   int    // PR number (required)
	Repo string // owner/repo (required)

	// ── provider / model ────────────────────────────────────────────────────
	Provider string
	Model    string
	BaseURL  string
	APIKey   string

	// ── model behaviour (every one env-configurable) ────────────────────────
	// ReasoningEffort selects the thinking depth (AI_REVIEW_REASONING_EFFORT).
	// Empty disables thinking for providers that accept "none" semantics.
	ReasoningEffort string
	// MaxTokens is the SHARED reasoning+answer budget (AI_REVIEW_MAX_TOKENS).
	// Ollama Cloud counts reasoning against it, so it must exceed what thinking
	// consumes. 0 disables.
	MaxTokens int64
	// MaxCompletionTokens is the answer-only budget for providers that separate
	// it (AI_REVIEW_MAX_COMPLETION_TOKENS). Nil when unset.
	MaxCompletionTokens *int64
	// Temperature (AI_REVIEW_TEMPERATURE); Nil = the review default.
	Temperature *float64
	// TopP (AI_REVIEW_TOP_P); Nil = provider default.
	TopP *float64
	// Seed forces determinism where the provider supports it (AI_REVIEW_SEED).
	Seed *int64
	// Stop sequences (AI_REVIEW_STOP, comma-separated).
	Stop []string
	// FrequencyPenalty / PresencePenalty (AI_REVIEW_FREQUENCY_PENALTY /
	// AI_REVIEW_PRESENCE_PENALTY).
	FrequencyPenalty *float64
	PresencePenalty  *float64

	// ── timeouts ────────────────────────────────────────────────────────────
	// StreamIdleTimeout bounds SILENCE between streamed chunks — the primary
	// bound for a streaming provider (AI_REVIEW_STREAM_IDLE_TIMEOUT seconds).
	StreamIdleTimeout time.Duration
	// AttemptTimeout is the whole-request cap (AI_REVIEW_ATTEMPT_TIMEOUT
	// seconds). 0 = no cap beyond the idle bound.
	AttemptTimeout time.Duration

	// ── context guard (fail-closed) ─────────────────────────────────────────
	// ContextTokens is the model's window (AI_REVIEW_CONTEXT_TOKENS). The
	// assembled context + the output reserve must fit it, or the run fails hard
	// with an actionable class — a review is never silently truncated.
	ContextTokens int
	// ContextMarginTokens is the headroom kept below it (AI_REVIEW_CONTEXT_MARGIN).
	ContextMarginTokens int

	// ── prompt ──────────────────────────────────────────────────────────────
	// Prompt is the review rulebook. It is EMBEDDED in the binary (prompt.md
	// beside charly.yml) — there is NO runtime file read, so a run cannot be
	// redirected by a path in the environment. AI_REVIEW_PROMPT_EXTRA appends
	// operator text without replacing the shipped rulebook.
	Prompt      string
	PromptExtra string

	// ── effects ─────────────────────────────────────────────────────────────
	// OutPath writes the review body to a file (AI_REVIEW_OUT or --out).
	OutPath string
	// PostComment posts the review as ONE PR comment (AI_REVIEW_POST_COMMENT).
	PostComment bool

	// ── debug ───────────────────────────────────────────────────────────────
	// Debug emits the per-request trace AND dumps the full reasoning text
	// (AI_REVIEW_DEBUG). There is no separate reasoning knob: an RCA always needs
	// the reasoning, and the reasoning is exactly what a runaway diagnosis reads.
	Debug bool

	// ── runner identity (for the comment footer) ────────────────────────────
	ServerURL string
	RepoEnv   string
	RunID     string
	SessionID string
}

// Defaults — the single place the shipped values live.
const (
	DefaultProvider              = "ollama-cloud"
	DefaultModel                 = "deepseek-v4.1-flash"
	DefaultBaseURL               = "https://ollama.com/v1"
	DefaultReasoningEffort       = "high"
	DefaultMaxTokens       int64 = 262144
	DefaultStreamIdle            = 3 * time.Minute
	DefaultAttemptTimeout        = 15 * time.Minute
	DefaultContextTokens         = 1 << 20 // 1,048,576
	DefaultContextMargin         = 16 << 10
	reviewTemperature            = 0.2
)

// FromEnv builds the Config from the process environment. It is THE constructor:
// the command path, the tests and any embedder call it. Missing required
// identity (PR/Repo) is a returned error, never a zero value that fetches PR 0.
func FromEnv() (Config, error) {
	c := Config{
		Provider:            envStr("AI_REVIEW_PROVIDER", DefaultProvider),
		Model:               envStr("AI_REVIEW_MODEL", DefaultModel),
		BaseURL:             envStr("AI_REVIEW_BASE_URL", DefaultBaseURL),
		APIKey:              envStr("AI_REVIEW_API_KEY", ""),
		ReasoningEffort:     envStrLookup("AI_REVIEW_REASONING_EFFORT", DefaultReasoningEffort),
		MaxTokens:           envInt64("AI_REVIEW_MAX_TOKENS", DefaultMaxTokens),
		StreamIdleTimeout:   envSeconds("AI_REVIEW_STREAM_IDLE_TIMEOUT", DefaultStreamIdle),
		AttemptTimeout:      envSeconds("AI_REVIEW_ATTEMPT_TIMEOUT", DefaultAttemptTimeout),
		ContextTokens:       envInt("AI_REVIEW_CONTEXT_TOKENS", DefaultContextTokens),
		ContextMarginTokens: envInt("AI_REVIEW_CONTEXT_MARGIN", DefaultContextMargin),
		Prompt:              embeddedPrompt,
		PromptExtra:         envStr("AI_REVIEW_PROMPT_EXTRA", ""),
		PostComment:         envBool("AI_REVIEW_POST_COMMENT", true),
		Debug:               envBool("AI_REVIEW_DEBUG", false),
		ServerURL:           envStr("GITHUB_SERVER_URL", "https://github.com"),
		RepoEnv:             envStr("GITHUB_REPOSITORY", ""),
		RunID:               envStr("GITHUB_RUN_ID", ""),
		SessionID:           sessionID(),
	}
	c.MaxCompletionTokens = envInt64Ptr("AI_REVIEW_MAX_COMPLETION_TOKENS")
	c.Temperature = envFloatPtr("AI_REVIEW_TEMPERATURE")
	c.TopP = envFloatPtr("AI_REVIEW_TOP_P")
	c.Seed = envInt64Ptr("AI_REVIEW_SEED")
	c.FrequencyPenalty = envFloatPtr("AI_REVIEW_FREQUENCY_PENALTY")
	c.PresencePenalty = envFloatPtr("AI_REVIEW_PRESENCE_PENALTY")
	c.Stop = envList("AI_REVIEW_STOP")

	// identity: PR_NUMBER then the pull_request event payload; --repo/args are
	// merged by the command parser after this returns.
	if v := envStr("PR_NUMBER", ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.PR = n
		}
	}
	if c.PR == 0 {
		c.PR = prFromEventPath(envStr("GITHUB_EVENT_PATH", ""))
	}
	c.Repo = c.RepoEnv
	c.OutPath = envStr("AI_REVIEW_OUT", "")
	return c, nil
}

// Validate checks the required identity and bounds. A failure is returned BEFORE
// any network call, so a misconfigured run costs nothing.
func (c Config) Validate() error {
	if c.PR <= 0 {
		return fmt.Errorf("no pull request: set PR_NUMBER, pass `pr <N>`, or run under a pull_request event")
	}
	if !strings.Contains(c.Repo, "/") {
		return fmt.Errorf("no repository: set GITHUB_REPOSITORY, pass --repo owner/repo")
	}
	if c.ContextTokens <= 0 {
		return fmt.Errorf("AI_REVIEW_CONTEXT_TOKENS must be positive")
	}
	return nil
}

// EffectivePrompt is the rulebook the run will use: the embedded prompt, plus any
// operator extra. A blank embedded prompt is a build defect, not a runtime state.
func (c Config) EffectivePrompt() string {
	if c.PromptExtra == "" {
		return c.Prompt
	}
	return c.Prompt + "\n\n## Operator additions\n\n" + c.PromptExtra
}

// ---- env helpers (one spelling per type; no scattered os.Getenv) ----

func envStr(name, def string) string {
	if v, ok := os.LookupEnv(name); ok && strings.TrimSpace(v) != "" {
		return v
	}
	return def
}

// envStrLookup honours an explicitly-set empty value (a knob whose "" is
// meaningful, e.g. disabling reasoning), unlike envStr.
func envStrLookup(name, def string) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return def
}

func envInt(name string, def int) int {
	if v, ok := os.LookupEnv(name); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}

func envInt64(name string, def int64) int64 {
	if p := envInt64Ptr(name); p != nil {
		return *p
	}
	return def
}

func envInt64Ptr(name string) *int64 {
	if v, ok := os.LookupEnv(name); ok {
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			return &n
		}
	}
	return nil
}

func envSeconds(name string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(name); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			if n <= 0 {
				return 0 // explicit 0 = disabled
			}
			return time.Duration(n) * time.Second
		}
	}
	return def
}

func envFloatPtr(name string) *float64 {
	if v, ok := os.LookupEnv(name); ok {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			return &f
		}
	}
	return nil
}

func envBool(name string, def bool) bool {
	v, ok := os.LookupEnv(name)
	if !ok {
		return def
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off", "":
		return false
	}
	return def
}

func envList(name string) []string {
	v, ok := os.LookupEnv(name)
	if !ok || strings.TrimSpace(v) == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
