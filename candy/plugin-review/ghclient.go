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
// fixture branch, and no per-file index/indirection: the assembler reads
// everything in one pass.
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

// PRMeta is the PR identity + counts the review context renders.
type PRMeta struct {
	Title        string
	State        string
	HeadSHA      string
	BaseSHA      string
	Branch       string
	ChangedFiles int
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
		Branch: m.Head, ChangedFiles: m.FileCount,
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
	needRaw := false
	for _, f := range fs {
		// GitHub omits the per-file `patch` field when a file's diff exceeds the
		// API's per-file limit (and for some renamed/binary cases), setting
		// PRFile.NoPatch. A real (+/-) change with an empty patch must still be
		// reviewed, so recover it from the PR's raw .diff below.
		if f.NoPatch && f.Additions+f.Deletions > 0 {
			needRaw = true
		}
		out = append(out, ChangedFile{
			Path: f.Path, Status: f.Status, Additions: f.Additions, Deletions: f.Deletions, Patch: f.Patch, NoPatch: f.NoPatch,
		})
	}
	if needRaw {
		// The .diff media type always carries every file's hunk (it is not subject
		// to the per-file `patch` omission). Fill the omitted ones so the assembler
		// keeps its "every changed file's FULL diff" guarantee. A failure here is
		// non-fatal: render() marks the file explicitly rather than showing an
		// empty block.
		if raw, derr := cli.PRDiff(ctx, repo, pr); derr == nil {
			byPath := splitUnifiedDiff(raw)
			for i := range out {
				if out[i].Patch == "" && out[i].Additions+out[i].Deletions > 0 {
					if p, ok := byPath[out[i].Path]; ok && p != "" {
						out[i].Patch = p
						out[i].NoPatch = false
					}
				}
			}
		}
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
		out = append(out, Commit{SHA: c.SHA, Message: c.Message})
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
