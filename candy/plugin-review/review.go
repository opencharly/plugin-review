package pluginreview

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/opencharly/sdk/llmkit"
)

// =============================================================================
// command:review — the 1:1 port of opencharly/pi-review-action's index.js.
//   charly review pr <N> [--repo owner/repo] [--out PATH] [--self-test]
//                         [--self-test-verdict] [--plan PATH]
// Configuration precedence: CLI flag > env var (AI_REVIEW_*) > baked image ENV
// (charly.yml var:) > hardcoded default.
// =============================================================================

const (
	defaultProvider = "openrouter"
	defaultModel    = "~deepseek/deepseek-v4-flash-latest"
	defaultBaseURL  = "https://openrouter.ai/api/v1"
	defaultMaxTurns = 20
	// defaultAttemptTimeout is the OPTIONAL whole-request cap (env
	// AI_REVIEW_ATTEMPT_TIMEOUT, seconds). The real bound for a streaming review
	// is defaultStreamIdleTimeout; this is a defense-in-depth outer ceiling an
	// operator may impose, preserved from the pre-llmkit client so the knob's
	// contract is unchanged. The workflow must NOT hand it a value smaller than a
	// large turn legitimately needs (a 5-minute override here was the measured
	// cause of ~14m runs: turn 2 timed out, the per-turn retry re-sent it).
	defaultAttemptTimeout = 15 * time.Minute
	// defaultStreamIdleTimeout is the maximum SILENCE inside a streamed
	// completion — the gap between chunks, including the wait for the first one
	// (prompt processing of a large tool-result context). It is the PRIMARY time
	// bound: llmkit has no whole-generation deadline, so a slow-but-progressing
	// review is never cut off while a silent provider fails in bounded time.
	// (An OPTIONAL whole-request cap is preserved as defaultAttemptTimeout below.)
	defaultStreamIdleTimeout = 3 * time.Minute
	// defaultToolResultMaxBytes caps ONE tool result appended to the
	// conversation (the general cap). Tool output is the context-growth trigger:
	// get_pr_diff is up to 96 KiB and get_pr_thread up to 100 comments x 24 KiB.
	defaultToolResultMaxBytes = 64 << 10
	// defaultContextTokens is the model's context window (env
	// AI_REVIEW_CONTEXT_TOKENS). deepseek-v4.1-flash advertises 1,048,576 in
	// models.dev; the provider is authoritative, so it is env-overridable.
	defaultContextTokens = 1 << 20
	// defaultContextMarginTokens is the headroom kept below the context window so
	// the guard trips BEFORE the provider does (env AI_REVIEW_CONTEXT_MARGIN).
	defaultContextMarginTokens = 16 << 10
	// defaultRetryBackoff is the base backoff between re-issues of a FAILED TURN
	// request (attempt x base = 5s then 10s — the action's original schedule).
	defaultRetryBackoff = 5 * time.Second
	// maxTurnRequestAttempts bounds the re-issues of a SINGLE failed turn request
	// (never of the whole loop). Every review tool is a read-only GET and the loop
	// is the only writer of the conversation, so re-issuing is safe and
	// deterministic.
	maxTurnRequestAttempts = 3
	// defaultMaxAttempts bounds whole-loop passes, re-run only when a completed
	// pass produced no Verdict line (a format failure, not a transport failure).
	// Env AI_REVIEW_MAX_ATTEMPTS; 1 = fail hard, no repeat cycle.
	defaultMaxAttempts = 3
)

type reviewConfig struct {
	PR         int
	Repo       string // owner/repo
	Provider   string
	Model      string
	BaseURL    string
	APIKey     string
	MaxTurns   int
	PromptPath string
	OutPath    string
	PlanPath   string
	ServerURL  string
	RepoEnv    string // GITHUB_REPOSITORY fallback
	RunID      string
	// MaxAttempts bounds whole-loop passes (env AI_REVIEW_MAX_ATTEMPTS, default
	// 3; 1 = fail hard, no repeat cycle).
	MaxAttempts int
	// AttemptTimeout is the OPTIONAL whole-request cap (env
	// AI_REVIEW_ATTEMPT_TIMEOUT, seconds). Zero means "no cap beyond the idle
	// bound" — the streaming-appropriate default.
	AttemptTimeout time.Duration
	// StreamIdleTimeout bounds SILENCE inside a streamed completion (env
	// AI_REVIEW_STREAM_IDLE_TIMEOUT, seconds). The primary time bound.
	StreamIdleTimeout time.Duration
	// ToolResultMaxBytes caps ONE tool result before it enters the conversation
	// (env AI_REVIEW_TOOL_RESULT_MAX_BYTES).
	ToolResultMaxBytes int
	// ContextTokens is the model's context window (env AI_REVIEW_CONTEXT_TOKENS).
	// The mechanical PR-size guard fails hard when input + output reserve exceeds
	// it, so a PR too large to review whole is refused, never silently truncated.
	ContextTokens int
	// ContextMarginTokens is the headroom kept below ContextTokens (env
	// AI_REVIEW_CONTEXT_MARGIN).
	ContextMarginTokens int
	// ReasoningEffort sets the thinking depth (env AI_REVIEW_REASONING_EFFORT;
	// default "high" — the eval QUALITY default; never reduce it to save time).
	// Ollama Cloud's OpenAI-compatible endpoint accepts high|medium|low|max|none.
	//
	// CRITICAL — max_tokens is a SHARED budget for reasoning + answer. A high
	// thinking model can spend MORE than a small budget thinking and then return
	// an empty completion with finish_reason=length (measured: reasoning alone
	// consumed 57KB/66KB/64KB of a 16384-token cap, three attempts, 2m52s). So
	// ReasoningEffort and MaxTokens must be raised TOGETHER; MaxTokens must leave
	// ample room above what thinking can consume.
	ReasoningEffort string
	// MaxTokens bounds the completion, reasoning + answer (env
	// AI_REVIEW_MAX_TOKENS; 0 disables). Default defaultMaxTokens (see the
	// reasoning-budget note above). The provider rejects a value above the
	// model's maximum output tokens.
	MaxTokens int64
	// Temperature is the sampling temperature (env AI_REVIEW_TEMPERATURE; default
	// reviewTemperature). Low for a deterministic verdict; configurable so an
	// operator can tune it without a rebuild.
	Temperature *float64
	// RetryBackoff is the base backoff between re-issues of a failed turn
	// request. Not env-configurable: it is a scheduling constant, not a policy
	// knob (tests set it directly).
	RetryBackoff time.Duration
	// SessionID is the review RUN's session-affinity token: one id per run, shared
	// by every pass and every turn (a per-request id would defeat the routing it
	// exists for). Minted once in parseReviewArgs; empty disables the header.
	SessionID string
	// Debug turns on the full diagnostic trace (env AI_REVIEW_DEBUG): per-turn
	// wall-clock + time-to-first-chunk, token usage (prompt/completion/reasoning),
	// finish_reason, tool name/arg/result byte sizes, per-request context bytes,
	// and the model's REASONING text. RCA requires the reasoning and the timings
	// to be observable; without this the engine emitted only three aggregate lines
	// and a 7m35s run could not be attributed to a turn.
	Debug bool
	// DebugReasoning additionally prints the full reasoning text per turn. It is a
	// separate knob because reasoning can be megabytes; the timing/usage trace is
	// cheap and safe to leave on, the reasoning dump is opt-in.
	DebugReasoning bool
}

