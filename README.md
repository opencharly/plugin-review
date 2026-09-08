# plugin-review

Charly plugin contributing the **read-only GitHub PR review** for OpenCharly's gate —
a from-scratch, 1:1 Go port of the retired [pi-review-action](https://github.com/opencharly/pi-review-action):

- **`verb:pr`** — deterministic read-only check tools: `pr_diff` / `pr_commits` /
  `pr_thread` / `pr_meta` (same data shapes + truncation as the action's tools;
  offline `fixture:` mode for deterministic beds/tests).
- **`command:review`** — `charly review pr <N>`: the chat-completions agent loop
  (temperature 0.2, tool_choice auto, max_turns, 3-attempt retry/backoff), a
  deterministic **`Verdict: PASS|BLOCK`** final line, ONE PR comment with a run
  footer, and `$GITHUB_OUTPUT` compatibility (`response` / `success` / `verdict`).
- **`charly review --plan <path>`** — the runtime orchestration executor: executes a
  declared `review-plan.yml` step list, so ANY runtime plugin can join the review
  workflow purely through config (the workflow YAML stays static).
- **`charly review bare`** — the BARE AGENT: sends things DIRECTLY to an LLM (no
  harness, no agent runtime) with a **fully configurable system prompt**
  (`--system-prompt <text|@file>` / `EVAL_LLM_SYSTEM_PROMPT`, falling back to
  `REVIEW_PROMPT_PATH`), the endpoint from the eval contract
  (`EVAL_LLM_BASE_URL/MODEL/API_KEY`, falling back to `AI_REVIEW_*`), a
  `--prompt <text|@file|-|:pr>` user message, and **all the tools needed to review
  a PR** via `--tools`: the four read-only verb:pr tools (diff/commits/thread/meta)
  attached as function tools and driven through the agent loop against
  `--repo/--pr` (or `GITHUB_REPOSITORY/PR_NUMBER`). With no `--tools` it is a
  single raw completion — used by the eval-charly judgment stages whose prompts are
  content files (fully configured in charly.yml).

## Distribution

Shipped two ways:

1. **Welded into the charly release** (primary): `plugin-review@v<tag>` in
   `opencharly/charly`'s `scripts/host-command-plugins.txt` + the
   `packaging/charly.yml` variants — every charly install (release assets, native
   packages, images) then serves `charly review` with no build step.
2. **Direct release assets** (`release.yml`): `review-linux-{amd64,arm64}` +
   `review.providers`, materialized into `$CHARLY_PLUGIN_DIR` by the plan executor.

## Development

```bash
cd candy/plugin-review
go mod tidy
go test ./...
# regenerate params after schema changes:
#   printf 'package params\n' > /tmp/w.cue && cat schema/review.cue >> /tmp/w.cue
#   cd /tmp && mkdir -p p && cp w.cue p/review.cue && cd p && cue exp gengotypes .
charly box validate   # ADE: description + ≥1 deterministic check
```

Every PR lands via the org-wide `charly/pr-validator` gate; every merge mints
`v<CalVer>` and is the pin target for consumers.
