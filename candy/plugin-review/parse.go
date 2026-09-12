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

func lookupEnv(k string) (string, bool) { return os.LookupEnv(k) }

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
	var cs []rawCommit
	if err := json.Unmarshal([]byte(raw), &cs); err != nil {
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

func parseComments(raw string) []prComment {
	var cs []rawComment
	if err := json.Unmarshal([]byte(raw), &cs); err != nil {
		return nil
	}
	out := make([]prComment, 0, len(cs))
	for _, c := range cs {
		author := "unknown"
		if c.User.Login != "" {
			author = c.User.Login
		}
		out = append(out, prComment{ID: c.ID, Author: author, CreatedAt: c.CreatedAt, Body: truncateStr(c.Body, 24*1024)})
	}
	return out
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
