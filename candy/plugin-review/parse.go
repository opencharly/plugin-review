package pluginreview

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"
)

// ---- small env/parse helpers shared by the engine ----

func getenvAny(names ...string) string {
	for _, n := range names {
		if v, ok := os.LookupEnv(n); ok && v != "" {
			return v
		}
	}
	return ""
}

// truncateStr truncates s to at most bytes UTF-8 bytes, avoiding a partial
// character at the cut (same behaviour as the action's truncate()).
func truncateStr(s string, bytes int) string {
	if len(s) <= bytes {
		return s
	}
	b := []byte(s)[:bytes]
	// drop a trailing partial rune
	for len(b) > 0 && !utf8Valid(b) {
		b = b[:len(b)-1]
	}
	return string(b) + "\n[…truncated…]"
}

// utf8Valid reports whether b is valid UTF-8. The previous implementation
// validated `"` + b + `"` as JSON, which is false for ANY payload containing a
// quote — i.e. for every real diff — so the rune-boundary loop above stripped
// bytes back to the last JSON-string-safe position instead of just dropping a
// partial rune (R1: divergence from the documented behaviour).
func utf8Valid(b []byte) bool { return utf8.Valid(b) }

// ---- GitHub API payload parsers ----

type rawCommit struct {
	SHA    string `json:"sha"`
	Commit struct {
		Author struct {
			Name string `json:"name"`
			Date string `json:"date"`
		} `json:"author"`
		Message string `json:"message"`
	} `json:"commit"`
	Author *struct {
		Login string `json:"login"`
	} `json:"author"`
}

// truncateToolResult bounds ONE tool result before it is appended to the
// conversation as a tool message. Tool output IS the context-growth trigger
// (a diff of up to 96 KiB plus a PR thread of up to 100 comments x 24 KiB), and
// unbounded growth is what pushed the non-streaming turn-2 request past the
// whole-response deadline (RCA: review timeouts). The HEAD is kept — the first
// hunk headers and first files are what a review cites — and the cut is
// announced, so the model knows the payload above it is incomplete.
func truncateToolResult(s string, max int) string {
	if max <= 0 {
		max = defaultToolResultMaxBytes
	}
	if len(s) <= max {
		return s
	}
	b := []byte(s)[:max]
	for len(b) > 0 && !utf8Valid(b) {
		b = b[:len(b)-1]
	}
	return string(b) + fmt.Sprintf("\n[tool result truncated by the review harness: kept %d of %d bytes — the payload above is INCOMPLETE]", len(b), len(s))
}

func parseCommits(raw string) ([]prCommit, error) {
	// Accept the plain array AND the `gh api --paginate --slurp` array-of-pages
	// shape, same as parseCommentIndex — a >100-commit PR must be complete.
	cs, err := flattenPages[rawCommit](raw)
	if err != nil {
		return nil, err
	}
	out := make([]prCommit, 0, len(cs))
	for _, c := range cs {
		author := ""
		if c.Author != nil && c.Author.Login != "" {
			author = c.Author.Login
		} else if c.Commit.Author.Name != "" {
			author = c.Commit.Author.Name
		}
		msg := strings.SplitN(c.Commit.Message, "\n", 2)[0]
		sha := c.SHA
		if len(sha) > 12 {
			sha = sha[:12]
		}
		out = append(out, prCommit{SHA: sha, Author: author, Date: c.Commit.Author.Date, Message: msg})
	}
	return out, nil
}

