package pluginreview

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// =============================================================================
// the BARE AGENT — send things DIRECTLY to an LLM, no harness, no runtime.
//
//   charly review bare [--system-prompt <text|@path>] [--prompt <text|@path|-|:pr>]
//                       [--tools] [--repo o/r] [--pr N] [--out P] [--self-test]
//
// Config (all env-overridable — the eval-charly contract):
//   endpoint: EVAL_LLM_BASE_URL (fallback AI_REVIEW_BASE_URL)
//   model:    EVAL_LLM_MODEL    (fallback AI_REVIEW_MODEL)
//   key:      EVAL_LLM_API_KEY  (fallback AI_REVIEW_API_KEY)
//   system:   --system-prompt / EVAL_LLM_SYSTEM_PROMPT (literal or @path) ->
//             REVIEW_PROMPT_PATH (file) -> the embedded default rulebook line.
//   prompt:   --prompt / BARE_PROMPT ('-' = stdin; ':pr' = the PR review task;
//             @path = file; otherwise literal).
// With --tools the four read-only PR tools (verb:pr) are attached as function
// tools and the agent loop runs them against --repo/--pr; without --tools it is
// a single raw completion. "All the tools needed to review a PR" = the same
// toolSet the core review uses (diff/commits/thread/meta) + the verdict +
// comment wiring, all reachable from this bare mode via the tools loop.
// =============================================================================

func (c *reviewConfig) bareSystemPrompt() string {
	// priority: EVAL_LLM_SYSTEM_PROMPT (literal or @path) > --set value > REVIEW_PROMPT_PATH > default
	if v := getenvAny("EVAL_LLM_SYSTEM_PROMPT"); v != "" {
		return resolvePromptArg(v)
	}
	if c.SystemPrompt != "" {
		return resolvePromptArg(c.SystemPrompt)
	}
	if c.PromptPath != "" {
		if raw, err := os.ReadFile(c.PromptPath); err == nil && len(raw) > 0 {
			return string(raw)
		}
	}
	return "You are a bare LLM agent. Answer the prompt directly and precisely."
}

func (c *reviewConfig) bareUserPrompt() (string, error) {
	v := c.BarePrompt
	if v == "" {
		v = getenvAny("BARE_PROMPT")
	}
	switch {
	case v == "-":
		raw, err := ioReadAllStdin()
		return strings.TrimSpace(raw), err
	case v == ":pr":
		if c.PR == 0 || c.Repo == "" {
			return "", fmt.Errorf("bare :pr prompt needs --repo owner/repo and --pr N (or PR_NUMBER/GITHUB_REPOSITORY)")
		}
		return fmt.Sprintf("Review pull request #%d in %s. Use the read-only tools to verify the CURRENT state, then produce your review.", c.PR, c.Repo), nil
	case strings.HasPrefix(v, "@"):
		raw, err := os.ReadFile(strings.TrimPrefix(v, "@"))
		if err != nil {
			return "", fmt.Errorf("bare prompt file: %w", err)
		}
		return strings.TrimSpace(string(raw)), nil
	case v == "":
		if c.PR != 0 && c.Repo != "" {
			return fmt.Sprintf("Review pull request #%d in %s.", c.PR, c.Repo), nil
		}
		return "", fmt.Errorf("bare agent needs a prompt: --prompt <text|@file|->, BARE_PROMPT, or :pr with PR context")
	}
	return v, nil
}

func resolvePromptArg(v string) string {
	if strings.HasPrefix(v, "@") {
		if raw, err := os.ReadFile(strings.TrimPrefix(v, "@")); err == nil {
			return string(raw)
		}
	}
	return v
}

func ioReadAllStdin() (string, error) {
	raw, err := os.ReadFile(filepath.Join(os.TempDir(), "charly-bare-stdin"))
	if err != nil {
		return "", fmt.Errorf("read stdin: %w", err)
	}
	return string(raw), nil
}

// runBareAgent: the mode body. With --tools it drives the PR-tools loop over the
// configurable system prompt; without, one raw completion.
func runBareAgent(ctx context.Context, cfg reviewConfig) (int, error) {
	sys := cfg.bareSystemPrompt()
	prompt, err := cfg.bareUserPrompt()
	if err != nil {
		return 2, err
	}
	llm := newLLMClient(cfg.BaseURL, cfg.APIKey, cfg.Model)
	out := ""
	if cfg.Tools {
		gh := newGHClient()
		head, base := "", ""
		if m, er := gh.toolMeta(ctx, cfg.owner(), cfg.repo(), cfg.PR); er == nil {
			head, base = m.HeadSHA, m.BaseSHA
		}
		deps := toolSet{gh: gh, owner: cfg.owner(), repo: cfg.repo(), pr: cfg.PR, headSHA: head, baseSHA: base}
		messages := []chatMsg{{Role: "system", Content: &sys}, {Role: "user", Content: &prompt}}
		for turn := 0; turn < cfg.MaxTurns; turn++ {
			msg, err := llm.chat(ctx, messages)
			if err != nil {
				return 1, err
			}
			content := ""
			if msg.Content != nil {
				content = *msg.Content
			}
			messages = append(messages, chatMsg{Role: "assistant", Content: msg.Content, ToolCalls: msg.ToolCalls})
			if len(msg.ToolCalls) == 0 {
				out = content
				break
			}
			for _, tc := range msg.ToolCalls {
				res, err := deps.call(ctx, tc.Function.Name)
				if err != nil {
					res = "{\"error\": " + jsonQuote(err.Error()) + "}"
				}
				messages = append(messages, chatMsg{Role: "tool", ToolCallID: tc.ID, Content: &res})
			}
		}
		if out == "" {
			return 1, fmt.Errorf("bare agent: no final content within %d turns", cfg.MaxTurns)
		}
	} else {
		c := prompt
		msg, err := llm.chat(ctx, []chatMsg{{Role: "system", Content: &sys}, {Role: "user", Content: &c}})
		if err != nil {
			return 1, err
		}
		if msg.Content != nil {
			out = *msg.Content
		}
	}
	if cfg.OutPath != "" {
		_ = os.WriteFile(cfg.OutPath, []byte(out), 0o644)
	}
	fmt.Println(out)
	return 0, nil
}

// hasFlag reports whether args contains flag (used by the bare self-test path).
func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}