func (c *reviewConfig) owner() string { return strings.SplitN(c.Repo, "/", 2)[0] }
func (c *reviewConfig) repo() string {
	if i := strings.Index(c.Repo, "/"); i >= 0 {
		return c.Repo[i+1:]
	}
	return ""
}

// RunReviewFromArgs is the shared command effect (in-proc Invoke(OpRun) path).
func RunReviewFromArgs(args []string) (int, error) {
	exit, err := runReview(context.Background(), args)
	return exit, err
}

func runReview(ctx context.Context, args []string) (int, error) {
	cfg, mode, err := parseReviewArgs(args, os.Environ())
	if err != nil {
		return 2, err
	}
	switch mode {
	case "self-test":
		fmt.Println("plugin-review self-test: ok (command:review resolves)")
		return 0, nil
	case "self-test-verdict":
		return runVerdictSelfTest()
	case "plan":
		return runPlan(ctx, cfg) // review-plan.yml executor (plan.go)
	default: // "review"
		return runCoreReview(ctx, cfg)
	}
}

func parseReviewArgs(args []string, environ []string) (reviewConfig, string, error) {
	cfg := reviewConfig{
		Provider: defaultProvider, Model: defaultModel, BaseURL: defaultBaseURL,
		MaxTurns: defaultMaxTurns, AttemptTimeout: defaultAttemptTimeout,
		StreamIdleTimeout: defaultStreamIdleTimeout, ToolResultMaxBytes: defaultToolResultMaxBytes,
		ContextTokens: defaultContextTokens, ContextMarginTokens: defaultContextMarginTokens,
		ReasoningEffort: defaultReasoningEffort, MaxTokens: defaultMaxTokens,
		RetryBackoff: defaultRetryBackoff,
		MaxAttempts:  defaultMaxAttempts,
		SessionID:    reviewSessionID(),
		ServerURL:    getenvAny("GITHUB_SERVER_URL"),
		RepoEnv:      getenvAny("GITHUB_REPOSITORY"),
		RunID:        getenvAny("GITHUB_RUN_ID"),
	}
	if cfg.ServerURL == "" {
		cfg.ServerURL = "https://github.com"
	}

	// env overrides (AI_REVIEW_*: provider/model/base_url/max_turns/max_attempts; REVIEW_PROMPT_PATH; REVIEW_PLAN_PATH)
	if v := getenvAny("AI_REVIEW_PROVIDER"); v != "" {
		cfg.Provider = v
	}
	if v := getenvAny("AI_REVIEW_MODEL"); v != "" {
		cfg.Model = v
	}
	if v := getenvAny("AI_REVIEW_BASE_URL"); v != "" {
		cfg.BaseURL = v
	}
	if v := getenvAny("AI_REVIEW_ATTEMPT_TIMEOUT"); v != "" {
		if n, e := parseInt(v); e == nil && n > 0 {
			cfg.AttemptTimeout = time.Duration(n) * time.Second
		}
	}
	if v := getenvAny("AI_REVIEW_STREAM_IDLE_TIMEOUT"); v != "" {
		if n, e := parseInt(v); e == nil && n > 0 {
			cfg.StreamIdleTimeout = time.Duration(n) * time.Second
		}
	}
	if v := getenvAny("AI_REVIEW_TOOL_RESULT_MAX_BYTES"); v != "" {
		if n, e := parseInt(v); e == nil && n > 0 {
			cfg.ToolResultMaxBytes = n
		}
	}
	if v := getenvAny("AI_REVIEW_CONTEXT_TOKENS"); v != "" {
		if n, e := parseInt(v); e == nil && n > 0 {
			cfg.ContextTokens = n
		}
	}
	if v := getenvAny("AI_REVIEW_CONTEXT_MARGIN"); v != "" {
		if n, e := parseInt(v); e == nil && n > 0 {
			cfg.ContextMarginTokens = n
		}
	}
	if v, ok := os.LookupEnv("AI_REVIEW_REASONING_EFFORT"); ok {
		cfg.ReasoningEffort = v // "" explicitly disables thinking
	}
	if v := getenvAny("AI_REVIEW_MAX_TOKENS"); v != "" {
		if n, e := parseInt64(v); e == nil && n > 0 {
			cfg.MaxTokens = n
		}
	}
	if v := getenvAny("AI_REVIEW_TEMPERATURE"); v != "" {
		if f, e := strconv.ParseFloat(v, 64); e == nil {
			cfg.Temperature = &f
		}
	}
	if v := getenvAny("AI_REVIEW_MAX_TURNS"); v != "" {
		if n, e := parseInt(v); e == nil && n > 0 {
			cfg.MaxTurns = n
		}
	}
	if v := getenvAny("AI_REVIEW_MAX_ATTEMPTS"); v != "" {
		if n, e := parseInt(v); e == nil && n > 0 {
			cfg.MaxAttempts = n
		}
	}
	// Debug: a bare env flag (set to any non-empty value). AI_REVIEW_DEBUG gives
	// the timing/usage/tool trace; AI_REVIEW_DEBUG_REASONING additionally dumps the
	// full reasoning text. Both are read via LookupEnv so an explicit empty value
	// leaves them off (the same empty-is-unset convention the other knobs use).
	if _, ok := os.LookupEnv("AI_REVIEW_DEBUG"); ok {
		cfg.Debug = truthyEnv(getenvAny("AI_REVIEW_DEBUG"))
	}
	if _, ok := os.LookupEnv("AI_REVIEW_DEBUG_REASONING"); ok {
		cfg.DebugReasoning = truthyEnv(getenvAny("AI_REVIEW_DEBUG_REASONING"))
	}
	cfg.PromptPath = getenvAny("REVIEW_PROMPT_PATH")
	cfg.PlanPath = getenvAny("REVIEW_PLAN_PATH")
	cfg.APIKey = getenvAny("AI_REVIEW_API_KEY")

	mode := "review"
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--self-test":
			mode = "self-test"
		case a == "--self-test-verdict":
			mode = "self-test-verdict"
		case a == "--plan":
			mode = "plan"
			if i+1 < len(args) {
				cfg.PlanPath = args[i+1]
				i++
			}
		case a == "--out" || a == "-o":
			if i+1 < len(args) {
				cfg.OutPath = args[i+1]
				i++
			}
		case a == "--repo":
			if i+1 < len(args) {
				cfg.Repo = args[i+1]
				i++
			}
		case a == "pr":
			if i+1 < len(args) {
				n, e := parseInt(args[i+1])
				if e == nil {
					cfg.PR = n
					i++
				}
			}
		default:
			if n, e := parseInt(a); e == nil {
				cfg.PR = n
			}
		}
	}
	// PR identity: flag > PR_NUMBER env > GITHUB_EVENT_PATH payload
	if cfg.PR == 0 {
		if v := getenvAny("PR_NUMBER"); v != "" {
			if n, e := parseInt(v); e == nil {
				cfg.PR = n
			}
		}
	}
	if cfg.PR == 0 {
		cfg.PR = prFromEventPath(getenvAny("GITHUB_EVENT_PATH"))
	}
	if cfg.Repo == "" && cfg.RepoEnv != "" {
		cfg.Repo = cfg.RepoEnv
	}
	return cfg, mode, nil
}

