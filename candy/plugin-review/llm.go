package pluginreview

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ---- chat-completions tool-loop client (the 1:1 port of pi-review-action) ----

type toolSchema struct {
	Type     string         `json:"type"`
	Function functionSchema `json:"function"`
}
type functionSchema struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

var emptyParams = json.RawMessage(`{"type":"object","properties":{}}`)

// the SAME four read-only tools the action exported — identical names/descriptions.
var reviewTools = []toolSchema{
	{Type: "function", Function: functionSchema{Name: "get_pr_diff", Description: "CURRENT unified diff (head vs base).", Parameters: emptyParams}},
	{Type: "function", Function: functionSchema{Name: "get_pr_commits", Description: "Commit history of this PR (sha, message, author) — read commit messages since the last review here.", Parameters: emptyParams}},
	{Type: "function", Function: functionSchema{Name: "get_pr_thread", Description: "CURRENT live issue body plus all prior comments (older comments are stale until re-verified).", Parameters: emptyParams}},
	{Type: "function", Function: functionSchema{Name: "get_pr_meta", Description: "PR metadata: title, state, mergeable, head/base sha, file counts.", Parameters: emptyParams}},
}

type chatMsg struct {
	Role       string     `json:"role"`
	Content    *string    `json:"content,omitempty"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}
type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}
type chatRequest struct {
	Model       string       `json:"model"`
	Messages    []chatMsg    `json:"messages"`
	Temperature float64      `json:"temperature"`
	Tools       []toolSchema `json:"tools"`
	ToolChoice  string       `json:"tool_choice"`
}
type chatResponse struct {
	Choices []struct {
		Message struct {
			Content   *string    `json:"content"`
			ToolCalls []toolCall `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
}

type llmClient struct {
	baseURL string
	apiKey  string
	model   string
	http    *http.Client
}

func newLLMClient(baseURL, apiKey, model string, timeout time.Duration) *llmClient {
	if timeout <= 0 {
		timeout = defaultAttemptTimeout
	}
	return &llmClient{
		baseURL: trimTrailingSlash(baseURL),
		apiKey:  apiKey,
		model:   model,
		http:    &http.Client{Timeout: timeout},
	}
}

func trimTrailingSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

// chat posts one completion step; returns the assistant message.
func (c *llmClient) chat(ctx context.Context, messages []chatMsg) (chatMsg, error) {
	body, err := json.Marshal(chatRequest{
		Model: c.model, Messages: messages, Temperature: 0.2,
		Tools: reviewTools, ToolChoice: "auto",
	})
	if err != nil {
		return chatMsg{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return chatMsg{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("HTTP-Referer", "https://github.com/opencharly/action-review")
	req.Header.Set("X-Title", "action-review")

	resp, err := c.http.Do(req)
	if err != nil {
		return chatMsg{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return chatMsg{}, fmt.Errorf("LLM %d: %s", resp.StatusCode, truncateStr(string(raw), 300))
	}
	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return chatMsg{}, fmt.Errorf("LLM response decode: %w", err)
	}
	if len(cr.Choices) == 0 {
		return chatMsg{}, fmt.Errorf("LLM response had no choices")
	}
	m := cr.Choices[0].Message
	return chatMsg{Role: "assistant", Content: m.Content, ToolCalls: m.ToolCalls}, nil
}
