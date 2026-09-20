package pluginreview

import (
	"context"
	"encoding/json"
	"fmt"

	ghkit "github.com/opencharly/plugin-gh/candy/plugin-gh/gh"
)

// ghClient adapts the org's CANONICAL GitHub client (ghkit, plugin-gh) to the
// review engine's needs. There is exactly ONE GitHub client in the org (R3):
// the hand-rolled `exec.Command("gh")` wrapper is DELETED. ghkit owns token
// resolution (GH_TOKEN/GITHUB_TOKEN then the gh CLI's hosts.yml), the API base,
// auth headers, the read bound and the non-2xx status+body surfacing, so this
// file holds only the review-specific shaping.
type ghClient struct {
	c   *ghkit.Client
	err error
}

// newGHClient builds the canonical client. A missing token is NOT fatal: public
// reads work unauthenticated, and the auth failure surfaces at the call with
// the HTTP status + body (never as a speculative abort). A CONSTRUCTION error,
// however, is stored and surfaced at the first use — never discarded (a nil
// dereference later is the class this engine already fixed once).
func newGHClient() *ghClient {
	c, err := ghkit.New()
	return &ghClient{c: c, err: err}
}

// client returns the underlying client or the construction error, so a failed
// build is a clear error, never a nil dereference.
func (g *ghClient) client() (*ghkit.Client, error) {
	if g.err != nil {
		return nil, fmt.Errorf("github client construction failed: %w", g.err)
	}
	if g.c == nil {
		return nil, fmt.Errorf("github client is nil (not constructed)")
	}
	return g.c, nil
}

// ---- the read-only tools (the review gate's own shaping over ghkit) ----

// prFileMeta is one row of the CHANGED-FILE INDEX: metadata + patch size, never
// the patch text. The index stays small and is delivered complete; a file's
// patch is fetched on demand by path (get_pr_file), so a multi-file diff is
// delivered ONE FILE PER MESSAGE — the shape that keeps a reasoning model from
// spiralling on a monolithic multi-file diff (measured: 507-741 KB of reasoning
// on one consolidated diff vs 580 B delivered per file).
type prFileMeta struct {
	Path      string `json:"path"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	PatchSize int    `json:"patch_bytes"`
	// NoPatch: the API returned no patch text (a binary file, a rename with no
	// content change, or a file too large to render). The caller fetches nothing.
	NoPatch bool `json:"no_patch,omitempty"`
}

// prFileIndex is the result of get_pr_files.
type prFileIndex struct {
	Files []prFileMeta `json:"files"`
	// Count is len(Files) stated explicitly so a consumer can check it read the
	// whole index (a truncated list would show a smaller count).
	Count int `json:"file_count"`
	// TotalPatchBytes is the summed patch size of every changed file, so the
	// model (and the context guard) can see the review's size up front.
	TotalPatchBytes int `json:"total_patch_bytes"`
}

// prFile is ONE file's full patch, delivered as its own message.
type prFile struct {
	Path      string `json:"path"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Patch     string `json:"patch"`
}

// toolFiles returns the changed-file INDEX (no patches). A file's patch is read
// by toolFile.
func (c *ghClient) toolFiles(ctx context.Context, repo string, pr int) (prFileIndex, error) {
	cli, err := c.client()
	if err != nil {
		return prFileIndex{}, err
	}
	files, err := cli.PRFiles(ctx, repo, pr)
	if err != nil {
		return prFileIndex{}, err
	}
	out := prFileIndex{Files: make([]prFileMeta, 0, len(files))}
	for _, f := range files {
		out.Files = append(out.Files, prFileMeta{
			Path: f.Path, Status: f.Status, Additions: f.Additions, Deletions: f.Deletions,
			PatchSize: len(f.Patch), NoPatch: f.NoPatch,
		})
		out.TotalPatchBytes += len(f.Patch)
	}
	out.Count = len(out.Files)
	return out, nil
}

// toolFile returns ONE changed file's FULL patch (never truncated here — the
// per-message cap is applied exactly once, in the loop). A path NOT in the PR's
// changed set is a clear error, so the model cannot invent files.
func (c *ghClient) toolFile(ctx context.Context, repo string, pr int, path string) (prFile, error) {
	cli, err := c.client()
	if err != nil {
		return prFile{}, err
	}
	files, err := cli.PRFiles(ctx, repo, pr)
	if err != nil {
		return prFile{}, err
	}
	for _, f := range files {
		if f.Path == path {
			return prFile{Path: f.Path, Status: f.Status, Additions: f.Additions, Deletions: f.Deletions, Patch: f.Patch}, nil
		}
	}
	return prFile{}, fmt.Errorf("get_pr_file: %q is not a changed file in this PR (read the get_pr_files index for the exact paths)", path)
}