// parseInt parses a NON-NEGATIVE decimal integer: it rejects a negative value
// and a non-number, so a malformed config knob fails rather than silently
// truncating. (A leading '+' is accepted by strconv.Atoi; that is a valid
// positive integer and every caller treats it as such — an over-strict digit
// loop would only add a rejection with no behavioural benefit.)
// parseInt64 parses a positive int64 (a token budget). Non-positive/empty is an
// error so the caller keeps its default.
func parseInt64(s string) (int64, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("not a positive integer: %q", s)
	}
	return n, nil
}

func parseInt(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("not a non-negative integer: %q", s)
	}
	return n, nil
}

type ghEvent struct {
	PullRequest *struct {
		Number int `json:"number"`
		Head   struct {
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			SHA string `json:"sha"`
		} `json:"base"`
	} `json:"pull_request"`
	Repository *struct {
		Owner struct {
			Login string `json:"login"`
		} `json:"owner"`
		Name string `json:"name"`
	} `json:"repository"`
}

func prFromEventPath(p string) int {
	if p == "" {
		return 0
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return 0
	}
	var ev ghEvent
	if json.Unmarshal(raw, &ev) != nil {
		return 0
	}
	// A workflow_dispatch (or any non-PR) event has no `pull_request` object, so
	// ev.PullRequest is nil. Dereferencing it panicked (measured: a SIGSEGV in
	// prFromEventPath during the bootstrap self-test on a workflow_dispatch run,
	// which crashed `charly review --self-test`). A missing PR context is not an
	// error here — it just means this event cannot supply a PR number; the caller
	// falls back to PR_NUMBER / the flag and reports "no pull request context".
	if ev.PullRequest == nil {
		return 0
	}
	return ev.PullRequest.Number
}

// =============================================================================
// the core engine (B1–B6 of the plan) — gather → LLM tool-loop → verdict → comment
// =============================================================================

