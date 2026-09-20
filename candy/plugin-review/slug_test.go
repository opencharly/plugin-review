package pluginreview

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestToolCallsPassOwnerRepoSlug is the regression for the 404 class that made
// every validator run BLOCK: ghkit (plugin-gh) builds /repos/<slug>/... from ONE
// slug, but the engine stores owner and repo separately and the tool dispatch
// passed the BARE repo name — /repos/spec/pulls/140 404s (not a route). The
// fixture-backed tests never exercise ghkit's path building, which is why the
// defect escaped review.
//
// This test stands up a stub GitHub API and drives ALL SEVEN dispatch sites
// (get_pr_meta, get_pr_body, get_pr_files, get_pr_file, get_pr_commits,
// get_pr_thread, get_pr_comment — the two argument-taking tools included), and
// asserts every request targets /repos/<owner>/<repo>/..., failing loudly on
// the bare-name form.
func TestToolCallsPassOwnerRepoSlug(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		// A bare-name path (the bug) is /repos/spec/... ; the correct slug form is
		// /repos/opencharly/spec/...
		if strings.HasPrefix(req.URL.Path, "/repos/spec/") {
			t.Errorf("bare-name path reached GitHub: %s (ghkit requires owner/repo)", req.URL.Path)
		}
		if !strings.HasPrefix(req.URL.Path, "/repos/opencharly/spec/") {
			t.Errorf("unexpected path: %s", req.URL.Path)
		}
		rw.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(req.URL.Path, "/files"):
			// get_pr_file calls PRFiles and selects the requested path, so the
			// index must contain it.
			_, _ = rw.Write([]byte(`[{"filename":"f.go","status":"modified","additions":1,"deletions":0,"patch":"x"}]`))
		case strings.Contains(req.URL.Path, "/commits"):
			_, _ = rw.Write([]byte(`[]`))
		case strings.Contains(req.URL.Path, "/comments/"):
			// Single-comment fetch (get_pr_comment): one object.
			_, _ = rw.Write([]byte(`{"id":1,"user":{"login":"u"},"created_at":"2026-01-01T00:00:00Z","body":"c"}`))
		case strings.Contains(req.URL.Path, "/comments"):
			// Comment list (get_pr_thread): an array.
			_, _ = rw.Write([]byte(`[]`))
		case strings.Contains(req.URL.Path, "/issues/"):
			_, _ = rw.Write([]byte(`{"body":"b"}`))
		default:
			_, _ = rw.Write([]byte(`{"title":"t","state":"open","changed_files":1,"head":{"sha":"h"},"base":{"ref":"main"}}`))
		}
	}))
	defer srv.Close()

	t.Setenv("GITHUB_API_URL", srv.URL)
	t.Setenv("GH_TOKEN", "test-token")

	tools := toolSet{gh: newGHClient(), owner: "opencharly", repo: "spec", pr: 140}
	calls := []struct{ name, args string }{
		{"get_pr_meta", ""},
		{"get_pr_body", ""},
		{"get_pr_files", ""},
		{"get_pr_file", `{"path":"f.go"}`},
		{"get_pr_commits", ""},
		{"get_pr_thread", ""},
		{"get_pr_comment", `{"id":1}`},
	}
	for _, c := range calls {
		if _, err := tools.call(context.Background(), c.name, c.args); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
	}
}

// TestToolSetSlug pins the join itself: an owner-qualified pair joins with
// exactly one slash and reports ok.
func TestToolSetSlug(t *testing.T) {
	got, ok := (&toolSet{owner: "opencharly", repo: "spec"}).slug()
	if !ok || got != "opencharly/spec" {
		t.Fatalf("slug() = %q,%v, want opencharly/spec,true", got, ok)
	}
}

// TestToolSetSlugRejectsEmptyOwner pins that an owner-less engine context is a
// reported error, NOT a silent bare-name path (the 404 class the PR fixes).
func TestToolSetSlugRejectsEmptyOwner(t *testing.T) {
	if got, ok := (&toolSet{repo: "spec"}).slug(); ok || got != "" {
		t.Fatalf("slug() with no owner = %q,%v; want empty,false", got, ok)
	}
	if got, ok := (&toolSet{owner: "opencharly"}).slug(); ok || got != "" {
		t.Fatalf("slug() with no repo = %q,%v; want empty,false", got, ok)
	}
}

// TestToolCallFailsWithoutOwner pins the dispatch-level behaviour: with a
// GitHub client but no owner context, every GitHub-backed tool returns a CLEAR
// error rather than issuing /repos/<bare>/... .
func TestToolCallFailsWithoutOwner(t *testing.T) {
	tools := toolSet{gh: newGHClient(), repo: "spec", pr: 140}
	if _, err := tools.call(context.Background(), "get_pr_meta", ""); err == nil {
		t.Fatalf("get_pr_meta with no owner must error, not issue a bare-name 404")
	}
}
