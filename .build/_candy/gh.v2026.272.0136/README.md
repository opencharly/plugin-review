# gh

The GitHub command-line toolchain — `gh`, `git`, and `git-lfs` — from the shell.

`gh` is the single-responsibility home for the GitHub toolchain: the `gh` CLI
(packaged as `github-cli` on arch, `gh` on fedora/debian/ubuntu), `git`, and
`git-lfs`. `git-lfs` is installed with its rpm `%post` scriptlet suppressed
(`tsflags=noscripts`, since that scriptlet tries to talk to systemd inside the
build container) and its system hooks are configured by an explicit
post-install task (`git-lfs install --system --skip-repo`). All three binaries
are present and runnable in any box that composes this candy.

## What it provides

| Property | Value |
|---|---|
| Layer / candy | `gh` |
| Distro | all — `github-cli`/`gh` per distro; `git`, `git-lfs` |
| Binaries | `/usr/bin/gh`, `/usr/bin/git`, `/usr/bin/git-lfs` |
| Service / port | none |

## How to use it

Compose the layer by pinning this repo in a box's `candy:` list:

```yaml
my-box:
  candy:
    base: fedora
    candy:
      - '@github.com/opencharly/layer-gh:v2026.239.1624'
```

Then, inside the built image:

```bash
gh --version             # gh version X.Y.Z
git --version            # git version X.Y.Z
git-lfs --version        # git-lfs/X.Y.Z
```

## Why `tsflags=noscripts` + a post-install task

The `git-lfs` RPM's `%post` scriptlet runs `git-lfs install --system`, which
tries to modify `/etc/` and talk to systemd — operations that fail inside a
buildah container. The candy installs with `noscripts` and then runs the hook
configuration manually in a `run:` step.

## Single-responsibility ownership

This candy is the exclusive home for `gh`, `git`, and `git-lfs` — no other candy
(including `dev-tools`) installs them. Any box that wants git tooling composes
`gh` explicitly.

## Layout

- `charly.yml` — the `gh:` candy entity: the package/distro arms, the
  post-install `run:` step, and the `check:` steps.
- `.github/workflows/tag-on-merge.yml` — CalVer tag + `CHANGELOG/` on merge.
- `README.md` — this user overview.

## Related

- Family skill: `/charly-coder:gh`
- `/charly-distros:agent-forwarding` — pairs with gh for SSH/GPG agent access
- `/charly-build:secrets` — provision `GITHUB_TOKEN` for `gh auth login`
- [`opencharly/charly`](https://github.com/opencharly/charly) — the charly CLI and image builder
- [`opencharly/opencharly`](https://github.com/opencharly/opencharly) — the umbrella