// runReviewEngine runs the read-only tool loop and returns the review text. It
// has NO side effects (no PR comment, no $GITHUB_OUTPUT write) — emitting those
// is the caller's single job, so the standalone `charly review pr N` path and the
// `--plan` path can share ONE engine and still fire each effect exactly once.
func runReviewEngine(ctx context.Context, cfg reviewConfig) (string, error) {
	if cfg.PR == 0 {
		return "", fmt.Errorf("no pull request context: pass a PR number (charly review pr <N>), the PR_NUMBER env var, or run under a pull_request event")
	}
	if cfg.Repo == "" {
		return "", fmt.Errorf("no repository context: pass --repo owner/repo or set GITHUB_REPOSITORY")
	}
	prompt := loadPrompt(cfg.PromptPath)

	gh := newGHClient()
	headSHA, baseSHA := "", ""
	// resolve head/base from meta once (cheap) so thread + user prompt carry them
	if m, err := gh.toolMeta(ctx, cfg.Repo, cfg.PR); err == nil {
		headSHA, baseSHA = m.HeadSHA, m.BaseSHA
	}
	deps := toolSet{gh: gh, owner: cfg.owner(), repo: cfg.repo(), pr: cfg.PR, headSHA: headSHA, baseSHA: baseSHA,
		toolResultMaxBytes: cfg.ToolResultMaxBytes}
	return runAgentLoop(ctx, cfg, prompt, deps)
}

// emitReviewEffects is the ONE place a review result reaches the outside world:
// $GITHUB_OUTPUT (response/success/verdict), the --out file, and ONE PR comment.
// Both the standalone path (via runCoreReview) and the plan path (via runPlan)
// call it exactly once per run — never twice.
func emitReviewEffects(ctx context.Context, cfg reviewConfig, body string) error {
	_, distinct, n := extractVerdict(body)
	if n > 0 && len(distinct) != 1 {
		return fmt.Errorf("ambiguous verdict: multiple distinct Verdict lines: %v", distinct)
	}
	writeGHOutputs(body, n > 0 && len(distinct) == 1, distinct)
	fmt.Println("plugin-review: verdict_lines=" + fmt.Sprint(n) + " distinct=" + fmt.Sprint(distinct))
	if cfg.OutPath != "" {
		_ = os.WriteFile(cfg.OutPath, []byte(body), 0o644)
	}
	if cfg.PR != 0 && cfg.Repo != "" {
		runURL := cfg.ServerURL + "/" + cfg.RepoEnv + "/actions/runs/" + cfg.RunID
		footer := fmt.Sprintf("\n\n---\n%s/%s — action-review.\n\n[View action run](%s)", cfg.Provider, cfg.Model, runURL)
		if err := newGHClient().postComment(ctx, cfg.Repo, cfg.PR, body+footer); err != nil {
			fmt.Println("plugin-review: comment post failed (non-fatal): " + err.Error())
		}
	}
	return nil
}

func runCoreReview(ctx context.Context, cfg reviewConfig) (int, error) {
	review, err := runReviewEngine(ctx, cfg)
	if err != nil {
		return 1, err
	}
	if err := emitReviewEffects(ctx, cfg, review); err != nil {
		return 2, err
	}
	return 0, nil
}

func loadPrompt(path string) string {
	if path != "" {
		if raw, err := os.ReadFile(path); err == nil && len(raw) > 0 {
			return string(raw)
		}
	}
	// fallbacks: <project>/prompt/validator.md (config repo default), baked image path
	for _, cand := range []string{"prompt/validator.md", "/etc/charly/review/prompt.md"} {
		if raw, err := os.ReadFile(cand); err == nil && len(raw) > 0 {
			return string(raw)
		}
	}
	return "You are the PR validator. Review the PR and emit exactly one final line: Verdict: PASS or Verdict: BLOCK."
}

// toolSet wires the read-only tools to the gh client + fixtures.
type toolSet struct {
	gh      *ghClient
	owner   string
	repo    string
	pr      int
	headSHA string
	baseSHA string
	fixture string // non-empty → offline mode
	// toolResultMaxBytes is the per-message cap the thread tool reports as its
	// per-comment budget (so the model knows how much of a large comment
	// get_pr_comment will return). Zero → the engine default.
	toolResultMaxBytes int
	// fileIndex is the full changed-file set from get_pr_files; filesRead is the
	// subset fetched via get_pr_file. Together they MECHANICALLY enforce "read
	// every changed file in full": a verdict is refused until filesRead covers
	// fileIndex (Phase C anti-skim guard).
	fileIndex   []string
	fileIndexed bool
	filesRead   map[string]bool
}

// recordFileIndex stores the full changed-file set the anti-skim guard checks.
func (t *toolSet) recordFileIndex(idx prFileIndex) {
	t.fileIndexed = true
	t.fileIndex = t.fileIndex[:0]
	for _, f := range idx.Files {
		t.fileIndex = append(t.fileIndex, f.Path)
	}
}

// recordFileRead marks one file as read in full.
func (t *toolSet) recordFileRead(path string) {
	if t.filesRead == nil {
		t.filesRead = map[string]bool{}
	}
	t.filesRead[path] = true
}

// unreadFiles returns the changed files NOT yet fetched via get_pr_file. Empty
// when the index was never read (the guard then cannot enforce — the prompt and
// the file-index tool description require it, and this returns empty so a run
// that never calls get_pr_files is not spuriously failed). It returns the index
// itself before the index is read only when the index WAS read.
func (t *toolSet) unreadFiles() []string {
	if !t.fileIndexed {
		return nil
	}
	var out []string
	for _, p := range t.fileIndex {
		if !t.filesRead[p] {
			out = append(out, p)
		}
	}
	return out
}

// commentArgs is the parsed argument object for get_pr_comment.
type commentArgs struct {
	ID int `json:"id"`
}

// fileArgs is the parsed argument object for get_pr_file.
type fileArgs struct {
	Path string `json:"path"`
}

