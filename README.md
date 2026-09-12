# plugin-review

Charly plugin contributing the **read-only GitHub PR review** for OpenCharly's gate —
a from-scratch, 1:1 Go port of the retired [pi-review-action](https://github.com/opencharly/pi-review-action):

- **`verb:pr`** — deterministic read-only check tools: `pr_diff` / `pr_commits` /
  `pr_thread` / `pr_meta` (same data shapes + truncation as the action's tools;
  offline `fixture:` mode for deterministic beds/tests).
- **`command:review`** — `charly review pr <N>`: the chat-completions agent loop
  (temperature 0.2, tool_choice auto, max_turns, **streamed** completions with a
  per-TURN retry), a deterministic **`Verdict: PASS|BLOCK`** final line, ONE PR
  comment with a run footer, and `$GITHUB_OUTPUT` compatibility
  (`response` / `success` / `verdict`).
- **`charly review --plan <path>`** — the runtime orchestration executor: executes a
  declared `review-plan.yml` step list, so ANY runtime plugin can join the review
  workflow purely through config (the workflow YAML stays static).

## Distribution

Shipped two ways:

1. **Welded into the charly release** (primary): `plugin-review@v<tag>` in
   `opencharly/charly`'s `scripts/host-command-plugins.txt` + the
   `packaging/charly.yml` variants — every charly install (release assets, native
   packages, images) then serves `charly review` with no build step.
2. **Direct release assets** (`release.yml`): `review-linux-{amd64,arm64}` +
   `review.providers`, materialized into `$CHARLY_PLUGIN_DIR` by the plan executor.

## Review-engine bounds (why the client is streamed)

The action this plugin ports issued ONE NON-streaming POST per turn with a single
`http.Client{Timeout}` covering the whole response. Turn 2 is the first turn that
carries tool output (the diff / thread / commits), so it had to produce an entire
generation inside that wall clock — which is why the org gate kept dying with
`context deadline exceeded (Client.Timeout exceeded while awaiting headers)`,
always on the same turn, on a healthy endpoint.

The engine bounds PROGRESS instead of total generation:

| Bound | Default | Env (seconds / bytes) | Covers |
|---|---|---|---|
| Stream idle | 3m | `AI_REVIEW_STREAM_IDLE_TIMEOUT` | silence between streamed chunks, including the wait for the first one (prompt processing of a large context) |
| Whole turn request | 15m | `AI_REVIEW_ATTEMPT_TIMEOUT` | response headers through the last chunk (also the time-to-first-byte bound) |
| Tool-result payload | 64 KiB | `AI_REVIEW_TOOL_RESULT_MAX_BYTES` | ONE tool result appended to the conversation |

Supporting decisions:

- the transport is **explicit**: `DisableKeepAlives` (a connection idled across a
  long local tool gap — each tool is a separate `gh api` subprocess — can never
  hang a POST; Go retries only idempotent requests and a POST is not one), a 30s
  `IdleConnTimeout`, and a `ResponseHeaderTimeout`;
- a **failed turn request is re-issued** (up to 3 attempts, 5s/10s backoff) with
  the SAME conversation state — re-issuing is safe because every review tool is a
  read-only GET — instead of re-running the whole loop from turn 1;
- only a COMPLETED pass that produced no Verdict line re-runs the loop;
- a stalled or unanswered provider is reported as `inconclusive:`, never as a
  review BLOCK;
- a provider/gateway that ignores `stream: true` and answers with a whole JSON
  body is still accepted.

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
