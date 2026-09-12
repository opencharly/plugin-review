package pluginreview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
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
	// defaultAttemptTimeout bounds ONE turn request as a whole (request headers
	// through the last streamed chunk). It was 5m covering a NON-streaming whole
	// generation, so the large tool-result turn 2 had to finish inside a single
	// wall clock and died with "awaiting headers" (RCA: review timeouts).
	// Streaming moved the real bound to defaultStreamIdleTimeout; this stays as
	// the generous outer cap.
	defaultAttemptTimeout = 15 * time.Minute
	// defaultStreamIdleTimeout is the maximum SILENCE inside a streamed
	// completion — the gap between chunks, including the wait for the first one
	// (prompt processing of a large tool-result context). A live generation may
	// run far longer than this; a dead one is cut here.
	defaultStreamIdleTimeout = 3 * time.Minute
	// defaultToolResultMaxBytes caps ONE tool result appended to the
	// conversation. Tool output is the context-growth trigger: get_pr_diff is up
	// to 96 KiB and get_pr_thread up to 100 comments x 24 KiB.
	defaultToolResultMaxBytes = 64 << 10
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
	// AttemptTimeout bounds ONE turn request as a whole: request headers through
	// the last streamed chunk (env AI_REVIEW_ATTEMPT_TIMEOUT, seconds).
	AttemptTimeout time.Duration
	// StreamIdleTimeout bounds SILENCE inside a streamed completion (env
	// AI_REVIEW_STREAM_IDLE_TIMEOUT, seconds).
	StreamIdleTimeout time.Duration
	// ToolResultMaxBytes caps ONE tool result before it enters the conversation
	// (env AI_REVIEW_TOOL_RESULT_MAX_BYTES).
	ToolResultMaxBytes int
	// RetryBackoff is the base backoff between re-issues of a failed turn
	// request. Not env-configurable: it is a scheduling constant, not a policy
	// knob (tests set it directly).
	RetryBackoff time.Duration
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
		RetryBackoff: defaultRetryBackoff,
		MaxAttempts:  defaultMaxAttempts,
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

func parseInt(s string) (int, error) {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("not a number: %s", s)
		}
		n = n*10 + int(r-'0')
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
	return ev.PullRequest.Number
}

// =============================================================================
// the core engine (B1–B6 of the plan) — gather → LLM tool-loop → verdict → comment
// =============================================================================