// call dispatches one tool by name. args is the raw tool-call argument JSON
// (empty / "{}" for the zero-arg reads); only get_pr_comment consumes it, and a
// missing or non-positive id is a clear tool error rather than a silent
// whole-thread fetch.
func (t *toolSet) call(ctx context.Context, name, args string) (string, error) {
	fx := ""
	if t.fixture != "" {
		fx = filepath.Join("fixtures", t.fixture+"-"+name+".json")
	}
	// A tool that needs GitHub with no client is a CLEAR error, never a nil
	// dereference (R1: a test/local construction without a client must not crash
	// the run).
	if fx == "" && t.gh == nil && name != "" {
		return "", fmt.Errorf("%s: no GitHub client is configured", name)
	}
	switch name {
	case "get_pr_files":
		if fx != "" {
			return readFixture(fx)
		}
		idx, err := t.gh.toolFiles(ctx, t.repo, t.pr)
		if err != nil {
			return "", err
		}
		// Record the index so the anti-skim guard knows the full changed-file set
		// (Phase C): a verdict is refused until every one of these is read.
		t.recordFileIndex(idx)
		b, _ := json.Marshal(idx)
		return string(b), nil
	case "get_pr_file":
		// Argument validation FIRST (never a silent fallback): a missing path is
		// a tool error in every mode.
		var a fileArgs
		if strings.TrimSpace(args) != "" {
			if err := json.Unmarshal([]byte(args), &a); err != nil {
				return "", fmt.Errorf("get_pr_file: invalid arguments %q: %w", args, err)
			}
		}
		if strings.TrimSpace(a.Path) == "" {
			return "", fmt.Errorf("get_pr_file: a file path is required (read the get_pr_files index for the exact paths)")
		}
		if fx != "" {
			return readFixture(fx)
		}
		f, err := t.gh.toolFile(ctx, t.repo, t.pr, a.Path)
		if err != nil {
			return "", err
		}
		t.recordFileRead(a.Path)
		b, _ := json.Marshal(f)
		return string(b), nil
	case "get_pr_commits":
		if fx != "" {
			return readFixture(fx)
		}
		cs, err := t.gh.toolCommits(ctx, t.repo, t.pr)
		if err != nil {
			return "", err
		}
		b, _ := json.Marshal(cs)
		return string(b), nil
	case "get_pr_body":
		if fx != "" {
			return readFixture(fx)
		}
		bd, err := t.gh.toolBody(ctx, t.repo, t.pr)
		if err != nil {
			return "", err
		}
		b, _ := json.Marshal(bd)
		return string(b), nil
	case "get_pr_thread":
		if fx != "" {
			return readFixture(fx)
		}
		th, err := t.gh.toolThread(ctx, t.repo, t.pr, t.headSHA, t.baseSHA, t.toolResultMaxBytes)
		if err != nil {
			return "", err
		}
		b, _ := json.Marshal(th)
		return string(b), nil
	case "get_pr_comment":
		// Argument validation comes FIRST, before the fixture shortcut: a missing
		// or non-positive id is a tool error in every mode, never a silent fetch.
		var a commentArgs
		if strings.TrimSpace(args) != "" {
			if err := json.Unmarshal([]byte(args), &a); err != nil {
				return "", fmt.Errorf("get_pr_comment: invalid arguments %q: %w", args, err)
			}
		}
		if a.ID <= 0 {
			return "", fmt.Errorf("get_pr_comment: a positive comment id is required (read get_pr_thread's comments[] index); got %d", a.ID)
		}
		if fx != "" {
			return readFixture(fx)
		}
		raw, err := t.gh.toolComment(ctx, t.repo, a.ID)
		if err != nil {
			return "", err
		}
		return string(raw), nil
	case "get_pr_meta":
		if fx != "" {
			return readFixture(fx)
		}
		m, err := t.gh.toolMeta(ctx, t.repo, t.pr)
		if err != nil {
			return "", err
		}
		b, _ := json.Marshal(m)
		return string(b), nil
	}
	return "", fmt.Errorf("unknown tool %q", name)
}

func readFixture(name string) (string, error) {
	raw, err := os.ReadFile(name)
	if err != nil {
		return "", fmt.Errorf("fixture %s: %w", name, err)
	}
	return string(raw), nil
}

// runAgentLoop: the review's pass budget over the shared llmkit client.
// temperature/streaming/bounds belong to llmkit; this layer owns only the
// REVIEW-specific policy: a completed pass with no Verdict line is retried
// (cfg.MaxAttempts, env AI_REVIEW_MAX_ATTEMPTS, default 3; 1 = fail hard), and a
// transport failure is reported as the distinct inconclusive class rather than
// being retried as a whole loop.
func runAgentLoop(ctx context.Context, cfg reviewConfig, prompt string, tools toolSet) (string, error) {
	backoff := cfg.RetryBackoff
	if backoff <= 0 {
		backoff = defaultRetryBackoff
	}
	maxPasses := cfg.MaxAttempts
	if maxPasses < 1 {
		maxPasses = 1
	}
	for pass := 1; pass <= maxPasses; pass++ {
		fmt.Printf("plugin-review: pass %d/%d — provider=%s model=%s base_url=%s\n", pass, maxPasses, cfg.Provider, cfg.Model, cfg.BaseURL)
		review, err := agentLoopOnce(ctx, cfg, prompt, tools)
		if err != nil {
			fmt.Println("plugin-review: pass " + fmt.Sprint(pass) + " failed: " + err.Error())
			// A DETERMINISTIC failure is reported immediately and NOT re-run: a
			// whole-loop pass would reproduce the exact same outcome and burn the
			// wall clock. The error already names the fix. Fail hard with a clear
			// class so the run ends fast instead of retrying or hanging.
			if isDeterministicClass(err) {
				return "", fmt.Errorf("inconclusive: the model produced no answer and this is not retryable (%w)", err)
			}
			// A transport failure is NOT a reason to re-run the whole loop: the
			// failing turn already carries its own re-issues. Report the distinct
			// inconclusive class instead (the gate must never read it as a BLOCK).
			if isTimeoutClass(err) {
				return "", fmt.Errorf("inconclusive: the LLM provider never answered or stopped streaming and every re-issue of the failing turn timed out (idle bound %v); this is NOT a review verdict — re-run the gate", cfg.StreamIdleTimeout)
			}
			if pass < maxPasses {
				if err := sleepCtx(ctx, time.Duration(pass)*backoff); err != nil {
					return "", err
				}
				continue
			}
			return "", err
		}
		_, distinct, n := extractVerdict(review)
		fmt.Printf("plugin-review: pass %d review_len=%d verdict_lines=%d distinct=%v\n", pass, len(review), n, distinct)
		if n > 0 {
			return review, nil
		}
		if pass < maxPasses {
			if err := sleepCtx(ctx, time.Duration(pass)*backoff); err != nil {
				return "", err
			}
		}
	}
	return "", fmt.Errorf("all %d attempts failed to produce a Verdict line", maxPasses)
}

