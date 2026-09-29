# plugin-review

The OpenCharly PR-review engine — the externalized `charly review` command.

`command:review` assembles the **complete** PR context (the body, every changed
file's full unified diff, the commits, and the comment thread) into **one
message** and makes **one model call**. It emits a deterministic
**`Verdict: PASS|BLOCK`** final line, one PR comment with a run footer, and
`$GITHUB_OUTPUT` compatibility (`response` / `success` / `verdict`).

A review is a pure function of `(rulebook, complete PR, model)`: all input is
read **once** and sent **once**, so there is nothing for the model to fetch and
no tool loop. The prompt is **embedded** in the binary (`prompt.md`) — no external
file is read at run time, so a run cannot be redirected by an environment path.
Every model-behaviour knob is an `AI_REVIEW_*` env var, declared in `charly.yml`
(`env_accept`) and consumed in `config.go`; a test asserts the declaration list
equals the env the engine reads, so a knob cannot silently drift. The fail-closed
context guard refuses a PR too large for the window rather than truncating it.

The canonical GitHub READ verbs live in `plugin-gh` (`verb:gh`); this plugin
consumes that module's client and does not duplicate it.

## What it provides

| Capability | Surface |
|---|---|
| `command:review` | `charly review` — one-message, one-call PR review with a deterministic `Verdict: PASS|BLOCK` |

## How to use it

```bash
charly review --pr <n> --repo <owner>/<repo>
charly review --self-test
charly review --self-test-verdict
```

Set the `AI_REVIEW_*` org/repo GitHub Actions variables for provider, model,
endpoint, sampling, timeouts, and the context window; `AI_REVIEW_DEBUG=1` emits
the full request trace and the model's reasoning. No retry: a terminal failure is
classified and reported, never re-issued.

## Distribution

Shipped two ways:

1. **Welded into the charly release** (primary): `plugin-review@v<tag>` in
   `opencharly/charly`'s `scripts/host-command-plugins.txt` + the
   `packaging/charly.yml` variants — every charly install then serves
   `charly review` with no build step.
2. **Direct release assets**: `review-linux-{amd64,arm64}` + `review.providers`.

## Development

```console
$ cd candy/plugin-review
$ gofmt -l . && go vet ./... && go test ./...
```

To build the plugin into a private dir so `charly` resolves this build:

```console
$ ( cd candy/plugin-review && GOWORK=off go build -o ~/plugins/plugin-review ./cmd/serve )
$ printf 'command:review\n' > ~/plugins/plugin-review.providers
$ CHARLY_PLUGIN_DIR=~/plugins charly review --self-test
```

## Layout

- `candy/plugin-review/` — the plugin module: `review.go` (the one-path engine),
  `context.go` (the complete-context assembly), `llm.go` (the one model call),
  `config.go` (`FromEnv`), `ghclient.go` (the PR reads), `prompt.md` +
  `prompt_embed.go` (the embedded rulebook), `schema/review.cue`,
  `cmd/serve/main.go`.
- `charly.yml` — the root project manifest (`discover: candy`, the model-behaviour
  `var:`/`env_accept`/`secret_accept` surface, + the `review-skill` skill entity).
- `.github/workflows/ci.yml` + `.github/workflows/release.yml` +
  `.github/workflows/tag-on-merge.yml`.

## Related

- Owning skill: `/charly-review:review` — the `charly review` PR-review engine,
  authored in this candy's `skill:` entity.
- `/charly-internals:git-workflow` — the PR-only landing + validator gate.
- [`opencharly/charly`](https://github.com/opencharly/charly) — the charly CLI.
