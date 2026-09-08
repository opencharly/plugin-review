package pluginreview

import (
	"context"
	"encoding/json"
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
)

type reviewConfig struct {
	PR           int
	Repo         string // owner/repo
	Provider     string
	Model        string
	BaseURL      string
	APIKey       string
	MaxTurns     int
	PromptPath   string
	OutPath      string
	PlanPath     string
	SystemPrompt string
	BarePrompt   string
	Tools        bool
	ServerURL    string
	RepoEnv      string // GITHUB_REPOSITORY fallback
	RunID        string
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
	case "bare", "self-test-bare":
		if mode == "self-test-bare" || hasFlag(args, "--self-test") {
			fmt.Printf("bare agent self-test: ok — base_url=%s model=%s key_set=%v system_prompt_len=%d\n",
				cfg.BaseURL, cfg.Model, cfg.APIKey != "", len(cfg.bareSystemPrompt()))
			return 0, nil
		}
		return runBareAgent(context.Background(), cfg)
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
		MaxTurns:  defaultMaxTurns,
		ServerURL: getenvAny("GITHUB_SERVER_URL"),
		RepoEnv:   getenvAny("GITHUB_REPOSITORY"),
		RunID:     getenvAny("GITHUB_RUN_ID"),
	}
	if cfg.ServerURL == "" {
		cfg.ServerURL = "https://github.com"
	}

	// env overrides (AI_REVIEW_*: provider/model/base_url/max_turns; REVIEW_PROMPT_PATH; REVIEW_PLAN_PATH)
	if v := getenvAny("AI_REVIEW_PROVIDER"); v != "" {
		cfg.Provider = v
	}
	if v := getenvAny("AI_REVIEW_MODEL"); v != "" {
		cfg.Model = v
	}
	if v := getenvAny("AI_REVIEW_BASE_URL"); v != "" {
		cfg.BaseURL = v
	}
	if v := getenvAny("AI_REVIEW_MAX_TURNS"); v != "" {
		if n, e := parseInt(v); e == nil && n > 0 {
			cfg.MaxTurns = n
		}
	}
	cfg.PromptPath = getenvAny("REVIEW_PROMPT_PATH")
	cfg.PlanPath = getenvAny("REVIEW_PLAN_PATH")
	cfg.APIKey = getenvAny("AI_REVIEW_API_KEY")
	// EVAL_LLM_* (the eval-charly contract) win over AI_REVIEW_* for the bare agent.
	if v := getenvAny("EVAL_LLM_BASE_URL"); v != "" {
		cfg.BaseURL = v
	}
	if v := getenvAny("EVAL_LLM_MODEL"); v != "" {
		cfg.Model = v
	}
	if v := getenvAny("EVAL_LLM_API_KEY"); v != "" {
		cfg.APIKey = v
	}

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
		case a == "bare":
			mode = "bare"
		case a == "--system-prompt":
			if i+1 < len(args) {
				cfg.SystemPrompt = args[i+1]
				i++
			}
		case a == "--prompt":
			if i+1 < len(args) {
				cfg.BarePrompt = args[i+1]
				i++
			}
		case a == "--tools":
			cfg.Tools = true
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

// runAgentLoop: temperature 0.2, tool_choice auto, max_turns, 3 attempts with
// 5s/10s backoff, verdict-less retry — the action's semantics.
func runAgentLoop(ctx context.Context, cfg reviewConfig, prompt string, tools toolSet) (string, error) {
	const maxAttempts = 3
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		fmt.Printf("plugin-review: attempt %d/%d — provider=%s model=%s base_url=%s\n", attempt, maxAttempts, cfg.Provider, cfg.Model, cfg.BaseURL)
		review, err := agentLoopOnce(ctx, cfg, prompt, tools)
		if err != nil {
			fmt.Println("plugin-review: attempt " + fmt.Sprint(attempt) + " failed: " + err.Error())
			if attempt < maxAttempts {
				time.Sleep(time.Duration(attempt) * 5 * time.Second)
			}
			continue
		}
		_, distinct, n := extractVerdict(review)
		fmt.Printf("plugin-review: attempt %d review_len=%d verdict_lines=%d distinct=%v\n", attempt, len(review), n, distinct)
		if n > 0 {
			return review, nil
		}
	}
	return "", fmt.Errorf("all %d attempts failed to produce a Verdict line", maxAttempts)
}

func agentLoopOnce(ctx context.Context, cfg reviewConfig, prompt string, tools toolSet) (string, error) {
	llm := newLLMClient(cfg.BaseURL, cfg.APIKey, cfg.Model)
	sys := prompt
	userMsg := fmt.Sprintf("Review pull request #%d in %s. Current head %s vs base %s. Use the read-only tools to verify the CURRENT state, then produce your review ending in exactly 'Verdict: PASS' or 'Verdict: BLOCK' on the final line.",
		cfg.PR, cfg.Repo, truncateStr(tools.headSHA, 12), truncateStr(tools.baseSHA, 12))
	messages := []chatMsg{{Role: "system", Content: &sys}, {Role: "user", Content: &userMsg}}

	for turn := 0; turn < cfg.MaxTurns; turn++ {
		msg, err := llm.chat(ctx, messages)
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
			c := out
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