// chatTurn issues ONE turn request through llmkit, re-issuing it on failure with
// the SAME conversation state. Re-issuing is safe and deterministic: all review
// tools are read-only gh-api GETs and the loop is the only writer of the
// conversation, so a retried turn cannot duplicate a side effect or lose
// accumulated context. This is the ONE retry site for a failed turn.
//
// A DETERMINISTIC failure is NOT re-issued: an EmptyCompletionError with
// finish_reason=length is a budget decision — the reasoning consumed the whole
// max_tokens budget before any answer — and re-sending the identical request
// cannot change it (measured: 3 identical attempts produced 57KB/66KB/64KB of
// reasoning and no answer, wasting 2m52s). It fails HARD and immediately with a
// message naming the exact fix (raise AI_REVIEW_MAX_TOKENS), so the run ends in
// seconds with a clear cause instead of hanging or burning the wall clock.
func chatTurn(ctx context.Context, cfg reviewConfig, llm llmkit.Config, messages []llmkit.Message) (llmkit.Message, error) {
	backoff := cfg.RetryBackoff
	if backoff <= 0 {
		backoff = defaultRetryBackoff
	}
	var lastErr error
	for attempt := 1; attempt <= maxTurnRequestAttempts; attempt++ {
		msg, err := llmkit.Chat(ctx, llm, llmkit.ToSDKMessages(messages), sdkTools())
		if err == nil {
			return msg, nil
		}
		lastErr = err
		if isDeterministicClass(err) {
			// No retry: the same request yields the same outcome. Report the cause
			// and the knob that fixes it.
			return llmkit.Message{}, fmt.Errorf("the model produced no answer and retrying it is pointless (%w); raise AI_REVIEW_MAX_TOKENS (currently %d) so the reasoning budget leaves room for the answer, or lower AI_REVIEW_REASONING_EFFORT", err, cfg.MaxTokens)
		}
		fmt.Printf("plugin-review: turn request attempt %d/%d failed: %v\n", attempt, maxTurnRequestAttempts, err)
		if attempt < maxTurnRequestAttempts {
			if err := sleepCtx(ctx, time.Duration(attempt)*backoff); err != nil {
				return llmkit.Message{}, lastErr
			}
		}
	}
	return llmkit.Message{}, lastErr
}

// isDeterministicClass reports whether an error cannot be fixed by re-issuing
// the IDENTICAL request. An EmptyCompletionError is deterministic: the provider
// answered and made a budget/format decision (finish_reason), so retrying the
// same parameters reproduces it exactly.
func isDeterministicClass(err error) bool {
	var ece *llmkit.EmptyCompletionError
	return errors.As(err, &ece)
}