type rawIssue struct {
	Body string `json:"body"`
}
type rawComment struct {
	ID   int `json:"id"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
	CreatedAt string `json:"created_at"`
	Body      string `json:"body"`
}

func parseIssueBody(raw string) string {
	var i rawIssue
	if err := json.Unmarshal([]byte(raw), &i); err != nil {
		return ""
	}
	return i.Body
}

// commentPreviewBytes is how much of a comment body the INDEX carries. The index
// must stay small enough to be delivered complete in ONE tool message, so it
// carries per-comment METADATA + a short preview — never the bodies.
const commentPreviewBytes = 200

// parseCommentIndex parses the comments endpoint into the compact INDEX the
// thread tool returns: one row per comment with its id, author, date, byte size
// and a short preview. It never carries a full body, so the index is O(#comments)
// and can never be truncated — this is what fixes the aggregate-blob RCA (see
// truncateToolResult). The model reads a body by calling get_pr_comment with the
// row's id, which returns THAT comment as its own bounded message.
// parseCommentIndex parses the comments endpoint into the compact INDEX the
// thread tool returns. It accepts BOTH shapes the API can yield:
//
//   - the plain array from a single page: `[{…},{…}]`;
//   - the `gh api --paginate --slurp` array-of-pages: `[[{…}],[{…}]]`,
//
// so the caller's pagination (which is what makes a >100-comment thread
// complete) is transparent here. A page-shaped payload is flattened in order,
// preserving GitHub's ascending comment order across pages.
func parseCommentIndex(raw string) []commentMeta {
	cs, err := flattenPages[rawComment](raw)
	if err != nil {
		return nil
	}
	out := make([]commentMeta, 0, len(cs))
	for _, c := range cs {
		author := "unknown"
		if c.User.Login != "" {
			author = c.User.Login
		}
		out = append(out, commentMeta{
			ID: c.ID, Author: author, CreatedAt: c.CreatedAt,
			Bytes: len(c.Body), Preview: firstLine(c.Body, commentPreviewBytes),
		})
	}
	return out
}

// flattenPages decodes either a flat JSON array of T or the array-of-arrays shape
// `gh api --paginate --slurp` produces, returning the concatenated rows in order.
func flattenPages[T any](raw string) ([]T, error) {
	// Try the slurped (array of pages) shape first: it is what --slurp yields.
	var pages [][]T
	if err := json.Unmarshal([]byte(raw), &pages); err == nil {
		// Distinguish `[[…]]` from `[]`; a JSON object element would have failed
		// the [][]T decode already, so a successful decode with any page is the
		// slurped shape. An empty `[]` also decodes here and means no rows.
		out := make([]T, 0, len(pages))
		for _, p := range pages {
			out = append(out, p...)
		}
		return out, nil
	}
	var flat []T
	if err := json.Unmarshal([]byte(raw), &flat); err != nil {
		return nil, err
	}
	return flat, nil
}

// parseOneComment renders a SINGLE fetched comment as a self-contained JSON
// object (id + author + date + FULL body). This is the unit that travels as one
// tool message, so it is bounded exactly once, by truncateToolResult, at the
// per-message cap — never as part of a larger aggregate.
func parseOneComment(raw string) (json.RawMessage, error) {
	var c rawComment
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return nil, err
	}
	author := "unknown"
	if c.User.Login != "" {
		author = c.User.Login
	}
	return json.Marshal(oneComment{ID: c.ID, Author: author, CreatedAt: c.CreatedAt, Body: c.Body})
}

// oneComment is the shape a single fetched comment is delivered in.
type oneComment struct {
	ID        int    `json:"id"`
	Author    string `json:"author"`
	CreatedAt string `json:"created_at"`
	Body      string `json:"body"`
}

// firstLine returns at most max BYTES of s, collapsed to a single line (so a
// multi-line body yields a deterministic one-line preview), with a trailing
// ellipsis when the preview is shorter than the body.
func firstLine(s string, max int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) <= max {
		return s
	}
	b := []byte(s)[:max]
	for len(b) > 0 && !utf8Valid(b) {
		b = b[:len(b)-1]
	}
	return string(b) + "…"
}

type rawPull struct {
	Title     string `json:"title"`
	State     string `json:"state"`
	Mergeable *bool  `json:"mergeable"`
	Draft     bool   `json:"draft"`
	Head      struct {
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		SHA string `json:"sha"`
	} `json:"base"`
	Additions    int `json:"additions"`
	Deletions    int `json:"deletions"`
	ChangedFiles int `json:"changed_files"`
}

func parseMeta(raw string) (prMeta, error) {
	var p rawPull
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return prMeta{}, err
	}
	return prMeta{
		Title: p.Title, State: p.State, Mergeable: p.Mergeable, Draft: p.Draft,
		HeadSHA: p.Head.SHA, BaseSHA: p.Base.SHA,
		Additions: p.Additions, Deletions: p.Deletions, ChangedFiles: p.ChangedFiles,
	}, nil
}