func runCoreReview(ctx context.Context, cfg reviewConfig) (int, error) {
	if cfg.PR == 0 {
		return 2, fmt.Errorf("no pull request context: pass a PR number (charly review pr <N>), the PR_NUMBER env var, or run under a pull_request event")
	}
	if cfg.Repo == "" {
		return 2, fmt.Errorf("no repository context: pass --repo owner/repo or set GITHUB_REPOSITORY")
	}
	prompt := loadPrompt(cfg.PromptPath)

	gh := newGHClient()
	headSHA, baseSHA := "", ""
	// resolve head/base from meta once (cheap) so thread + user prompt carry them
	if m, err := gh.toolMeta(ctx, cfg.owner(), cfg.repo(), cfg.PR); err == nil {
		headSHA, baseSHA = m.HeadSHA, m.BaseSHA
	}

	deps := toolSet{gh: gh, owner: cfg.owner(), repo: cfg.repo(), pr: cfg.PR, headSHA: headSHA, baseSHA: baseSHA}

	review, err := runAgentLoop(ctx, cfg, prompt, deps)
	if err != nil {
		return 1, err
	}
	_, distinct, n := extractVerdict(review)
	out := []string{review}
	if cfg.OutPath != "" {
		_ = os.WriteFile(cfg.OutPath, []byte(review), 0o644)
	}
	writeGHOutputs(review, n > 0 && len(distinct) == 1, distinct)
	fmt.Println("plugin-review: verdict_lines=" + fmt.Sprint(n) + " distinct=" + fmt.Sprint(distinct))
	if n > 0 && len(distinct) != 1 {
		return 2, fmt.Errorf("ambiguous verdict: multiple distinct Verdict lines: %v", distinct)
	}
	// ONE best-effort comment with a run footer (never fails the run)
	runURL := cfg.ServerURL + "/" + cfg.RepoEnv + "/actions/runs/" + cfg.RunID
	footer := fmt.Sprintf("\n\n---\n%s/%s — action-review.\n\n[View action run](%s)", cfg.Provider, cfg.Model, runURL)
	if err := gh.postComment(ctx, cfg.owner(), cfg.repo(), cfg.PR, strings.Join(out, "")+footer); err != nil {
		fmt.Println("plugin-review: comment post failed (non-fatal): " + err.Error())
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

// toolSet wires the four tools to the gh client + fixtures.
type toolSet struct {
	gh      *ghClient
	owner   string
	repo    string
	pr      int
	headSHA string
	baseSHA string
	fixture string // non-empty → offline mode
}

func (t *toolSet) call(ctx context.Context, name string) (string, error) {
	fx := ""
	if t.fixture != "" {
		fx = filepath.Join("fixtures", t.fixture+"-"+name+".json")
	}
	switch name {
	case "get_pr_diff":
		if fx != "" {
			return readFixture(fx)
		}
		return t.gh.toolDiff(ctx, t.owner, t.repo, t.pr)
	case "get_pr_commits":
		if fx != "" {
			return readFixture(fx)
		}
		cs, err := t.gh.toolCommits(ctx, t.owner, t.repo, t.pr)
		if err != nil {
			return "", err
		}
		b, _ := json.Marshal(cs)
		return string(b), nil
	case "get_pr_thread":
		if fx != "" {
			return readFixture(fx)
		}
		th, err := t.gh.toolThread(ctx, t.owner, t.repo, t.pr, t.headSHA, t.baseSHA)
		if err != nil {
			return "", err
		}
		b, _ := json.Marshal(th)
		return string(b), nil
	case "get_pr_meta":
		if fx != "" {
			return readFixture(fx)
		}
		m, err := t.gh.toolMeta(ctx, t.owner, t.repo, t.pr)
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

// runAgentLoop: temperature 0.2, tool_choice auto, max_turns. The retry policy
// lives at the layer that actually fails: a FAILED TURN REQUEST is re-issued
// (chatTurn, up to maxTurnRequestAttempts, same conversation state), and only a
// COMPLETED pass that produced no Verdict line re-runs the loop
// (cfg.MaxAttempts, env AI_REVIEW_MAX_ATTEMPTS, default 3; 1 = fail hard, no
// repeat cycle). A turn-2 timeout therefore resumes at turn 2 — it never
// restarts the review from turn 1 (pre-RCA behaviour re-ran the WHOLE loop per
// attempt, redoing every already-paid turn).
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
			// A transport failure is NOT a reason to re-run the whole loop: every
			// turn request already carries its own re-issues. Report the distinct
			// inconclusive class instead (the gate must never read it as a BLOCK).
			if isTimeoutClass(err) {
				return "", fmt.Errorf("inconclusive: the LLM provider never answered or stopped streaming and every re-issue of the failing turn timed out (idle bound %v, whole-request bound %v); this is NOT a review verdict — re-run the gate", cfg.StreamIdleTimeout, cfg.AttemptTimeout)
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

// chatTurn issues ONE turn request, re-issuing it on failure with the SAME
// conversation state. Re-issuing is safe and deterministic: all four review
// tools are read-only gh-api GETs (get_pr_diff / get_pr_commits / get_pr_thread
// / get_pr_meta) and the loop is the only writer of `messages`, so a retried
// turn cannot duplicate a side effect or lose accumulated context.
func chatTurn(ctx context.Context, cfg reviewConfig, llm *llmClient, messages []chatMsg) (chatMsg, error) {
	backoff := cfg.RetryBackoff
	if backoff <= 0 {
		backoff = defaultRetryBackoff
	}
	var lastErr error
	for attempt := 1; attempt <= maxTurnRequestAttempts; attempt++ {
		msg, err := llm.chat(ctx, messages)
		if err == nil {
			return msg, nil
		}
		lastErr = err
		fmt.Printf("plugin-review: turn request attempt %d/%d failed: %v\n", attempt, maxTurnRequestAttempts, err)
		if attempt < maxTurnRequestAttempts {
			if err := sleepCtx(ctx, time.Duration(attempt)*backoff); err != nil {
				return chatMsg{}, lastErr
			}
		}
	}
	return chatMsg{}, lastErr
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
	msg := err.Error()
	return strings.Contains(msg, "context deadline exceeded") ||
		strings.Contains(msg, "Client.Timeout exceeded") ||
		strings.Contains(msg, "timeout awaiting response headers") ||
		strings.Contains(msg, "i/o timeout")
}

func agentLoopOnce(ctx context.Context, cfg reviewConfig, prompt string, tools toolSet) (string, error) {
	llm := newLLMClient(cfg)
	sys := prompt
	userMsg := fmt.Sprintf("Review pull request #%d in %s. Current head %s vs base %s. Use the read-only tools to verify the CURRENT state, then produce your review ending in exactly 'Verdict: PASS' or 'Verdict: BLOCK' on the final line.",
		cfg.PR, cfg.Repo, truncateStr(tools.headSHA, 12), truncateStr(tools.baseSHA, 12))
	messages := []chatMsg{{Role: "system", Content: &sys}, {Role: "user", Content: &userMsg}}

	for turn := 0; turn < cfg.MaxTurns; turn++ {
		msg, err := chatTurn(ctx, cfg, llm, messages)
		if err != nil {
			return "", err
		}
		content := ""
		if msg.Content != nil {
			content = *msg.Content
		}
		assistant := chatMsg{Role: "assistant", Content: msg.Content, ToolCalls: msg.ToolCalls}
		messages = append(messages, assistant)
		if len(msg.ToolCalls) == 0 {
			fmt.Printf("plugin-review: turn %d: final content len=%d\n", turn+1, len(content))
			return content, nil
		}
		fmt.Printf("plugin-review: turn %d: %d tool call(s)\n", turn+1, len(msg.ToolCalls))
		for _, tc := range msg.ToolCalls {
			out, err := tools.call(ctx, tc.Function.Name)
			if err != nil {
				out = "{\"error\": " + jsonQuote(err.Error()) + "}"
			}
			// Bound the payload the model will see on the NEXT turn: tool output is
			// the context-growth trigger, and unbounded growth is what pushed the
			// non-streaming turn-2 request past the whole-response deadline.
			c := truncateToolResult(out, cfg.ToolResultMaxBytes)
			messages = append(messages, chatMsg{Role: "tool", ToolCallID: tc.ID, Content: &c})
		}
	}
	// turn budget exhausted: return the last assistant content with content, if any
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "assistant" && messages[i].Content != nil {
			return *messages[i].Content, nil
		}
	}
	return "Conversation exceeded turn budget without a verdict.", nil
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
	sortStrings(distinct)
	return found, distinct, len(found)
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
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

func appendFile(p string, b []byte) error {
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(b)
	return err
}

func randAlpha(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	seed := time.Now().UnixNano()
	for i := range b {
		seed = seed*6364136223846793005 + 1442695040888963407
		b[i] = letters[uint64(seed>>33)%uint64(len(letters))]
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
