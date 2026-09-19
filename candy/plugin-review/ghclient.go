package pluginreview

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// ghClient is a thin wrapper around the gh CLI (the org's layer-gh candy installs
// it; ubuntu-latest ships it). Zero dependencies beyond the charly SDK — the gh
// binary is a declared runtime dependency, resolved from PATH at call time so
// tests can inject a stub.
type ghClient struct {
	token string // GITHUB_TOKEN / GH_TOKEN override (default: gh's own auth)
}

func newGHClient() *ghClient {
	c := &ghClient{}
	if t := getenvAny("GITHUB_TOKEN", "GH_TOKEN"); t != "" {
		c.token = t
	}
	return c
}

// api runs `gh api <args...>` and returns stdout (raw body). gh api already
// authenticates (env GITHUB_TOKEN or gh's stored creds); our token is passed as
// GITHUB_TOKEN so a workflow-injected github.token flows through without gh
// auth setup.
func (c *ghClient) api(ctx context.Context, args ...string) (string, error) {
	full := append([]string{"api"}, args...)
	var cmd *exec.Cmd
	if c.token != "" {
		cmd = exec.Command("gh", full...)
		cmd.Env = append(envWithToken(c.token), "NO_COLOR=1")
	} else {
		cmd = exec.Command("gh", full...)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("gh api %s: %w: %s", strings.Join(args, " "), err, truncateStr(msg, 240))
	}
	return stdout.String(), nil
}

func envWithToken(token string) []string {
	base := []string{}
	for _, kv := range []string{} {
		_ = kv
	}
	// Rebuild a minimal env: keep PATH/HTTP_PROXY style vars, inject GITHUB_TOKEN.
	keep := []string{"PATH", "HOME", "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy"}
	for _, k := range keep {
		v, ok := os.LookupEnv(k)
		if ok {
			base = append(base, k+"="+v)
		}
	}
	return append(base, "GITHUB_TOKEN="+token, "GH_TOKEN="+token)
}

// ---- the four read-only tools (port of pi-review-action's index.js tools) ----

// toolDiff: CURRENT unified diff (head vs base), truncated to 96 KiB — same as the
// action's truncate().
func (c *ghClient) toolDiff(ctx context.Context, owner, repo string, pr int) (string, error) {
	raw, err := c.api(ctx,
		"-H", "Accept: application/vnd.github.diff",
		"-H", "X-GitHub-Api-Version: 2022-11-28",
		fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, repo, pr))
	if err != nil {
		return "", err
	}
	return truncateStr(raw, 96*1024), nil
}

type prCommit struct {
	SHA     string `json:"sha"`
	Author  string `json:"author"`
	Date    string `json:"date"`
	Message string `json:"message"`
}

// toolCommits: PR commit history (sha, message, author) — same shape as the action.
func (c *ghClient) toolCommits(ctx context.Context, owner, repo string, pr int) ([]prCommit, error) {
	raw, err := c.api(ctx, fmt.Sprintf("/repos/%s/%s/pulls/%d/commits?per_page=100", owner, repo, pr))
	if err != nil {
		return nil, err
	}
	return parseCommits(raw)
}

// commentMeta is the compact INDEX row for one comment: metadata + a short
// preview, never the body. The index therefore stays small and is delivered
// complete; a body is fetched on demand by id (get_pr_comment).
type commentMeta struct {
	ID        int    `json:"id"`
	Author    string `json:"author"`
	CreatedAt string `json:"created_at"`
	Bytes     int    `json:"bytes"`
	Preview   string `json:"preview"`
}

// prThread is the comment INDEX — one compact row per comment. It deliberately
// carries NO bodies: the PR body is get_pr_body and each comment body is
// get_pr_comment, so no single tool message can lose its tail to the per-message
// cap (RCA: the prior aggregate body+all-comments blob exceeded the cap and its
// tail — the trailing review round and the maintainer sign-off — was
// structurally invisible to the validator).
type prThread struct {
	HeadSHA      string        `json:"head_sha"`
	BaseSHA      string        `json:"base_sha"`
	Comments     []commentMeta `json:"comments"`
	CommentCount int           `json:"comment_count"`
	// MaxCommentBytes states the per-comment body cap so the model knows how much
	// of a large comment get_pr_comment will return.
	MaxCommentBytes int `json:"max_comment_bytes"`
}

// prBody is the PR description as its OWN message (bounded once at the cap).
type prBody struct {
	BodyIsAuthoritative bool   `json:"body_is_authoritative"`
	Bytes               int    `json:"bytes"`
	Body                string `json:"body"`
}

// toolThread returns the comment INDEX only (ids + metadata + preview), plus the
// per-comment byte cap. The body is NOT here — see toolBody.
func (c *ghClient) toolThread(ctx context.Context, owner, repo string, pr int, headSHA, baseSHA string, maxCommentBytes int) (prThread, error) {
	index := []commentMeta{}
	if rawCs, err := c.api(ctx, fmt.Sprintf("/repos/%s/%s/issues/%d/comments?per_page=100", owner, repo, pr)); err == nil {
		index = parseCommentIndex(rawCs)
	}
	return prThread{
		HeadSHA: headSHA, BaseSHA: baseSHA,
		Comments: index, CommentCount: len(index), MaxCommentBytes: maxCommentBytes,
	}, nil
}

// toolBody returns the CURRENT live issue/PR body as its own result. The body is
// authoritative (the CURRENT body supersedes anything an older comment said).
func (c *ghClient) toolBody(ctx context.Context, owner, repo string, pr int) (prBody, error) {
	raw, err := c.api(ctx, fmt.Sprintf("/repos/%s/%s/issues/%d", owner, repo, pr))
	if err != nil {
		return prBody{}, err
	}
	body := parseIssueBody(raw)
	return prBody{BodyIsAuthoritative: true, Bytes: len(body), Body: body}, nil
}

// toolComment fetches ONE comment by its GitHub comment id and renders it as a
// self-contained object (id + author + date + FULL body). It is the read path
// that makes the thread index lossless: each comment travels as its own message
// and is bounded exactly once, at the per-message cap.
func (c *ghClient) toolComment(ctx context.Context, owner, repo string, id int) (json.RawMessage, error) {
	raw, err := c.api(ctx, fmt.Sprintf("/repos/%s/%s/issues/comments/%d", owner, repo, id))
	if err != nil {
		return nil, err
	}
	return parseOneComment(raw)
}

type prMeta struct {
	Title        string `json:"title"`
	State        string `json:"state"`
	Mergeable    *bool  `json:"mergeable"`
	Draft        bool   `json:"draft"`
	HeadSHA      string `json:"head_sha"`
	BaseSHA      string `json:"base_sha"`
	Additions    int    `json:"additions"`
	Deletions    int    `json:"deletions"`
	ChangedFiles int    `json:"changed_files"`
}

// toolMeta: PR metadata — same shape as the action.
func (c *ghClient) toolMeta(ctx context.Context, owner, repo string, pr int) (prMeta, error) {
	raw, err := c.api(ctx, fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, repo, pr))
	if err != nil {
		return prMeta{}, err
	}
	return parseMeta(raw)
}

// postComment: ONE PR comment (best-effort, never fails the run).
func (c *ghClient) postComment(ctx context.Context, owner, repo string, pr int, body string) error {
	_, err := c.api(ctx, "--method", "POST",
		"-H", "Accept: application/vnd.github+json",
		"-H", "X-GitHub-Api-Version: 2022-11-28",
		"-f", "body="+body,
		fmt.Sprintf("/repos/%s/%s/issues/%d/comments", owner, repo, pr))
	return err
}
