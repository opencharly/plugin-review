# plugin-review

Charly plugin contributing the **read-only GitHub PR review** for OpenCharly's gate.

- **`command:review`** — `charly review`: assembles the COMPLETE PR context (the body,
  EVERY changed file's full unified diff, the commits, EVERY comment) into **ONE
  message** and makes **ONE model call**. It emits a deterministic
  **`Verdict: PASS|BLOCK`** final line, ONE PR comment with a run footer, and
  `$GITHUB_OUTPUT` compatibility (`response` / `success` / `verdict`).

The canonical GitHub READ verbs live in `plugin-gh` (`verb:gh`); this plugin consumes
that module's client and does not duplicate it.

## The review engine (one path)

A review is a pure function of `(rulebook, complete PR, model)`: all input is read
**once** and sent **once**, so there is nothing for the model to fetch and no tool loop.

```
FromEnv() → assemble() → render() → ONE message → ONE llmkit call → verdict → Emit()
```

**Why one message (measured RCA).** The earlier engine assembled the context turn by
turn through a tool-calling loop. Message reasoning is NOT re-sent between turns, so the
synthesis turn re-derived everything from partial tool results — on a 25-file PR that
produced **371 KB** of reasoning over three runaway synthesis turns (**8m25s**, no
verdict at cap). Delivering the whole context in ONE message produced a verdict from
**ONE call** (measured: 1m24s on a small PR; 2m55s on the 25-file spec#140).

## Configuration (env only, declared in `charly.yml`)

Every model-behaviour knob is an `AI_REVIEW_*` env var, set as a GitHub Actions org/repo
variable and consumed in `config.go` (`FromEnv`). Each is declared in `charly.yml`
(`env_accept`, with default `var:` values) as the documented surface and single source of
truth; `TestConfigSurfaceMatchesCharlyYML` asserts that declaration list EQUALS the env
the engine reads, so a knob cannot silently drift. (Note: for an out-of-process command
plugin charly currently passes the ambient environment, not the candy `var:` block — so
the actual values come from the runner's env/vars; the `var:` block documents defaults
and is the surface a future charly env-injection capability will honour. That capability
is the **charly env-injection cutover**.)

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
| `AI_REVIEW_POST_COMMENT` | true | post the review as ONE PR comment |
| `AI_REVIEW_DEBUG` | 0 | request trace + the model's reasoning (success AND failure) |
| `AI_REVIEW_PROMPT_EXTRA` | — | operator text appended to the embedded rulebook |
| `AI_REVIEW_SESSION_ID` | minted | session-affinity header (empty disables) |
| `AI_REVIEW_OUT` | — | write the review body to a file |

No retry: a terminal failure (a whole-request cap or idle stall) is classified and
reported, never re-issued. The prompt is EMBEDDED (`prompt.md` via `go:embed`): there is
no runtime prompt path for a PR or the environment to redirect; `AI_REVIEW_PROMPT_EXTRA`
appends operator text without replacing the shipped rulebook.

## Distribution

Shipped two ways:

1. **Welded into the charly release** (primary): `plugin-review@v<tag>` in
   `opencharly/charly`'s `scripts/host-command-plugins.txt` + the `packaging/charly.yml`
   variants — every charly install then serves `charly review` with no build step.
2. **Direct release assets**: `review-linux-{amd64,arm64}` + `review.providers`.

## Development

```console
$ cd candy/plugin-review
$ gofmt -l . && go vet ./... && go test ./...
```

To build the plugin into a private dir so `charly` resolves THIS build (a binary without
its `.providers` manifest is skipped and charly falls through to `/usr/lib/charly/plugins`):

```console
$ ( cd candy/plugin-review && GOWORK=off go build -o ~/plugins/plugin-review ./cmd/serve )
$ printf 'command:review\n' > ~/plugins/plugin-review.providers
$ CHARLY_PLUGIN_DIR=~/plugins charly review --self-test
```