type prCommit struct {
	SHA     string `json:"sha"`
	Author  string `json:"author"`
	Date    string `json:"date"`
	Message string `json:"message"`
}

// toolCommits: PR commit history (sha, message, author).
func (c *ghClient) toolCommits(ctx context.Context, repo string, pr int) ([]prCommit, error) {
	cli, err := c.client()
	if err != nil {
		return nil, err
	}
	cs, err := cli.PRCommits(ctx, repo, pr)
	if err != nil {
		return nil, err
	}
	out := make([]prCommit, 0, len(cs))
	for _, x := range cs {
		out = append(out, prCommit{SHA: x.SHA, Author: x.Author, Message: x.Message})
	}
	return out, nil
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
// cap.
type prThread struct {
	HeadSHA         string        `json:"head_sha"`
	BaseSHA         string        `json:"base_sha"`
	Comments        []commentMeta `json:"comments"`
	CommentCount    int           `json:"comment_count"`
	MaxCommentBytes int           `json:"max_comment_bytes"`
}

// prBody is the PR description as its OWN message (bounded once at the cap).
type prBody struct {
	BodyIsAuthoritative bool   `json:"body_is_authoritative"`
	Bytes               int    `json:"bytes"`
	Body                string `json:"body"`
}

// toolThread returns the comment INDEX only (ids + metadata + preview), plus the
// per-comment byte cap. The body is NOT here — see toolBody.
func (c *ghClient) toolThread(ctx context.Context, repo string, pr int, headSHA, baseSHA string, maxCommentBytes int) (prThread, error) {
	cli, err := c.client()
	if err != nil {
		return prThread{}, err
	}
	comments, err := cli.PRComments(ctx, repo, pr)
	if err != nil {
		return prThread{}, err
	}
	index := make([]commentMeta, 0, len(comments))
	for _, cm := range comments {
		index = append(index, commentMeta{
			ID: cm.ID, Author: cm.Author, CreatedAt: cm.CreatedAt,
			Bytes: len(cm.Body), Preview: firstLine(cm.Body, commentPreviewBytes),
		})
	}
	return prThread{
		HeadSHA: headSHA, BaseSHA: baseSHA,
		Comments: index, CommentCount: len(index), MaxCommentBytes: maxCommentBytes,
	}, nil
}

// toolBody returns the CURRENT live issue/PR body as its own result. ghkit.Get
// decodes the typed body, so the JSON shape lives in one place.
func (c *ghClient) toolBody(ctx context.Context, repo string, pr int) (prBody, error) {
	cli, err := c.client()
	if err != nil {
		return prBody{}, err
	}
	var raw struct {
		Body string `json:"body"`
	}
	if err := cli.Get(ctx, fmt.Sprintf("/repos/%s/issues/%d", repo, pr), &raw); err != nil {
		return prBody{}, err
	}
	return prBody{BodyIsAuthoritative: true, Bytes: len(raw.Body), Body: raw.Body}, nil
}

// toolComment fetches ONE comment by its GitHub comment id.
func (c *ghClient) toolComment(ctx context.Context, repo string, id int) (json.RawMessage, error) {
	cli, err := c.client()
	if err != nil {
		return nil, err
	}
	cm, err := cli.PRComment(ctx, repo, id)
	if err != nil {
		return nil, err
	}
	return json.Marshal(oneComment{ID: cm.ID, Author: cm.Author, CreatedAt: cm.CreatedAt, Body: cm.Body})
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

// toolMeta: PR metadata.
func (c *ghClient) toolMeta(ctx context.Context, repo string, pr int) (prMeta, error) {
	cli, err := c.client()
	if err != nil {
		return prMeta{}, err
	}
	m, err := cli.PRMeta(ctx, repo, pr)
	if err != nil {
		return prMeta{}, err
	}
	return prMeta{Title: m.Title, State: m.State, Mergeable: m.Mergeable, Draft: m.Draft, HeadSHA: m.HeadSHA, BaseSHA: m.Base, ChangedFiles: m.FileCount}, nil
}

// postComment: ONE PR comment via the canonical client. A failure is a real
// error (surfaced by the caller as non-fatal), never a silent drop.
func (c *ghClient) postComment(ctx context.Context, repo string, pr int, body string) error {
	cli, err := c.client()
	if err != nil {
		return err
	}
	return cli.PostComment(ctx, repo, pr, body)
}
