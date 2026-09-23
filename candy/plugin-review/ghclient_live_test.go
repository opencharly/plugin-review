package pluginreview

import (
	"context"
	"os"
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
