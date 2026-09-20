package pluginreview

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// thread_index_test.go — the engine-side delivery contract.
//
// The GitHub API parsing and pagination live in the canonical ghkit (plugin-gh),
// tested there against a stubbed HTTP transport (`TestPRComments_IndexWithIds`,
// `TestPRFiles_PerFilePatchAndPagination`) — that is ghkit's OWN unit boundary.
// What THIS file pins is the engine-specific shaping: the tool argument checks,
// the anti-skim guard, and (live, in livetest_test.go) that the body/index/file
// messages arrive separately.

// TestThreadToolReturnsIndexNotBodies pins that the thread result carries ids and
// previews but NOT the full comment bodies.
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
	if strings.Contains(got, `"body"`) {
		t.Fatalf("the thread index must NOT carry a full body field: %s", got)
	}
}

// TestGetPrCommentRequiresID pins that the per-comment read is argument-checked.
func TestGetPrCommentRequiresID(t *testing.T) {
	tools := toolSet{fixture: "fx"}
	for _, args := range []string{"", "{}", `{"id":0}`, `{"id":-1}`} {
		if _, err := tools.call(context.Background(), "get_pr_comment", args); err == nil {
			t.Fatalf("get_pr_comment(%q) must error, not fetch the whole thread", args)
		}
	}
}

// TestGetPrFileRequiresPath pins the per-file read's argument check.
func TestGetPrFileRequiresPath(t *testing.T) {
	tools := toolSet{fixture: "fx"}
	for _, args := range []string{"", "{}", `{"path":""}`, `{"path":"  "}`} {
		if _, err := tools.call(context.Background(), "get_pr_file", args); err == nil {
			t.Fatalf("get_pr_file(%q) must error", args)
		}
	}
}

// TestAntiSkimGuardRequiresEveryFileRead pins the mechanical anti-skim guard: a
// verdict is refused until every file in the index has been fetched via
// get_pr_file.
func TestAntiSkimGuardRequiresEveryFileRead(t *testing.T) {
	tools := toolSet{fixture: "fx"}
	tools.recordFileIndex(prFileIndex{Files: []prFileMeta{{Path: "a.go"}, {Path: "b.go"}}})
	if got := tools.unreadFiles(); len(got) != 2 {
		t.Fatalf("before any read, unread = %v, want 2 files", got)
	}
	tools.recordFileRead("a.go")
	if got := tools.unreadFiles(); len(got) != 1 || got[0] != "b.go" {
		t.Fatalf("after reading a.go, unread = %v, want [b.go]", got)
	}
	tools.recordFileRead("b.go")
	if got := tools.unreadFiles(); len(got) != 0 {
		t.Fatalf("after reading every file, unread = %v, want none", got)
	}
	if got := (&toolSet{}).unreadFiles(); got != nil {
		t.Fatalf("without an index, unread = %v, want nil", got)
	}
}

// TestContextGuardFailsClosedOnTooLargeInput pins the mechanical PR-size guard:
// input + output reserve + margin over the context window is a HARD failure with
// the actionable class, never a truncation.
func TestContextGuardFailsClosedOnTooLargeInput(t *testing.T) {
	cfg := reviewConfig{ContextTokens: 1000, ContextMarginTokens: 100, MaxTokens: 500}
	if err := checkContextBudget(cfg, 100); err != nil {
		t.Fatalf("input+reserve+margin under the window must pass, got: %v", err)
	}
	err := checkContextBudget(cfg, 900)
	if err == nil {
		t.Fatal("input+reserve+margin over the window must fail hard")
	}
	if !strings.Contains(err.Error(), "inconclusive") || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("the failure must carry the inconclusive PR-too-large class, got: %v", err)
	}
}

// TestSingleCommentIsItsOwnBoundedMessage proves a large single comment is
// delivered FULLY under the per-message cap (fixture-backed; the fixture is a
// committed INPUT, not a fabricated service response).
func TestSingleCommentIsItsOwnBoundedMessage(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "fixtures"), 0o755); err != nil {
		t.Fatal(err)
	}
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
}