// sleepCtx waits for d but abandons the wait as soon as ctx is done, so a
// cancelled run (or an expired outer deadline) is never held by a backoff.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// isTimeoutClass: an attempt error belongs to the provider-unanswered class —
// no response within the whole-request deadline, no chunk within the idle
// deadline, or a peer that never wrote response headers. It is distinct from
// content or review failures, so the exhaustion error can be classified
// (RCA: the gate conflates a provider latency tail with a genuine review BLOCK).
func isTimeoutClass(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	// llmkit's TYPED stall class, plus the transport-level timeouts that reach the
	// caller unwrapped (the SDK's own net/http errors, which llmkit does not wrap
	// in a named type). Prefer the typed check: string-matching a shared library's
	// message couples this caller to wording that library is free to change.
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

// checkContextBudget is the mechanical, fail-closed PR-size limit: input tokens
// (the provider's REAL count for the turn just sent) + the output reserve must
// leave headroom inside the model's context window. When they do not, the review
// CANNOT see the whole PR in one context, so it fails HARD with an actionable
// class — never a silent truncation (which would blind the gate to a diff past
// the cut). The reserve is MaxTokens (the shared reasoning+answer budget).
func checkContextBudget(cfg reviewConfig, promptTokens int) error {
	contextTokens := cfg.ContextTokens
	if contextTokens <= 0 {
		contextTokens = defaultContextTokens
	}
	margin := cfg.ContextMarginTokens
	if margin <= 0 {
		margin = defaultContextMarginTokens
	}
	used := promptTokens + int(cfg.MaxTokens) + margin
	if used > contextTokens {
		return fmt.Errorf("inconclusive: PR too large to review in one context — the input is %d tokens and the output reserve is %d, exceeding the %d-token context window (with a %d-token margin). This is NOT a review verdict; split the PR into smaller PRs, or raise AI_REVIEW_CONTEXT_TOKENS if the model's window is larger",
			promptTokens, cfg.MaxTokens, contextTokens, margin)
	}
	return nil
}

func agentLoopOnce(ctx context.Context, cfg reviewConfig, prompt string, tools toolSet) (string, error) {
	llm := llmConfig(cfg)
	userMsg := fmt.Sprintf("Review pull request #%d in %s. Current head %s vs base %s. Use the read-only tools to verify the CURRENT state, then produce your review ending in exactly 'Verdict: PASS' or 'Verdict: BLOCK' on the final line.",
		cfg.PR, cfg.Repo, truncateStr(tools.headSHA, 12), truncateStr(tools.baseSHA, 12))
	messages := []llmkit.Message{
		{Role: "system", Content: llmkit.Strptr(prompt)},
		{Role: "user", Content: llmkit.Strptr(userMsg)},
	}
	loopStart := time.Now()
	// The debug trace exists so a long run is ATTRIBUTABLE. Measured on a real
	// 7m35s run: the whole engine emitted three aggregate lines and no per-turn
	// timing, so the time could not be assigned to a turn, a tool, or the model.
	// With AI_REVIEW_DEBUG the loop emits, per turn, the wall time, the
	// prompt/context byte size, the reasoning byte size (and the text when
	// AI_REVIEW_DEBUG_REASONING is on), the finish reason, the token usage when
	// the provider reports it, and per-tool name/arg/result byte sizes.
	dbg := func(format string, a ...any) {
		if cfg.Debug {
			fmt.Printf("plugin-review[debug]: "+format+"\n", a...)
		}
	}
	dbg("loop start — turns_max=%d tool_result_max_bytes=%d idle=%v attempt_timeout=%v reasoning_effort=%q max_tokens=%d system_prompt_bytes=%d",
		cfg.MaxTurns, cfg.ToolResultMaxBytes, cfg.StreamIdleTimeout, cfg.AttemptTimeout, cfg.ReasoningEffort, cfg.MaxTokens, len(prompt))

	for turn := 0; turn < cfg.MaxTurns; turn++ {
		contextBytes := conversationBytes(messages)
		turnStart := time.Now()
		msg, err := chatTurn(ctx, cfg, llm, messages)
		turnDur := time.Since(turnStart)
		if err != nil {
			dbg("turn %d FAILED after %v (context=%d bytes): %v", turn+1, turnDur, contextBytes, err)
			return "", err
		}
		content := ""
		if msg.Content != nil {
			content = *msg.Content
		}
		dbg("turn %d done in %v — context_in=%d bytes content=%d reasoning=%d bytes finish_reason=%q usage=%s tool_calls=%d",
			turn+1, turnDur, contextBytes, len(content), len(msg.Reasoning), msg.FinishReason, usageString(msg.Usage), len(msg.ToolCalls))
		if cfg.DebugReasoning && msg.Reasoning != "" {
			dbg("turn %d REASONING\n%s\n[plugin-review debug: end reasoning %d bytes]", turn+1, msg.Reasoning, len(msg.Reasoning))
		}
		messages = append(messages, msg)

		// PHASE D — context guard (fail-closed). The provider reports the REAL
		// prompt-token count for the turn just sent; if input + the output reserve
		// leaves no room, the review cannot be completed in one context. Fail HARD
		// with an actionable class instead of truncating (which would blind the
		// gate) or looping until the wall clock.
		if msg.Usage != nil && msg.Usage.PromptTokens > 0 {
			if err := checkContextBudget(cfg, msg.Usage.PromptTokens); err != nil {
				return "", err
			}
		}

		if len(msg.ToolCalls) == 0 {
			// PHASE C — anti-skim enforcement. A verdict is REFUSED until every
			// changed file in the get_pr_files index has been read via
			// get_pr_file. If any remain, inject a message naming them and force
			// another turn — a run cannot pass (or block) on a diff it skimmed.
			if unread := tools.unreadFiles(); len(unread) > 0 {
				fmt.Printf("plugin-review: turn %d produced a verdict but %d changed file(s) are unread — refusing it and demanding a full read\n", turn+1, len(unread))
				dbg("anti-skim: unread files %v", unread)
				messages = append(messages, llmkit.Message{Role: "user", Content: llmkit.Strptr(
					"REFUSED: you have not read every changed file. Before a verdict you MUST call get_pr_file for EACH of these unread paths from the get_pr_files index: " + strings.Join(unread, ", ") + ". Read them, then give your verdict.")})
				continue
			}
			fmt.Printf("plugin-review: turn %d: final content len=%d\n", turn+1, len(content))
			dbg("loop end — %d turn(s) in %v", turn+1, time.Since(loopStart))
			return content, nil
		}
		fmt.Printf("plugin-review: turn %d: %d tool call(s)\n", turn+1, len(msg.ToolCalls))
		for _, tc := range msg.ToolCalls {
			toolStart := time.Now()
			out, err := tools.call(ctx, tc.Name, tc.Arguments)
			toolDur := time.Since(toolStart)
			if err != nil {
				out = "{\"error\": " + jsonQuote(err.Error()) + "}"
			}
			// Each tool result is its OWN tool message and is bounded EXACTLY ONCE
			// here. A result that WOULD be truncated is a FAIL-CLOSED class, never
			// a silent skim: the gate must never review a truncated security input
			// (a diff, the body, the thread). So "the reviewed input is never
			// truncated" is true by construction — if it cannot be delivered whole,
			// the run FAILS with an actionable class instead.
			cap := cfg.ToolResultMaxBytes
			if cap <= 0 {
				cap = defaultToolResultMaxBytes
			}
			if len(out) > cap {
				return "", fmt.Errorf("inconclusive: %s returned %d bytes, above the %d-byte per-message cap — the review cannot see this input whole, and a truncated security input must never be reviewed. This is NOT a verdict; raise AI_REVIEW_TOOL_RESULT_MAX_BYTES, split the change, or reduce the item",
					tc.Name, len(out), cap)
			}
			c := out
			dbg("  tool %q args=%d bytes -> result=%d bytes (post-cap=%d) in %v", tc.Name, len(tc.Arguments), len(out), len(c), toolDur)
			messages = append(messages, llmkit.Message{Role: "tool", ToolCallID: tc.ID, Content: llmkit.Strptr(c)})
		}
	}
	// turn budget exhausted: return the last assistant content with content, if any
	dbg("loop end — turn budget exhausted after %v (MaxTurns=%d)", time.Since(loopStart), cfg.MaxTurns)
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "assistant" && messages[i].Content != nil {
			return *messages[i].Content, nil
		}
	}
	return "Conversation exceeded turn budget without a verdict.", nil
}

