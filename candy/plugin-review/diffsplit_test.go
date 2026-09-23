package pluginreview

import "testing"

// TestSplitUnifiedDiff is the recovery guarantee: a whole-PR .diff is split back
// into one patch per file, keyed by the new-side path, so an API-omitted per-file
// patch can be filled from it (the charly.yml case in opencharly/plugin-review#20).
func TestSplitUnifiedDiff(t *testing.T) {
	diff := "diff --git a/charly.yml b/charly.yml\n" +
		"index 1111111..2222222 100644\n" +
		"--- a/charly.yml\n" +
		"+++ b/charly.yml\n" +
		"@@ -1,3 +1,2 @@\n" +
		"-old\n" +
		"+new\n" +
		" keep\n" +
		"diff --git a/pkg/x.go b/pkg/x.go\n" +
		"index 3333333..4444444 100644\n" +
		"--- a/pkg/x.go\n" +
		"+++ b/pkg/x.go\n" +
		"@@ -10 +10 @@\n" +
		"-a\n" +
		"+b\n"

	got := splitUnifiedDiff(diff)
	if len(got) != 2 {
		t.Fatalf("split into %d files, want 2: %#v", len(got), got)
	}
	for _, want := range []string{"charly.yml", "pkg/x.go"} {
		p, ok := got[want]
		if !ok {
			t.Fatalf("missing %q in split: %v", want, keys(got))
		}
		if len(p) == 0 || p[0:11] != "diff --git " {
			t.Errorf("%q section does not start with its diff header: %q", want, p)
		}
		if !contains(p, "@@") {
			t.Errorf("%q section lost its hunk", want)
		}
	}
	if !contains(got["charly.yml"], "+new") || contains(got["charly.yml"], "pkg/x.go") {
		t.Errorf("charly.yml section is not isolated: %q", got["charly.yml"])
	}
}

// TestSplitUnifiedDiffRenamedPath keys a rename by its NEW path (b/<p>), which is
// what the API's `filename` reports.
func TestSplitUnifiedDiffRenamedPath(t *testing.T) {
	got := splitUnifiedDiff("diff --git a/old/name.yml b/new/name.yml\n--- a/old/name.yml\n+++ b/new/name.yml\n@@ -1 +1 @@\n-a\n+b\n")
	if _, ok := got["new/name.yml"]; !ok {
		t.Fatalf("rename not keyed by new path: %v", keys(got))
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
