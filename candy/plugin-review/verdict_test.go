package pluginreview

import "testing"

func TestExtractVerdict(t *testing.T) {
	cases := []struct {
		in    string
		wantN int
		wantD []string
	}{
		{"## Review — PASS\n\nVerdict: PASS\n", 1, []string{"PASS"}},
		{"Verdict: BLOCK\n", 1, []string{"BLOCK"}},
		{"Verdict:  PASS  \n", 1, []string{"PASS"}},
		{"Verdict: PASS\r\n", 1, []string{"PASS"}},
		{"no verdict here", 0, nil},
		{"Verdict: PASS\nVerdict: BLOCK\n", 2, []string{"BLOCK", "PASS"}},
		{"text before\nVerdict: PASS\ntext after", 1, []string{"PASS"}},
	}
	for _, c := range cases {
		_, distinct, n := extractVerdict(c.in)
		if n != c.wantN {
			t.Errorf("%q: n=%d want %d", c.in, n, c.wantN)
		}
		if len(distinct) != len(c.wantD) {
			t.Errorf("%q: distinct=%v want %v", c.in, distinct, c.wantD)
			continue
		}
		for i := range distinct {
			if distinct[i] != c.wantD[i] {
				t.Errorf("%q: distinct=%v want %v", c.in, distinct, c.wantD)
			}
		}
	}
}

func TestTruncate(t *testing.T) {
	s := truncateStr("hello", 3)
	if len(s) > 3+len("\n[…truncated…]") {
		t.Fatalf("truncate too long: %q", s)
	}
	if truncateStr("short", 1000) != "short" {
		t.Fatalf("no truncation expected")
	}
}

func TestWriteGHOutputs(t *testing.T) {
	if getenvAny("GITHUB_OUTPUT") != "" {
		t.Skip("env has GITHUB_OUTPUT")
	}
	t.Log("writeGHOutputs no-ops without GITHUB_OUTPUT (covered in e2e with a temp file)")
}
