package pluginreview

import (
	"context"
	"fmt"
	"strings"

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
	Author       string
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

func (g *ghClient) body(ctx context.Context, repo string, pr int) (body string, author string, err error) {
	cli, err := g.client()
	if err != nil {
		return "", "", err
	}
	var raw struct {
		Body string `json:"body"`
		User struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if err := cli.Get(ctx, fmt.Sprintf("/repos/%s/issues/%d", repo, pr), &raw); err != nil {
		return "", "", err
	}
	author = raw.User.Login
	if author == "" {
		author = "unknown"
	}
	return raw.Body, author, nil
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

// gateNoticeHeaders are the exact first lines of the org pr-validator's
// MACHINE notices — comments the gate posts about ITSELF, not a human or a model
// posting a verdict about the code.
//
//   - "## validator INCONCLUSIVE" — the gate produced no review verdict (a
//     provider/stale-engine/runaway condition, not a code finding). Its body
//     carries a <details> Diagnostics tail: on an engine decoding collapse that
//     tail is ~65 KB of repeated "Hmm.", and the whole comment can be tens of KB.
//   - "## Auto-closed:" — the gate closed the PR after N unanswered BLOCKs. It is
//     a policy action notice, not a review finding.
//
// The marker is the deterministic key (the author is always github-actions[bot],
// but matching the body header survives a rename of the bot account).
var gateNoticeHeaders = []string{
	"## validator INCONCLUSIVE",
	"## Auto-closed:",
}

// isGateNotice reports whether body is one of the org pr-validator's machine
// notices (see gateNoticeHeaders). It matches the header on the comment's first
// NON-BLANK line, after trimming leading whitespace, so an incidental leading
// newline cannot smuggle a notice past the filter.
func isGateNotice(body string) bool {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		for _, h := range gateNoticeHeaders {
			if strings.HasPrefix(line, h) {
				return true
			}
		}
		return false // the first non-blank line is not a machine-notice header
	}
	return false
}

// filterGateNotices drops the org pr-validator's own machine notices from a
// comment slice, keeping every real contribution (see isGateNotice for WHY).
// It is pure so a test can assert the exact slice the context assembler receives.
func filterGateNotices(in []Comment) []Comment {
	out := in[:0:0] // never mutate the caller's backing array
	for _, c := range in {
		if isGateNotice(c.Body) {
			continue
		}
		out = append(out, c)
	}
	return out
}

func (g *ghClient) comments(ctx context.Context, repo string, pr int) ([]Comment, error) {
	cli, err := g.client()
	if err != nil {
		return nil, err
	}
	var out []Comment
	// Issue comments — the main conversation thread.
	cs, err := cli.PRComments(ctx, repo, pr)
	if err != nil {
		return nil, fmt.Errorf("read issue comments: %w", err)
	}
	for _, c := range cs {
		out = append(out, Comment{ID: c.ID, Kind: "issue", Author: c.Author, CreatedAt: c.CreatedAt, Body: c.Body})
	}
	// Submitted reviews — approvals / change requests / a review body.
	rs, err := cli.PRReviews(ctx, repo, pr)
	if err != nil {
		return nil, fmt.Errorf("read reviews: %w", err)
	}
	for _, r := range rs {
		out = append(out, Comment{ID: r.ID, Kind: "review:" + r.State, Author: r.Author, CreatedAt: r.SubmittedAt, Body: r.Body})
	}
	// Inline review comments — attached to a file/line in the diff.
	rcs, err := cli.PRReviewComments(ctx, repo, pr)
	if err != nil {
		return nil, fmt.Errorf("read review comments: %w", err)
	}
	for _, rc := range rcs {
		out = append(out, Comment{ID: rc.ID, Kind: "review-comment", Author: rc.Author, CreatedAt: rc.CreatedAt, Body: rc.Body})
	}
	// EXCLUDE the gate's OWN machine notices from the next review's context (see
	// isGateNotice). A "## validator INCONCLUSIVE" or "## Auto-closed:" comment
	// carries NO review finding — it is the gate reporting a provider/engine
	// condition or a policy close. Feeding it back is a feedback loop: each run
	// appends a fresh multi-KB notice (the INCONCLUSIVE <details> Diagnostics tail
	// can be ~65 KB of a degenerate "Hmm." line), the next run re-ingests all of
	// them, and the growing thread is itself what drives the model into the
	// decoding repetition collapse that produces the next INCONCLUSIVE. Dropping
	// only these two machine classes keeps the REAL review comments ("## Review —
	// BLOCK"/"PASS"), which the prompt requires be dispositioned ("Comment intake");
	// the human still sees the notices intact on the PR.
	return filterGateNotices(out), nil
}

// postComment posts ONE comment (the review).
func (g *ghClient) postComment(ctx context.Context, repo string, pr int, body string) error {
	cli, err := g.client()
	if err != nil {
		return err
	}
	return cli.PostComment(ctx, repo, pr, body)
}
