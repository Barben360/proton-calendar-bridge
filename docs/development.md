# Development

How to build and test pcb from a clone, rehearse a live session offline, run one against the test account, and find your way in the design notes. How to propose a change: [CONTRIBUTING.md](../CONTRIBUTING.md).

## Build and test

```sh
make build        # bin/pcb
make lint test    # gofmt, go vet, golangci-lint, go test -race, on JOBS CPUs (4 at most by default)
make install      # bin/pcb -> $(PREFIX)/bin/pcb, PREFIX ?= ~/.local
make ui           # bin/pcb-ui, the desktop app (cgo)
make install-ui   # pcb-ui, its desktop entry and its icon under $(PREFIX)
make image        # the container image, pcb:local
bin/pcb version
```

`make lint test` is the gate. It compiles `cmd/pcb-ui`, since `go vet`, golangci-lint and `go test ./...` compile it with cgo, so it needs a C compiler and the GL/X headers of the desktop app:

| Distribution | Packages |
|---|---|
| Debian, Ubuntu | `libgl1-mesa-dev libwayland-dev libxkbcommon-dev xorg-dev` (what CI installs) |
| Arch | `libglvnd libxcursor libxrandr libxinerama libxi wayland libxkbcommon` |

`make test JOBS=8` runs on more CPUs.

Tests must bite: each fails when the behaviour it guards breaks. Write the failing test first.

### Smoke tests

```sh
make container-smoke systemd-smoke   # throwaway containers; each prints PASS last
```

| Target | Checks |
|---|---|
| `container-smoke` | the image: healthy with no account, 401 over TLS with or without credentials, UID 65532, healthy after `docker kill` |
| `systemd-smoke` | `pcb install --user` and `--system` in a Debian container with systemd as PID 1: each unit up and waiting for a login, then uninstalled; no Proton call |

## Developer-only tools

`pcb-fake` (a loopback fake Proton, `cmd/pcb-fake`) and `pcb-lab` (the experiment harness that logs into the Proton test account) are developer tools: never built into the image, never installed by `make install`.

```sh
make rehearse             # the real pcb against pcb-fake; prints PASS last
make live-preflight-test  # the live-session guard's own test; make live-preflight checks a run root
make shellcheck           # the live-session and install scripts, with the pinned shellcheck image CI uses (needs docker)
```

`make rehearse` also drives `pcb mcp` against `pcb-fake`. A red rehearsal stops a live run.

## Release

Only the repository owner can create a `v*` tag (a repository ruleset). Releases follow semver: `vX.Y.Z`, or `vX.Y.Z-rc.N` for a pre-release, which never becomes `latest`.

1. Merge to main and wait for main's `ci` run to finish green.
2. Tag that commit, signed, and push the tag:

```sh
git tag -s vX.Y.Z <sha>
git push origin vX.Y.Z
```

3. Watch the one `release` run (`gh run watch`). It builds the archives and the image, attests them, and publishes the Release last, so `releases/latest` (what `install.sh` uses) changes only when everything is out.

The `release` workflow refuses a tag that is not on main, a tag whose `ci` run did not succeed (still running counts), a tag already released, and anyone but the owner. A refused tag published nothing: delete it, fix the cause, push it again.

```sh
git push origin :refs/tags/vX.Y.Z   # delete the remote tag
git tag -d vX.Y.Z                   # and the local one
```

Release notes come from the pull request titles; edit them on GitHub afterwards when needed.

A refused tag that you keep still counts: a later run compares every `v*` tag to decide `latest` and `X.Y`, so a kept `v2.0.0` stops `v1.5.0` from taking them. Delete it.

### When a release run fails

The run's jobs, in order: checks, the builds, the archives, macOS, `image`, `release`. What a failure leaves depends on where it happened:

