package pluginreview

import "testing"

func TestParseCommits(t *testing.T) {
	cs, err := parseCommits(`[{"sha":"0123456789abcdef0123456789abcdef01234567","commit":{"author":{"name":"Alice","date":"2026-01-01T00:00:00Z"},"message":"feat: example\nbody"},"author":{"login":"alice"}}]`)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || cs[0].SHA != "0123456789ab" || cs[0].Author != "alice" || cs[0].Message != "feat: example" {
		t.Fatalf("unexpected: %+v", cs)
	}
}

func TestParseMeta(t *testing.T) {
	m, err := parseMeta("{\"title\":\"t\",\"state\":\"open\",\"additions\":3,\"changed_files\":2,\"head\":{\"sha\":\"abc\"},\"base\":{\"sha\":\"def\"}}")
	if err != nil {
		t.Fatal(err)
	}
	if m.Title != "t" || m.Additions != 3 || m.ChangedFiles != 2 {
		t.Fatalf("unexpected: %+v", m)
	}
}

func TestSplitRepo(t *testing.T) {
	o, n := splitRepo("opencharly/plugin-review")
	if o != "opencharly" || n != "plugin-review" {
		t.Fatalf("splitRepo = %q,%q", o, n)
	}
}
