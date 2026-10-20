# AGENTS.md — layer-gh

Standalone candy repo for the `gh` layer — the GitHub command-line toolchain:
`gh`, `git`, and `git-lfs`, including the `tsflags=noscripts` + post-install
dance that keeps the `git-lfs` RPM scriptlet from failing at build time. The
candy lives in `charly.yml` at the repo root and projects the `gh` skill entity
(`family: coder`).

Canonical files:

- `charly.yml` — the `gh:` candy entity and the `gh-skill:` skill entity.
- `.github/workflows/tag-on-merge.yml` — CalVer tag + `CHANGELOG/` on merge.
- `README.md` — user overview only; never agent guidance.

## Load these skills first (R0)

- `/charly-coder:gh` — the owning skill: single-responsibility ownership of
  git/gh/git-lfs, the `tsflags=noscripts` rationale, and the six build-scope
  tests. Load before editing or troubleshooting the layer.
- `/charly-image:layer` — the candy authoring reference (`charly.yml` schema,
  `plan:` step verbs incl. `check:`, per-distro `distro:` arms, package
  sections). Load before editing any entity field or plan step.

## Build / validate / test

- The merge gate is the **org-wide** `charly/pr-validator` (required check
  `validate / validate`, defined in `opencharly/.github`); this repo has **no
  per-repo candy gate** and ships only `.github/workflows/tag-on-merge.yml`.
- The candy's `plan:` `check:` steps are the functional evidence: the
  `/usr/bin/{gh,git,git-lfs}` file checks, their `--version` stdout checks, and
  the `package=` checks for `gh`/`git`/`git-lfs` (with the `arch: github-cli`
  `package_map`).
- The `debian,ubuntu` compound distro arm is deliberate: the `github-cli` apt
  repo is codename-independent (`suite: stable`), so both share one section.

## Modify this repo

- Edit the `gh:` candy entity in `charly.yml`; keep the matching `gh-skill:`
  entity in step with it.
- Keep `gh`, `git`, and `git-lfs` owned here — do not add them to another
  candy's packages; that ownership is what keeps test ids collision-free.
- The `package_map` in the `package=gh` check must track the per-distro package
  arms.

## Landing

- PR-only. Every change lands through a pull request; the org-required
  `charly/pr-validator` validates the diff and body and arms native auto-merge
  on PASS. Direct pushes to `main` are blocked.
- History lives in `CHANGELOG/` (written by `tag-on-merge` at merge time); the
  PR body IS the changelog.
- The authoritative rulebook is the umbrella `AGENTS.md` in
  `opencharly/opencharly` and `charly/AGENTS.md` in the charly repo. Do not
  restate its rules here.
