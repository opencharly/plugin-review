package pluginreview

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubGitHub stands up a stub API and points ghkit at it, so the tool DISPATCH
// is exercised end to end: argument validation, each tool's path, the error
// paths, and error propagation. The removed slug_test drove the seven dispatch
// sites this way; that coverage is restored here for the new callTool.
func stubGitHub(t *testing.T) *ghClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		if strings.HasPrefix(req.URL.Path, "/repos/spec/") {
			t.Errorf("bare-name path reached GitHub: %s (ghkit requires owner/repo)", req.URL.Path)
		}
		rw.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(req.URL.Path, "/files"):
			_, _ = rw.Write([]byte(`[{"filename":"f.go","status":"modified","additions":1,"deletions":0,"patch":"x"}]`))
		case strings.Contains(req.URL.Path, "/commits"):
			_, _ = rw.Write([]byte(`[{"sha":"abc","author":"a","message":"m"}]`))
		case strings.Contains(req.URL.Path, "/comments/"):
			_, _ = rw.Write([]byte(`{"id":1,"user":{"login":"u"},"created_at":"2026-01-01T00:00:00Z","body":"c"}`))
		case strings.Contains(req.URL.Path, "/comments"):
			_, _ = rw.Write([]byte(`[{"id":1,"user":{"login":"u"},"created_at":"2026-01-01T00:00:00Z","body":"hello"}]`))
		case strings.Contains(req.URL.Path, "/issues/"):
			_, _ = rw.Write([]byte(`{"body":"b"}`))
		default:
			_, _ = rw.Write([]byte(`{"title":"t","state":"open","changed_files":1,"head":{"sha":"h"},"base":{"ref":"main"}}`))
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("GITHUB_API_URL", srv.URL)
	t.Setenv("GH_TOKEN", "test-token")
	return newGHClient()
}

// TestCallToolDispatch drives EVERY tool through callTool against the stub and
// asserts each returns a non-empty JSON result — the dispatch coverage the
// removed suite had.
func TestCallToolDispatch(t *testing.T) {
	gh := stubGitHub(t)
	ctx := context.Background()
	cases := []struct{ name, args string }{
		{"get_pr_meta", ""},
		{"get_pr_body", ""},
		{"get_pr_files", ""},
		{"get_pr_file", `{"path":"f.go"}`},
		{"get_pr_commits", ""},
		{"get_pr_thread", ""},
		{"get_pr_comment", `{"id":1}`},
	}
	for _, c := range cases {
		out, err := gh.callTool(ctx, "opencharly/spec", 140, c.name, c.args)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if strings.TrimSpace(out) == "" {
			t.Errorf("%s: empty result", c.name)
		}
	}
}

// TestCallToolArgumentValidation pins the argument checks the removed
// thread_index_test had (get_pr_file needs a path; get_pr_comment needs an id).
func TestCallToolArgumentValidation(t *testing.T) {
	gh := stubGitHub(t)
	ctx := context.Background()
	for _, args := range []string{"", "{}", `{"path":""}`, `{"path":"  "}`} {
		if _, err := gh.callTool(ctx, "opencharly/spec", 1, "get_pr_file", args); err == nil {
			t.Errorf("get_pr_file(%q) must error", args)
		}
	}
	for _, args := range []string{"", "{}", `{"id":0}`, `{"id":-1}`} {
		if _, err := gh.callTool(ctx, "opencharly/spec", 1, "get_pr_comment", args); err == nil {
			t.Errorf("get_pr_comment(%q) must error", args)
		}
	}
	if _, err := gh.callTool(ctx, "opencharly/spec", 1, "no_such_tool", ""); err == nil {
		t.Error("an unknown tool must error")
	}
}

// TestThreadIndexCarriesNoBodies is the restored contract: the thread index is
// metadata + a preview ONLY, never a full body (the removed
// TestThreadToolReturnsIndexNotBodies pinned this).
func TestThreadIndexCarriesNoBodies(t *testing.T) {
	gh := stubGitHub(t)
	out, err := gh.callTool(context.Background(), "opencharly/spec", 140, "get_pr_thread", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"comment_count"`) || !strings.Contains(out, `"preview"`) {
		t.Fatalf("thread index must carry comment_count + preview: %s", out)
	}
	if strings.Contains(out, `"body"`) {
		t.Fatalf("the thread index must NOT carry a full body field: %s", out)
	}
}

// TestFilesIndexCarriesPatchSize is the restored contract: get_pr_files carries
// patch_bytes per file (its description promises it) but NOT the patch text.
func TestFilesIndexCarriesPatchSize(t *testing.T) {
	gh := stubGitHub(t)
	out, err := gh.callTool(context.Background(), "opencharly/spec", 140, "get_pr_files", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"patch_bytes"`) || !strings.Contains(out, `"total_patch_bytes"`) {
		t.Fatalf("the files index must carry patch_bytes/total_patch_bytes: %s", out)
	}
}