// conversationBytes is the total byte size of the conversation as it will be
// sent (content + tool-call arguments + reasoning), so the debug trace shows
// EXACTLY what drives per-turn cost. It is a diagnostic-only helper and never
// affects what is sent.
func conversationBytes(msgs []llmkit.Message) int {
	n := 0
	for _, m := range msgs {
		if m.Content != nil {
			n += len(*m.Content)
		}
		n += len(m.Reasoning)
		for _, tc := range m.ToolCalls {
			n += len(tc.Name) + len(tc.Arguments)
		}
	}
	return n
}

// usageString renders the provider token usage when reported, else "n/a". The
// reasoning token count is the load-bearing one for the unbounded-generation
// RCA — it is what distinguishes "the model thought for 40k tokens" from "the
// prompt was simply large".
func usageString(u *llmkit.Usage) string {
	if u == nil {
		return "n/a"
	}
	return fmt.Sprintf("prompt=%d completion=%d total=%d reasoning=%d", u.PromptTokens, u.CompletionTokens, u.TotalTokens, u.ReasoningTokens)
}

// truthyEnv is the debug-flag predicate: "1", "true", "yes", "on" (any case) are
// ON; everything else (including empty) is OFF. Using a predicate rather than a
// bare non-empty check means AI_REVIEW_DEBUG=0 reads as OFF, not ON.
func truthyEnv(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// extractVerdict: the exact line-anchored regex + distinct-union from the action.
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

func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// writeGHOutputs writes response/success/verdict to $GITHUB_OUTPUT with the
// heredoc form for multi-line values (the action's GITHUB_OUTPUT spec).
func writeGHOutputs(response string, ok bool, distinct []string) {
	p := getenvAny("GITHUB_OUTPUT")
	if p == "" {
		fmt.Println("plugin-review: GITHUB_OUTPUT not set; outputs skipped")
		return
	}
	verdict := ""
	if ok && len(distinct) == 1 {
		verdict = distinct[0]
	}
	var sb strings.Builder
	writeOutput(&sb, "response", response)
	writeOutput(&sb, "success", fmt.Sprint(ok))
	writeOutput(&sb, "verdict", verdict)
	_ = appendFile(p, []byte(sb.String()))
}

func writeOutput(sb *strings.Builder, name, value string) {
	needsDelim := strings.ContainsAny(value, "\n\r")
	if !needsDelim {
		sb.WriteString("\n" + name + "=" + value)
		return
	}
	delim := "piout_" + randAlpha(8)
	for strings.Contains(value, delim) {
		delim = "piout_" + randAlpha(8)
	}
	sb.WriteString("\n" + name + "<<" + delim + "\n" + value + "\n" + delim)
}

// appendFile appends to $GITHUB_OUTPUT (which GitHub pre-creates per step, but
// may be absent in a local run).
func appendFile(p string, b []byte) error {
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(b)
	return err
}

// randAlpha returns n lowercase-alphanumeric characters from crypto/rand. The
// delimiter only needs to be unlikely to appear in the value; crypto/rand keeps
// it unpredictable without a hand-rolled LCG.
func randAlpha(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// A delimiter collision is handled by the caller's loop; an
		// unpredictable value is preferable but not load-bearing.
		return strings.Repeat("x", n)
	}
	for i := range b {
		b[i] = letters[int(b[i])%len(letters)]
	}
	return string(b)
}

func runVerdictSelfTest() (int, error) {
	cases := map[string]bool{
		"## Review — PASS\n\nVerdict: PASS\n": true,
		"Verdict: BLOCK\n":                    true,
		"Verdict:  PASS  \n":                  true,
		"no verdict here\n":                   false,
		"Verdict: PASS\nVerdict: BLOCK\n":     false, // ambiguous
	}
	fail := 0
	for in, want := range cases {
		_, _, n := extractVerdict(in)
		ok := n == 1
		distinct := 0
		if ok {
			_, d, _ := extractVerdict(in)
			distinct = len(d)
			ok = distinct == 1
		}
		if ok != want {
			fmt.Printf("verdict self-test FAIL: %q want ok=%v got ok=%v\n", "…", want, ok)
			fail++
		}
	}
	if fail > 0 {
		return 1, fmt.Errorf("verdict self-test: %d case(s) failed", fail)
	}
	fmt.Println("verdict self-test: ok (all " + fmt.Sprint(len(cases)) + " cases)")
	return 0, nil
}
