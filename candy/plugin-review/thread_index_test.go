package pluginreview

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// thread_index_test.go — the RCA fix for the 64 KiB aggregate thread cap.
//
// The defect: get_pr_thread concatenated the PR body + EVERY comment body into
// one JSON blob, which truncateToolResult then cut at the per-message cap. The
// tail of a long thread — the newest review round and the maintainer's sign-off —
// was STRUCTURALLY invisible to the validator (measured: a 119 KB thread kept
// 65,536 of 113,650 bytes).
//
// The fix: body, index and each comment travel as SEPARATE tool messages. The
// index is O(#comments) metadata and is therefore never truncated; a body is
// fetched by id and bounded ONCE, at the cap.

// bigComment builds a comment whose body is large enough that the OLD aggregate
// design would have pushed the thread past the per-message cap.
func bigComment(id int, bodyBytes int) map[string]any {
	return map[string]any{
		"id":         id,
		"user":       map[string]any{"login": fmt.Sprintf("author-%d", id)},
		"created_at": "2026-01-01T00:00:00Z",
		"body":       fmt.Sprintf("comment-%d\n%s", id, strings.Repeat("x", bodyBytes)),
	}
}

// TestCommentIndexIsLosslessUnderManyLargeComments is the root-cause proof: with
// 200 comments of 40 KiB each (≈8 MB aggregate — 100x the per-message cap), the
// index delivered by get_pr_thread lists EVERY comment, so a consumer can fetch
// any of them. Under the old aggregate design this was impossible: the blob was
// cut at 64 KiB and the tail vanished.
func TestCommentIndexIsLosslessUnderManyLargeComments(t *testing.T) {
	const n, bodyBytes = 200, 40 * 1024

	comments := make([]map[string]any, 0, n)
	for i := 1; i <= n; i++ {
		comments = append(comments, bigComment(i, bodyBytes))
	}
	rawComments, err := json.Marshal(comments)
	if err != nil {
		t.Fatal(err)
	}

	idx := parseCommentIndex(string(rawComments))
	if len(idx) != n {
		t.Fatalf("index rows = %d, want %d — the index lost comments", len(idx), n)
	}
	for i, m := range idx {
		want := i + 1
		if m.ID != want {
			t.Fatalf("row %d id = %d, want %d (index order/identity broken)", i, m.ID, want)
		}
		if m.Bytes < bodyBytes {
			t.Fatalf("row %d reports %d bytes, want >= %d", i, m.Bytes, bodyBytes)
		}
		if len(m.Preview) > commentPreviewBytes+8 {
			t.Fatalf("row %d preview = %d bytes, want ~%d (the index must stay small)", i, len(m.Preview), commentPreviewBytes)
		}
	}
	// The aggregate bodies WOULD have blown the cap; the index must not.
	rawIndex, _ := json.Marshal(prThread{Comments: idx, CommentCount: len(idx), MaxCommentBytes: defaultToolResultMaxBytes})
	if len(rawIndex) > defaultToolResultMaxBytes {
		t.Fatalf("index of %d comments is %d bytes — it must fit the per-message cap", n, len(rawIndex))
	}
	t.Logf("index of %d comments: %d bytes (aggregate bodies were %d bytes)", n, len(rawIndex), len(rawComments))
}

// TestThreadToolReturnsIndexNotBodies pins that the thread result carries ids and
// previews but NOT the full comment bodies — the separation that prevents the
// aggregate-blob truncation.
func TestThreadToolReturnsIndexNotBodies(t *testing.T) {
	tools := toolSet{fixture: "fx"}
	got, err := tools.call(context.Background(), "get_pr_thread", "")
	if err != nil {
		t.Fatal(err)
	}
	var th prThread
	if err := json.Unmarshal([]byte(got), &th); err != nil {
		t.Fatalf("thread result is not the index shape: %v", err)
	}
	if th.CommentCount != 2 || len(th.Comments) != 2 {
		t.Fatalf("comment_count=%d rows=%d, want 2", th.CommentCount, len(th.Comments))
	}
	if th.Comments[1].Author != "user" {
		t.Fatalf("index row author = %q, want user", th.Comments[1].Author)
	}
	// The full body of comment 2 is "second comment"; the index must only preview it.
	if strings.Contains(got, `"body"`) {
		t.Fatalf("the thread index must NOT carry a full body field: %s", got)
	}
}

