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

// TestSplitUnifiedDiffQuotedPath pins the parser against git's C-quoted header
// form (`diff --git "a/<p>" "b/<p>"`), which git emits under core.quotePath for
// paths with spaces/tabs/quotes/non-ASCII. The key must be the UNQUOTED new
// name, so it matches the API's `filename` and the recovery lookup hits.
func TestSplitUnifiedDiffQuotedPath(t *testing.T) {
	diff := "diff --git \"a/dir with space/odd\\tfile.yml\" \"b/dir with space/odd\\tfile.yml\"\n" +
		"--- \"a/dir with space/odd\\tfile.yml\"\n" +
		"+++ \"b/dir with space/odd\\tfile.yml\"\n" +
		"@@ -1 +1 @@\n" +
		"-a\n" +
		"+b\n"
	got := splitUnifiedDiff(diff)
	want := "dir with space/odd\tfile.yml"
	if _, ok := got[want]; !ok {
		t.Fatalf("quoted path not keyed by its unquoted new name %q: %v", want, keys(got))
	}
	if !contains(got[want], "+b") {
		t.Errorf("quoted-path section lost its hunk: %q", got[want])
	}
}

// TestUnquoteGitPath covers the escape forms git emits.
func TestUnquoteGitPath(t *testing.T) {
	cases := map[string]string{
		`"b/plain.txt"`:         "b/plain.txt",
		`"b/a b.txt"`:           "b/a b.txt",
		`"b/tab\there.txt"`:     "b/tab\there.txt",
		`"b/quote\"inside.txt"`: "b/quote\"inside.txt",
		`"b/back\\slash.txt"`:   "b/back\\slash.txt",
		`"b/octal\303\251.txt"`: "b/octal\u00e9.txt",
	}
	for in, want := range cases {
		if got := unquoteGitPath(in); got != want {
			t.Errorf("unquoteGitPath(%s) = %q, want %q", in, got, want)
		}
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
