# AGENTS.md — plugin-review

Standalone plugin repo for the PR-review engine (`command:review`). The plugin is
a Go module at `candy/plugin-review/` (module path
`github.com/opencharly/plugin-review/candy/plugin-review`); the root `charly.yml`
declares `discover: candy` so the repo is a project, and the candy manifest
carries the model-behaviour `var:`/`env_accept`/`secret_accept` surface **and**
the `review-skill` `skill:` entity (the corpus source for `/charly-review:review`).

Canonical files:

- `charly.yml` — the root project manifest (`discover: candy` only).
- `candy/plugin-review/charly.yml` — the `plugin-review:` candy entity
  (`plugin:` block, `require:`, the `AI_REVIEW_*` env surface, `plan:` checks) +
  the `review-skill` skill entity.
- `candy/plugin-review/review.go` — the one-path review engine.
- `candy/plugin-review/context.go` — the complete-PR-context assembly.
- `candy/plugin-review/llm.go` — the single model call.
- `candy/plugin-review/config.go` — `FromEnv`, the model-behaviour surface.
- `candy/plugin-review/prompt.md` + `prompt_embed.go` — the embedded rulebook.
- `.github/workflows/ci.yml` / `release.yml` / `tag-on-merge.yml`.
- `README.md` — user overview only; never agent guidance.

## Load these skills first (R0)

- `/charly-internals:plugin` — the plugin authoring reference: the `plugin:`
  block, the unified Provider model, the out-of-process command shape, the
  per-plugin CUE-schema contract, placement. Load before touching the provider or
  schema.
- `/charly-review:review` — the PR-review engine this candy owns.
- `/charly-internals:git-workflow` — the PR-only landing + validator gate and
  before any git/PR action.

## Build / validate / test

- `go build ./...` in `candy/plugin-review/` — compile the plugin module.
- `go test ./...` in `candy/plugin-review/` — the plugin's Go tests (the config
  surface, the verdict matcher, the diff split, the effects, and the live
  `ghclient_live_test.go` which skips without a token).
- `charly box validate` at the repo root — the structural check (the candy +
  `plugin:` block, CUE schema, the skill entity).
- The merge gate is the **org-wide** `charly/pr-validator` (required check
  `validate / validate`, defined in `opencharly/.github`); this repo has **no**
  per-repo candy gate.
- The `plan:` checks run `charly review --self-test` and
  `charly review --self-test-verdict`.

## Modify this repo

- Edit the `plugin-review:` candy entity, the Go source, and `schema/review.cue`
  **together** — the schema is the single source for the generated types.
- Every `AI_REVIEW_*` env the engine reads must be declared in the candy's
  `env_accept`/`var:` surface — a test asserts the two lists are equal, so a knob
  cannot silently fail to reach the engine.
- The `skill:` entity is the corpus source for `/charly-review:review`; a change
  to the engine surface belongs in BOTH the candy and the skill body.
- The prompt is **embedded** (`prompt.md` via `go:embed`); there is no runtime
  prompt path for a PR or the environment to redirect.

## Landing

- PR-only. Every change lands through a pull request; the org-required
  `charly/pr-validator` validates the diff and body and arms native auto-merge on
  PASS. Direct pushes to `main` are blocked.
- History lives in `CHANGELOG/` (written by `tag-on-merge` at merge time); the PR
  body IS the changelog.
- The authoritative rulebook is the umbrella `AGENTS.md` in
  `opencharly/opencharly` and `charly/AGENTS.md` in the charly repo. Do not
  restate its rules here.
