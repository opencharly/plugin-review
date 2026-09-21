package pluginreview

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEmitWritesOutputsAndBody is the coverage for the gate's OUTPUT surface
// (the removed e2e_test.go was its only test): $GITHUB_OUTPUT response/success/
// verdict, the --out file, and no comment when POST_COMMENT is false.
func TestEmitWritesOutputsAndBody(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "review.txt")
	ghOut := filepath.Join(dir, "github_output")
	if err := os.WriteFile(ghOut, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITHUB_OUTPUT", ghOut)
	cfg := Config{OutPath: outPath, PostComment: false}
	body := "## Review — BLOCK\n\nfindings\n\nVerdict: BLOCK\n"
	if err := Emit(context.Background(), cfg, body); err != nil {
		t.Fatal(err)
	}
	// --out holds the body verbatim.
	got, err := os.ReadFile(outPath)
	if err != nil || string(got) != body {
		t.Fatalf("--out mismatch: err=%v got=%q", err, got)
	}
	// $GITHUB_OUTPUT holds response/success/verdict with the verdict extracted.
	raw, err := os.ReadFile(ghOut)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, want := range []string{"success=true", "verdict=BLOCK"} {
		if !strings.Contains(s, want) {
			t.Errorf("$GITHUB_OUTPUT missing %q: %q", want, s)
		}
	}
	if !strings.Contains(s, "response<<") {
		t.Errorf("the multi-line response must use the heredoc form: %q", s)
	}
}

// TestEmitNoVerdictStillWritesSuccessFalse pins the output contract for a
// verdict-less body (Run turns this into a non-zero exit; Emit writes the state).
func TestEmitNoVerdictStillWritesSuccessFalse(t *testing.T) {
	ghOut := filepath.Join(t.TempDir(), "o")
	if err := os.WriteFile(ghOut, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITHUB_OUTPUT", ghOut)
	if err := Emit(context.Background(), Config{PostComment: false}, "nothing decisive"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(ghOut)
	if !strings.Contains(string(raw), "success=false") {
		t.Errorf("a verdict-less body must write success=false: %q", raw)
	}
}

// TestEmitOutWriteFailureIsFatal pins finding 2's fix: a failure to write --out is
// a REAL error (the workflow reads that file for the verdict), so Run's
// `if err != nil { return 2, err }` arm is live.
func TestEmitOutWriteFailureIsFatal(t *testing.T) {
	cfg := Config{OutPath: "/proc/does-not-exist/review.txt", PostComment: false}
	t.Setenv("GITHUB_OUTPUT", "")
	if err := Emit(context.Background(), cfg, "Verdict: PASS\n"); err == nil {
		t.Fatal("a failed --out write must be a real error, not a silent continue")
	}
}
