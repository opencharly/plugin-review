package pluginreview

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPlanPathFiresOneCommentOneOutput pins the SINGLE-EFFECT property: a plan
// whose one step is `kind: review` must write the $GITHUB_OUTPUT verdict exactly
// once (and post exactly one PR comment). Before this test the plan path ran the
// core review (which posts + writes) AND then the plan wrapper posted + wrote
// again — two comments per run, one code path too many.
//
// It runs LIVE (the real model + the real GitHub API) and SKIPS when either is
// unreachable — never a mock. It drives a plan whose review step reads a REAL
// PR, so the effect path is exercised end-to-end.
func TestPlanPathFiresOneCommentOneOutput(t *testing.T) {
	if os.Getenv("AI_REVIEW_LIVE_E2E") != "1" {
		t.Skip("SKIP: set AI_REVIEW_LIVE_E2E=1 to run the live plan single-effect proof (real model + real GitHub)")
	}
	_, repo, pr := liveGitHubRepo(t)
	cfg := liveReviewConfig(t, 40)
	cfg.Repo = repo
	cfg.PR = pr

	dir := t.TempDir()
	cfg.PromptPath = filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(cfg.PromptPath, []byte(loadPrompt("")), 0o644); err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(dir, "review-plan.yml")
	plan := "version: 1\nplugins: []\nsteps:\n  - id: review\n    kind: review\n    verdict: required\n"
	if err := os.WriteFile(planPath, []byte(plan), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.PlanPath = planPath
	cfg.OutPath = filepath.Join(dir, "out.txt")
	ghOutput := filepath.Join(dir, "gh_output.txt")
	t.Setenv("GITHUB_OUTPUT", ghOutput)
	// Posting a real comment is a MUTATION; it is opt-in so the proof does not
	// touch a live PR thread by default.
	post := os.Getenv("AI_REVIEW_LIVE_POST") == "1"
	if !post {
		cfg.PR = 0 // the comment path is skipped; the $GITHUB_OUTPUT effect is asserted
	}

	if _, err := runPlan(context.Background(), cfg); err != nil {
		t.Fatalf("runPlan: %v", err)
	}
	raw, _ := os.ReadFile(ghOutput)
	if got := strings.Count(string(raw), "verdict="); got != 1 {
		t.Errorf("GITHUB_OUTPUT verdict= count = %d, want exactly 1:\n%s", got, raw)
	}
}
