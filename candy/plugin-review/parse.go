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

// ---- review-specific shaping helpers ----
//
// The GitHub API PARSERS (commits/meta/comment bodies) live in the canonical
// ghkit (github.com/opencharly/plugin-gh/gh) now — this file keeps only what is
// review-specific: the env helper, the UTF-8-safe truncation used for the
// per-message bound, the file-index/one-comment JSON shapes and the preview.

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

type rawIssue struct {
	Body string `json:"body"`
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

// oneComment is the shape a single fetched comment is delivered in (built from
// ghkit's typed PRComment, so the API decode lives in exactly one place).
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
