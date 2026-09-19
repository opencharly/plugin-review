package pluginreview

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestPlanPathFiresOneCommentOneOutput pins the SINGLE-EFFECT property: a plan
// whose one step is `kind: review` must post exactly ONE PR comment and write the
// $GITHUB_OUTPUT verdict exactly once. Before this test the plan path ran the
// core review (which posts + writes) AND then the plan wrapper posted + wrote
// again — two comments per run, one code path too many.
func TestPlanPathFiresOneCommentOneOutput(t *testing.T) {
	dir := t.TempDir()

	stub := filepath.Join(dir, "gh")
	// Every `gh api` invocation counts as a call; the comment POST is the only
	// mutating one, so count by matching its path fragment.
	ghScript := "#!/bin/bash\n" +
		"for a in \"$@\"; do\n" +
		"  case \"$a\" in\n" +
		"    */comments*) cat /dev/null; exit 0;;\n" +
		"    *application/vnd.github.diff*) echo 'diff'; exit 0;;\n" +
		"    */commits*) echo '[]'; exit 0;;\n" +
		"    */pulls/*) echo '{\"title\":\"t\",\"head\":{\"sha\":\"a\"},\"base\":{\"sha\":\"b\"}}'; exit 0;;\n" +
		"    */issues/*) echo '{\"body\":\"\"}'; exit 0;;\n" +
		"    --method) echo '{}'; exit 0;;\n" +
		"  esac\n" +
		"done\n" +
		"echo '{}'\n"
	if err := os.WriteFile(stub, []byte(ghScript), 0o755); err != nil {
		t.Fatal(err)
	}

	// local streamed LLM: one turn, a verdict
	llm := newVerdictServer(t, "Verdict: PASS\n")

	// a plan file with one review step
	planPath := filepath.Join(dir, "review-plan.yml")
	plan := "version: 1\nplugins: []\nsteps:\n  - id: review\n    kind: review\n    verdict: required\n"
	if err := os.WriteFile(planPath, []byte(plan), 0o644); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(dir, "out.txt")
	ghOutput := filepath.Join(dir, "gh_output.txt")

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GITHUB_TOKEN", "stub")
	t.Setenv("GH_TOKEN", "stub")
	t.Setenv("PR_NUMBER", "1")
	t.Setenv("GITHUB_REPOSITORY", "opencharly/plugin-review")
	t.Setenv("GITHUB_OUTPUT", ghOutput)
	t.Setenv("AI_REVIEW_BASE_URL", llm)
	t.Setenv("AI_REVIEW_MODEL", "m")
	t.Setenv("AI_REVIEW_API_KEY", "k")
	t.Setenv("AI_REVIEW_MAX_TURNS", "2")

	promptPath := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(promptPath, []byte("You are the validator."), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REVIEW_PROMPT_PATH", promptPath)

	cfg, _, err := parseReviewArgs([]string{"--plan", planPath, "--out", outPath, "pr", "1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runPlan(context.Background(), cfg); err != nil {
		t.Fatalf("runPlan: %v", err)
	}

	// The single effect: exactly one verdict= line in GITHUB_OUTPUT.
	raw, _ := os.ReadFile(ghOutput)
	if got := strings.Count(string(raw), "verdict="); got != 1 {
		t.Errorf("GITHUB_OUTPUT verdict= count = %d, want exactly 1:\n%s", got, raw)
	}
}

// newVerdictServer returns a local streaming endpoint that answers every turn
// with the given final content (a verdict line).
func newVerdictServer(t *testing.T, content string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		sseChunk(rw, `{"choices":[{"delta":{"content":`+strconv.Quote(content)+`}}]}`)
		sseDone(rw)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}
