# plugin-review

Charly plugin contributing the **read-only GitHub PR review** for OpenCharly's gate.

- **`command:review`** — `charly review`: assembles the COMPLETE PR context (the body,
  EVERY changed file's full unified diff, the commits, every comment), **primes** the
  model with it in ONE message, then runs a **bounded tool loop** in which the read-only
  tools remain available for verification. It emits a deterministic
  **`Verdict: PASS|BLOCK`** final line, ONE PR comment with a run footer, and
  `$GITHUB_OUTPUT` compatibility (`response` / `success` / `verdict`).
- **`verb:pr`** — deterministic read-only PR-fact probes a check bed can call:
  `pr_meta` / `pr_commits` / `pr_thread` / `pr_files`.

## The review engine (one path)

```
FromEnv() → assemble() → render() → PRIME (one message, full context)
                                   → bounded tool loop (tools available)
                                   → extractVerdict() → Emit()
```

**Why prime AND tools.** The retired engine assembled the context turn by turn through
the tools. Message reasoning is NOT re-sent between turns, so the synthesis turn
re-derived everything from partial tool results — measured at **371 KB** of reasoning
over three runaway synthesis turns (**8m25s**, no verdict at cap) on a 25-file PR.
Delivering the whole context in ONE priming message produced a verdict from **ONE turn**
in **2m55s** on the same PR. The tools remain available so the agent can still verify a
fact or re-fetch something; because it already has the full context it rarely needs to.

## Configuration (env only, declared in `charly.yml`)

Every model-behaviour knob is an `AI_REVIEW_*` env var, set as a GitHub Actions org/repo
variable and consumed in `config.go` (`FromEnv`). Each is declared in `charly.yml`
(`env_accept`, with default `var:` values) as the documented surface and single source of
truth; `TestConfigSurfaceMatchesCharlyYML` asserts that declaration list EQUALS the env the
engine reads, so a knob cannot silently drift. (Note: for an out-of-process command plugin
charly currently passes the ambient environment, not the candy `var:` block — so the actual
values come from the runner's env/vars; the `var:` block documents defaults and is the
surface a future charly env-injection capability will honour. That capability is the
**charly env-injection cutover**.)

| Knob | Default | Purpose |
|---|---|---|
| `AI_REVIEW_PROVIDER` / `_MODEL` / `_BASE_URL` / `_API_KEY` | ollama-cloud / deepseek-v4.1-flash / https://ollama.com/v1 / — | provider + endpoint |
| `AI_REVIEW_REASONING_EFFORT` | high | thinking depth (high\|medium\|low\|max\|none) |
| `AI_REVIEW_MAX_TOKENS` | 262144 | shared reasoning+answer budget |
| `AI_REVIEW_MAX_COMPLETION_TOKENS` | — | answer-only budget (providers that split it) |
| `AI_REVIEW_TEMPERATURE` / `_TOP_P` / `_SEED` / `_STOP` | 0.2 / — / — / — | sampling |
| `AI_REVIEW_FREQUENCY_PENALTY` / `_PRESENCE_PENALTY` | — | repetition controls |
| `AI_REVIEW_STREAM_IDLE_TIMEOUT` | 180 s | silence between streamed chunks |
| `AI_REVIEW_ATTEMPT_TIMEOUT` | 900 s | whole-request cap (0 = none) |
| `AI_REVIEW_CONTEXT_TOKENS` / `_CONTEXT_MARGIN` | 1048576 / 16384 | the fail-closed size guard |
| `AI_REVIEW_MAX_TURNS` | 40 | tool-loop turns (the review is primed, so few are needed) |
| `AI_REVIEW_POST_COMMENT` | true | post the review as ONE PR comment |
| `AI_REVIEW_DEBUG` | 0 | full request trace + the model's reasoning (success AND failure) |
| `AI_REVIEW_PROMPT_EXTRA` | — | operator text appended to the embedded rulebook |
| `AI_REVIEW_SESSION_ID` | minted | session-affinity header (empty disables) |
| `AI_REVIEW_OUT` | — | write the review body to a file |

No retry: a terminal failure (empty completion, whole-request cap, idle stall) is
classified and reported, never re-issued — re-issuing the same request is the measured
amplifier of the long runs. The prompt is EMBEDDED (`prompt.md`): there is no runtime
prompt path for a PR to redirect.

## Distribution

Shipped two ways:

1. **Welded into the charly release** (primary): `plugin-review@v<tag>` in
   `opencharly/charly`'s `scripts/host-command-plugins.txt` + the `packaging/charly.yml`
   variants — every charly install (release assets, native packages, images) then serves
   `charly review` with no build step.
2. **Direct release assets**: `review-linux-{amd64,arm64}` + `review.providers`.

## Development

```console
$ cd candy/plugin-review
$ gofmt -l . && go vet ./... && go test ./...
```

The generated `params/cue_types_gen.go` is reproducible from `schema/review.cue`
(`cue exp gengotypes`, asserted by CI). To build the plugin into a private dir so
`charly` resolves THIS build (a binary without its `.providers` manifest is skipped and
charly falls through to `/usr/lib/charly/plugins`):

```console
$ ( cd candy/plugin-review && GOWORK=off go build -o ~/plugins/plugin-review ./cmd/serve )
$ printf 'verb:pr\ncommand:review\n' > ~/plugins/plugin-review.providers
$ CHARLY_PLUGIN_DIR=~/plugins charly review --self-test
```
