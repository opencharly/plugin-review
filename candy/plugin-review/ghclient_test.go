package pluginreview

import (
	"reflect"
	"testing"
)

// gateNoticeInconclusive is a realistic "## validator INCONCLUSIVE" notice,
// including the <details> Diagnostics tail that carries ~65 KB of a degenerate
// "Hmm." line in the live failure.
const gateNoticeInconclusive = "## validator INCONCLUSIVE — no review verdict was produced (not a BLOCK; no code finding)\n\n" +
	"The gate could **not** obtain a review verdict on this run, so it produced **no code finding**.\n\n" +
	"- class: provider-timeout\n" +
	"- review exit code: 3\n\n" +
	"<details><summary>Diagnostics (tail of the review log)</summary>\n\n" +
	"~~~\nplugin-review[debug]: FAILED after 10m26s: LLM: empty completion (finish_reason=\"length\")\nHmm. Hmm. Hmm. Hmm. Hmm.\n~~~\n\n</details>\n"

// gateNoticeAutoClosed is a realistic "## Auto-closed:" notice.
const gateNoticeAutoClosed = "## Auto-closed: 5 unanswered BLOCK verdicts\n\n" +
	"This PR has received **5** validator `BLOCK` verdicts without the raised findings being fixed, so the gate is auto-closing it.\n\n" +
	"This is a policy action, not a code finding — the validator verdict on this run stands.\n"

// TestIsGateNotice pins the exact marker match: the two org pr-validator machine
// notices are recognised, and nothing else is.
func TestIsGateNotice(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool // true = a machine notice that must be EXCLUDED
	}{
		{"validator INCONCLUSIVE notice", gateNoticeInconclusive, true},
		{"Auto-closed notice", gateNoticeAutoClosed, true},
		{"INCONCLUSIVE with leading blank lines + indent", "\n\n  ## validator INCONCLUSIVE — no review verdict was produced\n\nbody", true},
		{"Review BLOCK verdict is KEPT", "## Review — BLOCK\n\n### Blocks\n\n- `foo.go:12` is wrong\n\nVerdict: BLOCK", false},
		{"Review PASS verdict is KEPT", "## Review — PASS\n\nLooks good.\n\nVerdict: PASS", false},
		{"normal human comment is KEPT", "This needs a test for the empty-input case — see #42.", false},
		{"empty body is KEPT", "", false},
		{"prose mention of the notice is KEPT", "See the earlier ## validator INCONCLUSIVE comment? Provider timeout, not a finding.", false},
		// A body whose first non-blank line merely CONTAINS the header (not at
		// its start) must not be filtered — the marker is a header prefix.
		{"indented-then-text is KEPT", "Note: ## validator INCONCLUSIVE was posted", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isGateNotice(tc.body); got != tc.want {
				t.Errorf("isGateNotice(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

// TestFilterGateNoticesExcludesOnlyMachineNotices is the integration proof: the
// exact slice the context assembler receives drops the two gate machine notices
// and keeps the real review comment and the human comment. It MUST FAIL without
// the filter (both machine-notice rows would survive and the want-slice would
// differ).
func TestFilterGateNoticesExcludesOnlyMachineNotices(t *testing.T) {
	in := []Comment{
		{ID: 1, Kind: "issue", Author: "github-actions[bot]", Body: gateNoticeInconclusive},
		{ID: 2, Kind: "issue", Author: "atrawog", Body: "Please rebase — the base moved."},
		{ID: 3, Kind: "issue", Author: "github-actions[bot]", Body: gateNoticeAutoClosed},
		{ID: 4, Kind: "issue", Author: "github-actions[bot]", Body: "## Review — BLOCK\n\n### Blocks\n\n- `x.go:1` is wrong\n\nVerdict: BLOCK"},
	}
	want := []Comment{
		{ID: 2, Kind: "issue", Author: "atrawog", Body: "Please rebase — the base moved."},
		{ID: 4, Kind: "issue", Author: "github-actions[bot]", Body: "## Review — BLOCK\n\n### Blocks\n\n- `x.go:1` is wrong\n\nVerdict: BLOCK"},
	}
	got := filterGateNotices(in)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("filterGateNotices mismatch:\n got=%+v\nwant=%+v", got, want)
	}
	// The caller's slice must not be mutated in place (the backing array is shared
	// with `out` in comments()).
	if len(in) != 4 {
		t.Fatalf("filterGateNotices mutated its input (len=%d, want 4)", len(in))
	}
}
