package pluginreview

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/openai/openai-go/v3"
	"github.com/opencharly/sdk/llmkit"
)

// tools.go — the read-only PR tools the review agent may call.
//
// The agent is PRIMED with the complete PR context in one message (body, every
// changed file's full diff, commits, every comment) so it does NOT have to
// fragment its reading across turns — that fragmentation, plus the fact that
// message reasoning is not re-sent between turns, was the measured cause of the
// runaway thinking. The tools REMAIN AVAILABLE as a verification/follow-up
// surface: if the agent wants to re-fetch a fact, confirm a count, or read
// something the priming summary elided, it can. This is the union of the two
// designs — coherent single delivery FIRST, tools as a safety net — not one
// replacing the other.
type reviewTool struct {
	name        string
	description string
	params      map[string]any
}

// reviewTools is the SINGLE declaration of the read-only tools. The name is the
// dispatch key; the description is the model-facing contract.
var reviewTools = []reviewTool{
	{name: "get_pr_meta", description: "PR metadata: title, state, head/base sha, file counts. Confirm counts here."},
	{name: "get_pr_body", description: "The CURRENT live PR body, as one unit. Authoritative."},
	{name: "get_pr_files", description: "The changed-file INDEX: path, status, additions, deletions, patch size for EVERY changed file. Call get_pr_file per path for the full patch."},
	{name: "get_pr_file", description: "Read ONE changed file's FULL unified diff by path (from the get_pr_files index), as its own message.", params: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string", "description": "A path from the get_pr_files index (exact string)."},
		},
		"required": []string{"path"},
	}},
	{name: "get_pr_commits", description: "The commit history: sha, author, message."},
	{name: "get_pr_thread", description: "The comment INDEX: id, author, date, size, preview per comment. Bodies are NOT included — call get_pr_comment by id."},
	{name: "get_pr_comment", description: "Read ONE comment by id (from the get_pr_thread index) with its full body.", params: map[string]any{
		"type":       "object",
		"properties": map[string]any{"id": map[string]any{"type": "integer", "description": "Comment id from the get_pr_thread index."}},
		"required":   []string{"id"},
	}},
}

// sdkTools renders the tools for llmkit.Chat.
func sdkTools() []openai.ChatCompletionToolUnionParam {
	out := make([]openai.ChatCompletionToolUnionParam, 0, len(reviewTools))
	for _, t := range reviewTools {
		out = append(out, llmkit.FunctionTool(t.name, t.description, t.params))
	}
	return out
}

// callTool dispatches one tool by name and returns its JSON result. A read
// failure is returned as an error (surfaced to the model as the tool result),
// never a silent empty.
func (g *ghClient) callTool(ctx context.Context, repo string, pr int, name, args string) (string, error) {
	marshal := func(v any, err error) (string, error) {
		if err != nil {
			return "", err
		}
		b, err := json.Marshal(v)
		return string(b), err
	}
	switch name {
	case "get_pr_meta":
		return marshal(g.meta(ctx, repo, pr))
	case "get_pr_body":
		body, err := g.body(ctx, repo, pr)
		return marshal(map[string]string{"body": body}, err)
	case "get_pr_files":
		return marshal(g.filesIndex(ctx, repo, pr))
	case "get_pr_file":
		var a struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal([]byte(args), &a); err != nil {
			return "", fmt.Errorf("get_pr_file: invalid arguments %q: %w", args, err)
		}
		if a.Path == "" {
			return "", fmt.Errorf("get_pr_file: a path is required (read get_pr_files for the exact paths)")
		}
		files, err := g.files(ctx, repo, pr)
		if err != nil {
			return "", err
		}
		for _, f := range files {
			if f.Path == a.Path {
				return marshal(f, nil)
			}
		}
		return "", fmt.Errorf("get_pr_file: %q is not a changed file in this PR", a.Path)
	case "get_pr_commits":
		return marshal(g.commits(ctx, repo, pr))
	case "get_pr_thread":
		return marshal(g.thread(ctx, repo, pr))
	case "get_pr_comment":
		var a struct {
			ID int `json:"id"`
		}
		if err := json.Unmarshal([]byte(args), &a); err != nil {
			return "", fmt.Errorf("get_pr_comment: invalid arguments %q: %w", args, err)
		}
		if a.ID <= 0 {
			return "", fmt.Errorf("get_pr_comment: a positive id is required (read get_pr_thread's index)")
		}
		comments, err := g.comments(ctx, repo, pr)
		if err != nil {
			return "", err
		}
		for _, c := range comments {
			if c.ID == a.ID {
				return marshal(c, nil)
			}
		}
		return "", fmt.Errorf("get_pr_comment: no comment %d in this PR", a.ID)
	}
	return "", fmt.Errorf("unknown tool %q", name)
}
