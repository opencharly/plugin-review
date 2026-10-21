---
name: review
description: 'The `charly review` PR-review engine — assembles the complete PR context
  into

  ONE message and makes ONE model call for a deterministic Verdict: PASS|BLOCK.

  Use to review a PR.'
---

# charly review — PR review engine

`charly review` is OpenCharly's gate reviewer, served by `candy/plugin-review`
(`command:review`, out-of-process over gRPC).

## How it works

ONE path: assemble the COMPLETE PR context (the body, EVERY changed file's full
unified diff, the commits, and the comment thread) → ONE message → ONE model call →
extract the deterministic `Verdict: PASS|BLOCK` line → emit the effects
(`--out`, `$GITHUB_OUTPUT`, ONE PR comment). The body, the per-file diffs and the
commits are always whole; the comment thread keeps its NEWEST comments whole and,
when it exceeds `AI_REVIEW_CONTEXT_THREAD_MAX_BYTES`, summarises the older ones
rather than letting the thread dominate the context.

The prompt is EMBEDDED in the binary (`prompt.md`) — no external file is read at
run time, so a run cannot be redirected by an environment path. Every
model-behaviour knob is an `AI_REVIEW_*` env var, declared in `charly.yml`
(`env_accept`) and consumed in `config.go` — set them as Org/repo GitHub Actions
variables. `AI_REVIEW_DEBUG=1` emits the full request trace AND the model's
reasoning, which is what an RCA needs.

The fail-closed context guard refuses a PR too large for the window rather than
truncating it: split the PR, or raise `AI_REVIEW_CONTEXT_TOKENS`.

## Cross-References

- `/charly-internals:git-workflow` — the PR-only landing + validator gate.