| Failed | Published | What to do |
|---|---|---|
| before the `image` job | nothing | delete the tag, fix the cause, tag again |
| in the `image` job or after it | the image, maybe its tags | fix what is outside the commit (a runner, the network, GitHub), then "Re-run failed jobs" on that run. "Re-run all jobs" and a re-pushed tag are refused: the image is already pushed. A cause in the code cannot be fixed this way: the version is burned, release the next one |
| the image's attestation | the version tags, on an image without its attestation, and no Release | "Re-run failed jobs": it builds the same commit again and moves those tags onto the attested image |
| the last step, "The Release is published as decided" | everything: the Release is out | do not re-run; check the Release on GitHub and fix it there |

Only the owner may re-run a release's `image` job: its build and publish jobs refuse anyone else before building or tagging, as checks does on the first run.

### A development image

An image of any commit, branch or tag, tagged `sha-<12 hex>` of the commit it resolves to:

```sh
gh workflow run image.yml -f ref=<sha>
```

A commit on no branch or tag of this repository (a fork's, which GitHub resolves too) is refused: push it to a branch here first.

### Testing the release scripts

```sh
make release-test    # the tag rules and the archives
make install-test    # install.sh against a fake release
make actionlint      # lint the workflow files
make shellcheck      # shellcheck the shell scripts
```

`install.sh` and its test have hooks, for these tests only:

| Variable | Effect |
|---|---|
| `PCB_INSTALL_BASE_URL` | replaces `https://github.com/Barben360/proton-calendar-bridge/releases`; a `file://` URL works through curl |
| `PCB_INSTALL_OS` | the system name, instead of `uname -s` |
| `PCB_INSTALL_ARCH` | the architecture, instead of `uname -m` |
| `PCB_TEST_SH` | the shell `install_test.sh` runs `install.sh` with, instead of `sh`: `PCB_TEST_SH=dash make install-test` |

## Live sessions

A live session runs pcb against Proton itself. Rules:

- **Test account only**, never the author's own. Its username and password come from your password manager, through local copies of `scripts/*.env.example` in `~/.config/pcb-live/`.
- **Hosts that must never run a live command** are listed in the local `~/.config/pcb-live/hosts`, never in the repository.
- **Logins:** at most one per lab run, 60 s minimum between lab logins; honour `Retry-After`.
- **Every live command goes through** `scripts/live/run.sh`, which refuses directories outside the run root.

The order of the session and each step's checks: [HUMAN-CHECKS.md](../spec/HUMAN-CHECKS.md). The scripts and the hosts file: [scripts/live/README.md](../scripts/live/README.md). What has been run so far: [status](status.md).

## Where the design lives

| Path | What |
|---|---|
| [spec/SPEC.md](../spec/SPEC.md) | the specification |
| [spec/decisions/](../spec/decisions/INDEX.md) | the decisions, one file each, with an index |
| [spec/experiments/](../spec/experiments/) | experiments against Proton and its clients, and the live-session write-ups (`M5-live.md`, `M7-live.md`, `M8-live.md`) |
| [spec/mapping.md](../spec/mapping.md) | what the bridge does with each part of a CalDAV PUT (property mapping to Proton event rows) |
| [spec/OPEN.md](../spec/OPEN.md) | open questions and parked items |
| [spec/designs/](../spec/designs/), [spec/audits/](../spec/audits/) | designs, audits |

Credits for logic reused from other projects are in the [README](../README.md).

## Regenerating the screenshots

The `pcb-ui` pictures in the README and [desktop](desktop.md) are rendered by Fyne's test driver on made-up accounts (no bridge, no keyring, no Proton call), in the light and the dark variant, at twice the window's size, with a thin frame in the theme's border colour:

```sh
make screenshots   # writes docs/assets/ui/{account,add-account}-{light,dark}.png
```

Run it after a change to the window and commit the PNGs. The same test (`TestScreenshots` in `internal/ui/desktop/screenshots_test.go`) runs in `make test`, lighter (one view at 1×, into a temporary directory, so it does not starve the timing-sensitive login tests CI runs beside it), and fails when a picture has the wrong size, lacks its 1 px frame (so the window stands out from a README page of its own colour), or the two variants are not told apart. The output is byte-for-byte reproducible on one Fyne version.