// TestSingleCommentIsItsOwnBoundedMessage proves a large single comment is
// delivered FULLY at the per-message cap (the cap is the engine's, so a comment
// within it is complete), and that the cut is announced when it exceeds it.
func TestSingleCommentIsItsOwnBoundedMessage(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "fixtures"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 50 KiB body: under the 64 KiB cap, so it must arrive COMPLETE.
	body := strings.Repeat("z", 50*1024)
	one := map[string]any{"id": 7, "user": map[string]any{"login": "big"}, "created_at": "2026-01-01T00:00:00Z", "body": body}
	raw, _ := json.Marshal(one)
	if err := os.WriteFile(filepath.Join(dir, "fixtures", "fx-get_pr_comment.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	tools := toolSet{fixture: "fx"}
	got, err := tools.call(context.Background(), "get_pr_comment", `{"id":7}`)
	if err != nil {
		t.Fatal(err)
	}
	var oc oneComment
	if err := json.Unmarshal([]byte(got), &oc); err != nil {
		t.Fatal(err)
	}
	if oc.ID != 7 || oc.Body != body {
		t.Fatalf("single comment not delivered complete: id=%d len=%d want %d", oc.ID, len(oc.Body), len(body))
	}
	if len(got) > defaultToolResultMaxBytes {
		t.Fatalf("a 50 KiB comment must fit the %d-byte per-message cap; got %d", defaultToolResultMaxBytes, len(got))
	}
}

// TestGetPrCommentRequiresID pins that the per-comment read is argument-checked:
// a missing / non-positive id is a clear error, never a silent whole-thread fetch.
func TestGetPrCommentRequiresID(t *testing.T) {
	tools := toolSet{fixture: "fx"}
	for _, args := range []string{"", "{}", `{"id":0}`, `{"id":-1}`} {
		if _, err := tools.call(context.Background(), "get_pr_comment", args); err == nil {
			t.Fatalf("get_pr_comment(%q) must error, not fetch the whole thread", args)
		}
	}
}

// TestThreadIndexArrivesCompleteInTheLoop is the end-to-end root-cause proof. It
// drives the REAL loop with a stub provider that calls get_pr_thread first
// (yielding a 200-comment index whose aggregate bodies are ~8 MB) then the full
// comment. The turn-2 request must carry EVERY index row within the 64 KiB
// per-message cap — under the old aggregate design the tail was cut and the
// newest comments were unreachable.
func TestThreadIndexArrivesCompleteInTheLoop(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "fixtures"), 0o755); err != nil {
		t.Fatal(err)
	}
	const n, bodyBytes = 200, 40 * 1024
	comments := make([]map[string]any, 0, n)
	for i := 1; i <= n; i++ {
		comments = append(comments, bigComment(i, bodyBytes))
	}
	cms, _ := json.Marshal(comments)
	idx := parseCommentIndex(string(cms))
	th := prThread{Comments: idx, CommentCount: len(idx), MaxCommentBytes: defaultToolResultMaxBytes}
	thRaw, _ := json.Marshal(th)
	if err := os.WriteFile(filepath.Join(dir, "fixtures", "fx-get_pr_thread.json"), thRaw, 0o644); err != nil {
		t.Fatal(err)
	}
	// The single-comment fixture: comment 200 — the LAST row, the one the old
	// aggregate truncation made unreachable.
	lastRaw, _ := json.Marshal(map[string]any{
		"id": n, "user": map[string]any{"login": "author-200"},
		"created_at": "2026-01-01T00:00:00Z", "body": fmt.Sprintf("comment-%d\n%s", n, strings.Repeat("x", bodyBytes)),
	})
	if err := os.WriteFile(filepath.Join(dir, "fixtures", "fx-get_pr_comment.json"), lastRaw, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	var mu sync.Mutex
	var turn2 map[string]any
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch atomic.AddInt32(&calls, 1) {
		case 1:
			// turn 1: call get_pr_thread
			sseChunk(rw, `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"t1","type":"function","function":{"name":"get_pr_thread","arguments":"{}"}}]}}]}`)
		case 2:
			mu.Lock()
			turn2 = body
			mu.Unlock()
			// turn 2: read the LAST comment by id, then finish
			sseChunk(rw, fmt.Sprintf(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"get_pr_comment","arguments":"{\"id\":%d}"}}]}}]}`, n))
		default:
			sseChunk(rw, `{"choices":[{"delta":{"content":"Verdict: PASS\n"}}]}`)
		}
		sseDone(rw)
	}))
	defer srv.Close()

	cfg := reviewConfig{Provider: "test", Model: "m", BaseURL: srv.URL, APIKey: "k", MaxTurns: 6,
		StreamIdleTimeout: 5 * time.Second, RetryBackoff: time.Millisecond, ToolResultMaxBytes: defaultToolResultMaxBytes}
	if _, err := runAgentLoop(context.Background(), cfg, "prompt", toolSet{fixture: "fx", toolResultMaxBytes: defaultToolResultMaxBytes}); err != nil {
		t.Fatalf("runAgentLoop: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if turn2 == nil {
		t.Fatal("no turn-2 request captured")
	}
	msgs, _ := turn2["messages"].([]any)
	var threadMsg string
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok || mm["role"] != "tool" {
			continue
		}
		c, _ := mm["content"].(string)
		if strings.Contains(c, "comment_count") {
			threadMsg = c
		}
	}
	if threadMsg == "" {
		t.Fatal("turn-2 request must carry the thread index as a tool message")
	}
	if len(threadMsg) > defaultToolResultMaxBytes+200 {
		t.Fatalf("thread index message is %d bytes — it must fit the %d-byte cap", len(threadMsg), defaultToolResultMaxBytes)
	}
	var got prThread
	if err := json.Unmarshal([]byte(threadMsg), &got); err != nil {
		t.Fatal(err)
	}
	if got.CommentCount != n || len(got.Comments) != n {
		t.Fatalf("thread message carries %d/%d comments, want ALL %d — the tail was lost",
			len(got.Comments), got.CommentCount, n)
	}
	if got.Comments[n-1].ID != n {
		t.Fatalf("the LAST comment id = %d, want %d — the tail is unreachable", got.Comments[n-1].ID, n)
	}
}
