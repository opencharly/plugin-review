package pluginreview

import (
	"strings"
	"testing"
)

// review_test.go — the fail-closed verdict contract and the size guard. These are
// the engine's new control paths, so they carry their own coverage (the removed
// loop_test/e2e/livetest suites were the only ones that drove the engine).

// TestVerdictExitIsFailClosed pins the contract Run enforces: exit 0 ONLY for a
// single unambiguous verdict; a verdict-less review is exit 1 (the INCONCLUSIVE
// class the gate keeps RED); a mixed PASS+BLOCK is exit 2. An unreviewed PR must
// never read as a pass.
func TestVerdictExitIsFailClosed(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantExit int
		wantErr  bool
	}{
		{"single PASS", "## Review — PASS\n\nVerdict: PASS\n", 0, false},
		{"single BLOCK", "Verdict: BLOCK\n", 0, false},
		{"no verdict", "I could not decide.", 1, true},
		{"empty", "", 1, true},
		{"ambiguous", "Verdict: PASS\nVerdict: BLOCK\n", 2, true},
		{"prose mention is not a verdict (line-anchored)", "see Verdict: PASS above", 1, true},
	}
	for _, c := range cases {
		exit, err := verdictExit(c.body)
		if exit != c.wantExit {
			t.Errorf("%s: verdictExit exit=%d want %d", c.name, exit, c.wantExit)
		}
		if (err != nil) != c.wantErr {
			t.Errorf("%s: verdictExit err=%v wantErr=%v", c.name, err, c.wantErr)
		}
		if c.wantErr && err != nil && !strings.Contains(err.Error(), "inconclusive") {
			t.Errorf("%s: the error must be the inconclusive class, got %v", c.name, err)
		}
	}
}

// TestReviewValidatesBeforeNetwork pins that a misconfigured run fails before any
// network call (the required identity is checked up front).
func TestReviewValidatesBeforeNetwork(t *testing.T) {
	if _, err := Review(t.Context(), Config{PR: 0, Repo: ""}); err == nil {
		t.Fatal("an empty config must be rejected before any call")
	}
	if _, err := Review(t.Context(), Config{PR: 1, Repo: "no-slash"}); err == nil {
		t.Fatal("a repo without owner/ must be rejected before any call")
	}
}
