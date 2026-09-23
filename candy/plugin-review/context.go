package pluginreview

import (
	"context"
	"fmt"
	"strings"
)

// context.go — THE way the review input is assembled.
//
// Design (first principles): a review needs ONE coherent context UP FRONT.
// Fragmenting it across a turn-by-turn tool loop is what caused the runaway
// thinking — the model cannot see its own prior reasoning between turns, so it
// RE-DERIVES everything on the synthesis turn from partial tool results.
//
// Measured on a real 25-file PR (spec#140), all same model/effort:
//   - the fragmented tool loop: 371 KB of reasoning over three runaway synthesis
//     turns, 8m25s, no verdict within the cap;
//   - the SAME context delivered whole in ONE message, NO tools: 50 KB of
//     reasoning, finish_reason=stop, verdict in 82.6s (and 8/8 planted defects
//     found with exact line numbers in a separate anti-skim probe);
//   - the SHIPPED design (ONE message, ONE call): finish_reason=stop, verdict in
//     2m55s on spec#140 and 1m24s on action-review#7.
//
// So the assembler reads EVERY changed file's full patch (never a sample, never
// truncated) and emits ONE message; there is no per-file round-trip, no fixture
// branch, and no plan executor. A review cannot skim what it was never given, and
// it is given everything.
//
// Line-by-line guarantee: every changed file's complete unified diff is included,
// in order, each under a FILE header carrying its status and ±counts. The prompt
// instructs the model to review each file's patch line by line, and the size
// guard (config.ContextTokens) fails the run HARD if the whole context cannot fit
// — so the review either sees every line or does not run.

// Context is the assembled, complete review input.
type Context struct {
	Meta      PRMeta
	Body      string
	Files     []ChangedFile
	Commits   []Commit
	Comments  []Comment
	Assembled string // the exact user message sent to the model
}

// ChangedFile is one changed file with its FULL patch. NoPatch is true when the
// GitHub per-file `patch` field was omitted (the file's diff exceeds the API's
// per-file limit) — the file reader recovers those from the PR's raw .diff and
// clears NoPatch; if recovery also fails, render() says so explicitly rather
// than emitting an empty diff block a reviewer would read as "no change".
type ChangedFile struct {
	Path      string
	Status    string
	Additions int
	Deletions int
	Patch     string
	NoPatch   bool
}

// Commit is one commit row (render() emits the sha + the first line of the
// message; the author is not rendered, so it is not carried).
type Commit struct {
	SHA     string
	Message string
}

// Comment is one comment with its FULL body — the prompt requires every comment
// be considered and dispositioned, so the assembler delivers them all whole.
type Comment struct {
	ID        int
	Author    string
	CreatedAt string
	Body      string
}

// assemble gathers the complete PR context in ONE pass from the canonical client.
// Every read is required: a failure is a REAL error (the review must not proceed
// on a context it could not assemble — a partial context is a blind review).
func assemble(ctx context.Context, cfg Config, gh *ghClient) (*Context, error) {
	repo := cfg.Repo
	meta, err := gh.meta(ctx, repo, cfg.PR)
	if err != nil {
		return nil, fmt.Errorf("read PR metadata: %w", err)
	}
	body, err := gh.body(ctx, repo, cfg.PR)
	if err != nil {
		return nil, fmt.Errorf("read PR body: %w", err)
	}
	files, err := gh.files(ctx, repo, cfg.PR)
	if err != nil {
		return nil, fmt.Errorf("read changed files: %w", err)
	}
	commits, err := gh.commits(ctx, repo, cfg.PR)
	if err != nil {
		return nil, fmt.Errorf("read commits: %w", err)
	}
	comments, err := gh.comments(ctx, repo, cfg.PR)
	if err != nil {
		return nil, fmt.Errorf("read comment thread: %w", err)
	}
	c := &Context{Meta: meta, Body: body, Files: files, Commits: commits, Comments: comments}
	c.Assembled = render(c, cfg)
	return c, nil
}

// render is the pure assembler: Context -> the ONE user message. It is separated
// from IO so a test can assert the exact model input without a network call.
func render(c *Context, cfg Config) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Review pull request %s#%d — %q.\n\n", cfg.Repo, cfg.PR, c.Meta.Title)
	// The prompt's output format requires the head SHA (and branch context); the
	// engine MUST supply it, so it is rendered here from the fetched meta.
	fmt.Fprintf(&b, "Head SHA: `%s` (branch: `%s`; base `%s`; state `%s`; %d changed files).\n\n",
		c.Meta.HeadSHA, c.Meta.Branch, c.Meta.BaseSHA, c.Meta.State, c.Meta.ChangedFiles)
	b.WriteString("Everything below is the COMPLETE, CURRENT state of this PR: the body, EVERY changed file's full unified diff, the commits, and every comment, in the `<pr_body>`, per-file `### FILE:` and `<comment_thread>` sections. You have all of it; do not assume anything is missing.\n\n")

	b.WriteString("## PR body\n\n<pr_body>\n")
	b.WriteString(c.Body)
	b.WriteString("\n</pr_body>\n\n")

	fmt.Fprintf(&b, "## Changed files (%d total)\n\n", len(c.Files))
	for _, f := range c.Files {
		if f.Patch == "" {
			// Never emit an empty ```diff``` block: a reviewer (and the model)
			// must not read an API-omitted patch as "the file did not change".
			// The file reader recovers omitted patches from the raw .diff; if
			// this branch is reached, recovery failed for a real (+/-) change.
			fmt.Fprintf(&b, "### FILE: %s (%s, +%d/-%d)\n\n<!-- patch omitted by the GitHub API (per-file diff too large) AND not recoverable from the PR .diff; additions+deletions are non-zero, so this file DID change -->\n\n", f.Path, f.Status, f.Additions, f.Deletions)
			continue
		}
		fmt.Fprintf(&b, "### FILE: %s (%s, +%d/-%d)\n\n```diff\n%s\n```\n\n", f.Path, f.Status, f.Additions, f.Deletions, f.Patch)
	}

	if len(c.Commits) > 0 {
		b.WriteString("## Commits\n\n")
		for _, cm := range c.Commits {
			fmt.Fprintf(&b, "- %s %s\n", shortSHA(cm.SHA), firstLine(cm.Message, 200))
		}
		b.WriteString("\n")
	}

	fmt.Fprintf(&b, "## Comment thread (%d comments)\n\n<comment_thread>\n", len(c.Comments))
	for _, cm := range c.Comments {
		fmt.Fprintf(&b, "### Comment %d by %s at %s\n\n%s\n\n", cm.ID, cm.Author, cm.CreatedAt, cm.Body)
	}

	b.WriteString("</comment_thread>\n\nReview EVERY changed file's diff above LINE BY LINE, consider every comment above, re-derive each claim against the current state, then end with exactly `Verdict: PASS` or `Verdict: BLOCK` on the final line.\n")
	return b.String()
}

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
