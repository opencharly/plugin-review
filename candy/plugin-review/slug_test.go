package pluginreview

import (
	"context"
	"encoding/json"
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
// This test stands up a stub GitHub API and asserts that EVERY engine tool call
// targets /repos/<owner>/<repo>/..., failing loudly on the bare-name form.
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
		// Minimal shapes sufficient for each tool to decode.
		switch {
		case strings.Contains(req.URL.Path, "/files"):
			_, _ = rw.Write([]byte(`[]`))
		case strings.Contains(req.URL.Path, "/comments"):
			_, _ = rw.Write([]byte(`[]`))
		case strings.Contains(req.URL.Path, "/commits"):
			_, _ = rw.Write([]byte(`[]`))
		case strings.Contains(req.URL.Path, "/issues/"):
			_, _ = rw.Write([]byte(`{"body":"b"}`))
		default:
			_, _ = rw.Write([]byte(`{"title":"t","state":"open","changed_files":0,"head":{"sha":"h"},"base":{"ref":"main"}}`))
		}
	}))
	defer srv.Close()

	t.Setenv("GITHUB_API_URL", srv.URL)
	t.Setenv("GH_TOKEN", "test-token")

	tools := toolSet{gh: newGHClient(), owner: "opencharly", repo: "spec", pr: 140}
	for _, name := range []string{
		"get_pr_meta", "get_pr_body", "get_pr_files", "get_pr_commits", "get_pr_thread",
	} {
		if _, err := tools.call(context.Background(), name, ""); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// TestToolSetSlug pins the join itself: a bare repo with no owner is returned
// unchanged (the unauthenticated/public-read path), and an owner-qualified pair
// joins with exactly one slash.
func TestToolSetSlug(t *testing.T) {
	for _, c := range []struct{ owner, repo, want string }{
		{"opencharly", "spec", "opencharly/spec"},
		{"", "spec", "spec"},
		{"opencharly", "", "opencharly/"},
	} {
		got := (&toolSet{owner: c.owner, repo: c.repo}).slug()
		if got != c.want {
			t.Fatalf("slug(%q,%q) = %q, want %q", c.owner, c.repo, got, c.want)
		}
	}
}

// TestToolSetSlugIsNotTheBareRepo is the one-line guard: the slug used by the
// tools must be owner-qualified when the engine has an owner.
func TestToolSetSlugIsNotTheBareRepo(t *testing.T) {
	tools := toolSet{owner: "opencharly", repo: "spec"}
	if tools.slug() == tools.repo {
		t.Fatalf("slug() returned the bare repo %q — the 404 class", tools.repo)
	}
	var _ = json.Marshal
}
