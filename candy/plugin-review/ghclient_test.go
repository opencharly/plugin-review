package pluginreview

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestReadsUseOwnerRepoSlug is the carried-forward regression for the 404 class
// that made every validator run BLOCK: ghkit builds /repos/<slug>/... from ONE
// slug, so an owner-less/bare-name repo 404s. The clean engine removes the split
// representation entirely (Config.Repo IS the slug, and Validate rejects one
// without "/"), and this test pins that every read targets /repos/<owner>/<repo>/.
func TestReadsUseOwnerRepoSlug(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		if strings.HasPrefix(req.URL.Path, "/repos/spec/") {
			t.Errorf("bare-name path reached GitHub: %s (ghkit requires owner/repo)", req.URL.Path)
		}
		if !strings.HasPrefix(req.URL.Path, "/repos/opencharly/spec/") {
			t.Errorf("unexpected path: %s", req.URL.Path)
		}
		rw.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(req.URL.Path, "/files"):
			_, _ = rw.Write([]byte(`[{"filename":"f.go","status":"modified","additions":1,"deletions":0,"patch":"x"}]`))
		case strings.Contains(req.URL.Path, "/commits"):
			_, _ = rw.Write([]byte(`[]`))
		case strings.Contains(req.URL.Path, "/comments"):
			_, _ = rw.Write([]byte(`[]`))
		case strings.Contains(req.URL.Path, "/issues/"):
			_, _ = rw.Write([]byte(`{"body":"b"}`))
		default:
			_, _ = rw.Write([]byte(`{"title":"t","state":"open","changed_files":1,"head":{"sha":"h","ref":"x"},"base":{"ref":"main"}}`))
		}
	}))
	defer srv.Close()

	t.Setenv("GITHUB_API_URL", srv.URL)
	t.Setenv("GH_TOKEN", "test-token")

	gh := newGHClient()
	ctx := context.Background()
	if _, err := gh.meta(ctx, "opencharly/spec", 140); err != nil {
		t.Fatalf("meta: %v", err)
	}
	if _, err := gh.body(ctx, "opencharly/spec", 140); err != nil {
		t.Fatalf("body: %v", err)
	}
	if _, err := gh.files(ctx, "opencharly/spec", 140); err != nil {
		t.Fatalf("files: %v", err)
	}
	if _, err := gh.commits(ctx, "opencharly/spec", 140); err != nil {
		t.Fatalf("commits: %v", err)
	}
	if _, err := gh.comments(ctx, "opencharly/spec", 140); err != nil {
		t.Fatalf("comments: %v", err)
	}
}

// TestValidateRejectsBareRepo pins the structural prevention: a repo without an
// owner is refused before any network call, so the bare-name form cannot occur.
func TestValidateRejectsBareRepo(t *testing.T) {
	cfg := Config{PR: 1, Repo: "spec", ContextTokens: 1000, MaxTokens: 10}
	if err := cfg.Validate(); err == nil {
		t.Fatal("a bare repo name must be rejected (the bare-name 404 class)")
	}
}

// TestVerbWireShapesHaveJSONTags pins the verb:pr WIRE contract. The cleanup once
// dropped the struct tags, silently changing every verb result from snake_case
// (`head_sha`, `path`, `sha`, `id`, `comment_count`) to Go field names — a
// consumer-visible break. These tests assert the exact keys consumers read.
func TestVerbWireShapesHaveJSONTags(t *testing.T) {
	cases := []struct {
		name string
		v    any
		want []string
	}{
		{"meta", PRMeta{}, []string{"title", "state", "head_sha", "base_sha", "branch", "changed_files"}},
		{"file-index", ChangedFile{}, []string{"path", "status", "additions", "deletions", "patch_bytes"}},
		{"commit", Commit{}, []string{"sha", "author", "message"}},
		{"comment", Comment{}, []string{"id", "author", "created_at", "body"}},
	}
	for _, c := range cases {
		b, err := json.Marshal(c.v)
		if err != nil {
			t.Fatal(err)
		}
		for _, key := range c.want {
			if !strings.Contains(string(b), `"`+key+`"`) {
				t.Errorf("%s: wire shape is missing %q (got %s)", c.name, key, b)
			}
		}
	}
}
