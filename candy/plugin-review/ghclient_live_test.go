package pluginreview

import (
	"context"
	"os"
	"strings"
	"testing"
)

// TestFilesRecoversOmittedPatchLive is the live proof for
// opencharly/plugin-review#20. Against the REAL opencharly/charly#625 — whose
// charly.yml carries an API-omitted per-file `patch` (`has_patch=false`, +60/-1697)
// — files() must return a NON-empty Patch for charly.yml, recovered from the PR's
// raw .diff. Live or skip: skipped when no GitHub credential is available, never a
// mock of the API.
func TestFilesRecoversOmittedPatchLive(t *testing.T) {
	if os.Getenv("GH_TOKEN") == "" && os.Getenv("GITHUB_TOKEN") == "" {
		t.Skip("GH_TOKEN/GITHUB_TOKEN unset — skipping the live GitHub read")
	}
	gh := newGHClient()
	files, err := gh.files(context.Background(), "opencharly/charly", 625)
	if err != nil {
		t.Fatalf("files(): %v", err)
	}
	var found bool
	for _, f := range files {
		if f.Path != "charly.yml" {
			continue
		}
		found = true
		if f.Additions+f.Deletions == 0 {
			t.Fatalf("charly.yml reports no changes — wrong PR?")
		}
		if f.Patch == "" {
			t.Fatalf("charly.yml Patch is EMPTY — the #20 recovery did not fill it")
		}
		if !contains(f.Patch, "diff --git") && !contains(f.Patch, "@@") {
			t.Fatalf("charly.yml Patch is not a unified diff (first 80: %q)", f.Patch[:min(80, len(f.Patch))])
		}
		t.Logf("recovered charly.yml patch: %d bytes, NoPatch=%v", len(f.Patch), f.NoPatch)
	}
	if !found {
		t.Fatalf("charly.yml not among the changed files")
	}
}

// TestAssembleLiveCarriesAuthorship is the live proof that the assembled context
// carries WHO opened the PR and WHO wrote EVERY comment kind (issue, review,
// inline review) — the authorship facts a sign-off check reads. Live or skip:
// skipped when no GitHub credential is available, never a mock of the API.
func TestAssembleLiveCarriesAuthorship(t *testing.T) {
	if os.Getenv("GH_TOKEN") == "" && os.Getenv("GITHUB_TOKEN") == "" {
		t.Skip("GH_TOKEN/GITHUB_TOKEN unset — skipping the live GitHub read")
	}
	cfg := Config{Repo: "opencharly/spec", PR: 181, ContextTokens: 1 << 20, ContextMarginTokens: 16 << 10}
	c, err := assemble(context.Background(), cfg, newGHClient())
	if err != nil {
		t.Fatalf("assemble(): %v", err)
	}
	if c.Meta.Author == "" || c.Meta.Author == "unknown" {
		t.Fatalf("PR author not carried: %q", c.Meta.Author)
	}
	kinds := map[string]bool{}
	for _, cm := range c.Comments {
		if cm.Author == "" {
			t.Fatalf("comment %d carries no author", cm.ID)
		}
		kinds[cm.Kind] = true
	}
	if !kinds["issue"] {
		t.Errorf("no issue comments carried (kinds=%v)", kinds)
	}
	if !strings.Contains(c.Assembled, "Opened by: @"+c.Meta.Author) {
		t.Errorf("assembled context does not name the PR author %q", c.Meta.Author)
	}
	t.Logf("PR author=%s comments=%d kinds=%v", c.Meta.Author, len(c.Comments), kinds)
}

// TestCommentsExcludesGateNoticesLive is the live proof for
// opencharly/plugin-review#30 against the REAL opencharly/plugin-review#26 — a PR
// whose thread carries BOTH machine notices (a "## validator INCONCLUSIVE" and a
// "## Auto-closed:"). comments() must return NEITHER, while the RAW PRComments
// read still contains them (so the assertion is not vacuous: the filter is doing
// the work, not an empty PR). Live or skip: skipped when no GitHub credential is
// available, never a mock of the API.
func TestCommentsExcludesGateNoticesLive(t *testing.T) {
	if os.Getenv("GH_TOKEN") == "" && os.Getenv("GITHUB_TOKEN") == "" {
		t.Skip("GH_TOKEN/GITHUB_TOKEN unset — skipping the live GitHub read")
	}
	gh := newGHClient()
	cli, err := gh.client()
	if err != nil {
		t.Fatalf("client(): %v", err)
	}
	// The RAW issue comments — the unfiltered source comments() reads.
	raw, err := cli.PRComments(context.Background(), "opencharly/plugin-review", 26)
	if err != nil {
		t.Fatalf("PRComments(): %v", err)
	}
	rawNotices := 0
	for _, c := range raw {
		if isGateNotice(c.Body) {
			rawNotices++
		}
	}
	if rawNotices == 0 {
		t.Fatalf("fixture PR opencharly/plugin-review#26 carries no machine notice — the live assertion would be vacuous")
	}
	got, err := gh.comments(context.Background(), "opencharly/plugin-review", 26)
	if err != nil {
		t.Fatalf("comments(): %v", err)
	}
	for _, c := range got {
		if isGateNotice(c.Body) {
			t.Errorf("comments() returned a gate machine notice (id=%d): %.80q", c.ID, c.Body)
		}
	}
	if len(got) >= len(raw) {
		t.Errorf("comments() returned %d comments, raw has %d — the %d machine notice(s) were not dropped", len(got), len(raw), rawNotices)
	}
	t.Logf("opencharly/plugin-review#26: raw issue comments=%d (machine notices=%d), filtered comments()=%d", len(raw), rawNotices, len(got))
}
