package pluginreview

import (
	"context"
	"fmt"

	ghkit "github.com/opencharly/plugin-gh/candy/plugin-gh/gh"
)

// ghclient.go — the review engine's read access to GitHub, over the org's ONE
// canonical client (ghkit, plugin-gh). ghkit owns token resolution
// (GH_TOKEN/GITHUB_TOKEN then the gh CLI hosts.yml), the API base, auth headers,
// the read bound and non-2xx status+body surfacing. This file holds ONLY the
// review's typed reads — one method per fact the assembler needs. There is no
// tool-dispatch table, no fixture branch, and no per-file index/indirection: the
// assembler reads everything in one pass.
type ghClient struct {
	c   *ghkit.Client
	err error
}

func newGHClient() *ghClient {
	c, err := ghkit.New()
	return &ghClient{c: c, err: err}
}

func (g *ghClient) client() (*ghkit.Client, error) {
	if g.err != nil {
		return nil, fmt.Errorf("github client construction failed: %w", g.err)
	}
	if g.c == nil {
		return nil, fmt.Errorf("github client is nil (not constructed)")
	}
	return g.c, nil
}

// PRMeta is the PR identity + counts. The json tags are the verb:pr WIRE shape —
// a consumer-visible contract, so they are explicit and tested.
type PRMeta struct {
	Title        string `json:"title"`
	State        string `json:"state"`
	HeadSHA      string `json:"head_sha"`
	BaseSHA      string `json:"base_sha"`
	ChangedLines int    `json:"changed_lines"`
	ChangedFiles int    `json:"changed_files"`
}

func (g *ghClient) meta(ctx context.Context, repo string, pr int) (PRMeta, error) {
	cli, err := g.client()
	if err != nil {
		return PRMeta{}, err
	}
	m, err := cli.PRMeta(ctx, repo, pr)
	if err != nil {
		return PRMeta{}, err
	}
	return PRMeta{
		Title: m.Title, State: m.State, HeadSHA: m.HeadSHA, BaseSHA: m.Base,
		ChangedLines: m.ChangedSum, ChangedFiles: m.FileCount,
	}, nil
}

func (g *ghClient) body(ctx context.Context, repo string, pr int) (string, error) {
	cli, err := g.client()
	if err != nil {
		return "", err
	}
	var raw struct {
		Body string `json:"body"`
	}
	if err := cli.Get(ctx, fmt.Sprintf("/repos/%s/issues/%d", repo, pr), &raw); err != nil {
		return "", err
	}
	return raw.Body, nil
}

func (g *ghClient) files(ctx context.Context, repo string, pr int) ([]ChangedFile, error) {
	cli, err := g.client()
	if err != nil {
		return nil, err
	}
	fs, err := cli.PRFiles(ctx, repo, pr)
	if err != nil {
		return nil, err
	}
	out := make([]ChangedFile, 0, len(fs))
	for _, f := range fs {
		out = append(out, ChangedFile{
			Path: f.Path, Status: f.Status, Additions: f.Additions, Deletions: f.Deletions, Patch: f.Patch,
		})
	}
	return out, nil
}

func (g *ghClient) commits(ctx context.Context, repo string, pr int) ([]Commit, error) {
	cli, err := g.client()
	if err != nil {
		return nil, err
	}
	cs, err := cli.PRCommits(ctx, repo, pr)
	if err != nil {
		return nil, err
	}
	out := make([]Commit, 0, len(cs))
	for _, c := range cs {
		out = append(out, Commit{SHA: c.SHA, Author: c.Author, Message: c.Message})
	}
	return out, nil
}

func (g *ghClient) comments(ctx context.Context, repo string, pr int) ([]Comment, error) {
	cli, err := g.client()
	if err != nil {
		return nil, err
	}
	cs, err := cli.PRComments(ctx, repo, pr)
	if err != nil {
		return nil, err
	}
	out := make([]Comment, 0, len(cs))
	for _, c := range cs {
		out = append(out, Comment{ID: c.ID, Author: c.Author, CreatedAt: c.CreatedAt, Body: c.Body})
	}
	return out, nil
}

// postComment posts ONE comment (the review).
func (g *ghClient) postComment(ctx context.Context, repo string, pr int, body string) error {
	cli, err := g.client()
	if err != nil {
		return err
	}
	return cli.PostComment(ctx, repo, pr, body)
}

// Thread is the comment INDEX the `pr_thread` verb and the engine's
// get_pr_thread tool BOTH return — one shape (R3). It carries per-comment
// metadata + a short preview, never the bodies: a body is read by id.
type Thread struct {
	HeadSHA      string    `json:"head_sha"`
	BaseSHA      string    `json:"base_sha"`
	Comments     []Comment `json:"comments"`
	CommentCount int       `json:"comment_count"`
}

// thread builds the index (bodies replaced by previews) for a PR.
func (g *ghClient) thread(ctx context.Context, repo string, pr int) (Thread, error) {
	cs, err := g.comments(ctx, repo, pr)
	if err != nil {
		return Thread{}, err
	}
	idx := make([]Comment, 0, len(cs))
	for _, c := range cs {
		c.Body = firstLine(c.Body, 200)
		idx = append(idx, c)
	}
	var meta PRMeta
	if m, merr := g.meta(ctx, repo, pr); merr == nil {
		meta = m
	}
	return Thread{HeadSHA: meta.HeadSHA, BaseSHA: meta.BaseSHA, Comments: idx, CommentCount: len(idx)}, nil
}
