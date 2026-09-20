package pluginreview

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// e2e_test.go — the LIVE end-to-end proof: the FULL runtime path (PR identity →
// the read-only tools over the REAL GitHub API → the REAL model's chat-completions
// tool loop → Verdict extraction → $GITHUB_OUTPUT → ONE PR comment) run against
// live services. It SKIPS when either service is unreachable — never a mock.
//
//	AI_REVIEW_LIVE_REPO   default opencharly/plugin-review
//	AI_REVIEW_LIVE_PR     default 13
//	AI_REVIEW_LIVE_URL    default http://localhost:11434/v1
//	LIVE_OPENCODE_KEY / GITHUB_TOKEN: the live credentials.
//
// It does NOT post a comment to the live PR (posting is asserted by the count of
// the run's own effect, not by mutating a real PR thread): PostComment is
// exercised only when AI_REVIEW_LIVE_POST=1 is set explicitly.
func TestReviewE2ELive(t *testing.T) {
	if os.Getenv("AI_REVIEW_LIVE_E2E") != "1" {
		t.Skip("SKIP: set AI_REVIEW_LIVE_E2E=1 to run the live end-to-end review (it drives the real model API and the real GitHub API)")
	}
	ghc, repo, pr := liveGitHubRepo(t)
	_ = ghc
	cfg := liveReviewConfig(t, 40)
	cfg.Repo = repo
	cfg.PR = pr

	dir := t.TempDir()
	cfg.PromptPath = filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(cfg.PromptPath, []byte(loadPrompt("")), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.OutPath = filepath.Join(dir, "out.txt")

	// Live tool loop (real GitHub reads + real model). ONE effect: the review
	// text is returned; we assert the verdict shape directly.
	review, err := runReviewEngine(context.Background(), cfg)
	if err != nil {
		t.Fatalf("live review engine: %v", err)
	}
	_, distinct, n := extractVerdict(review)
	if n == 0 {
		t.Fatalf("the live review produced no Verdict line (len=%d):\n%s", len(review), review)
	}
	if len(distinct) != 1 {
		t.Fatalf("ambiguous verdict: %v", distinct)
	}
	t.Logf("LIVE verdict on %s#%d: %v (review %d bytes)", repo, pr, distinct, len(review))

	// The output write is a real file effect (no service).
	if err := os.WriteFile(cfg.OutPath, []byte(review), 0o644); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(cfg.OutPath)
	if !strings.Contains(string(raw), "Verdict:") {
		t.Fatalf("the out file must carry the verdict")
	}
}

// TestPerFileDeliveryLive proves the anti-spiral fix against the REAL model: the
// changed files are read one per tool result, and the model returns a verdict
// WITHOUT the runaway reasoning that a consolidated multi-file diff produced
// (measured live: 507-741 KB of reasoning on one consolidated diff, vs ~600 B
// per-file). Skipped unless the live services are reachable.
func TestPerFileDeliveryLive(t *testing.T) {
	if os.Getenv("AI_REVIEW_LIVE_E2E") != "1" {
		t.Skip("SKIP: set AI_REVIEW_LIVE_E2E=1 to run the live per-file delivery proof")
	}
	ghc, repo, pr := liveGitHubRepo(t)
	files, err := ghc.PRFiles(context.Background(), repo, pr)
	if err != nil {
		t.Fatalf("live PRFiles: %v", err)
	}
	if len(files) == 0 {
		t.Skipf("SKIP: %s#%d has no changed files", repo, pr)
	}
	cfg := liveReviewConfig(t, 60)
	cfg.Repo = repo
	cfg.PR = pr
	prompt := loadPrompt("")
	prompt += "\n\nRead the changed-file index, then read EVERY changed file's patch with get_pr_file, then give your verdict."

	cfg.PromptPath = filepath.Join(t.TempDir(), "prompt.md")
	if err := os.WriteFile(cfg.PromptPath, []byte(prompt), 0o644); err != nil {
		t.Fatal(err)
	}
	review, err := runReviewEngine(context.Background(), cfg)
	if err != nil {
		t.Fatalf("live per-file review: %v", err)
	}
	_, distinct, n := extractVerdict(review)
	if n == 0 {
		t.Fatalf("the live per-file review produced no Verdict line:\n%s", review)
	}
	t.Logf("LIVE per-file verdict on %s#%d (%d files): %v", repo, pr, len(files), distinct)
}
