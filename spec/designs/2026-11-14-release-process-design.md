# Release process — design

- **Date**: 2026-11-14
- **Status**: design approved by the author 2026-11-14; not built yet
- **Decided by**: author (choices marked [author]); the rest is the agent's call, marked [call].
- **Amends**: SPEC §3 ("updates/signed releases (while private)" leaves "not planned"), SPEC §13 "Until the repository is public" and "Later, when public" (the minisign [call] is replaced by GitHub attestations, and `install.sh` does not run `pcb install`), via a new decision 0017; `docs/install.md`, `docs/container.md`, `docs/development.md`, the README; the "private" wording left in SPEC §3, §13 and the `Makefile`'s `JOBS` comment (the repository is public now).

## 1. Intent

The repository is public, but nothing is released: users build from a clone, and the container image is private and published on every push to main. The author wants a release process [author]:

- a protected semver tag `vX.Y.Z` publishes the release;
- the container image is pushed **only on release**, with its version tags and `latest`, and the package becomes **public**;
- a **manual** workflow still builds an image from any commit, for development;
- a `curl … | sh` installer for Linux installs the binaries and, by default, the desktop entry; a flag skips the desktop entry;
- a **macOS** build, installed by the same script, untested by the author (someone else tests it later);
- the release files are signed so a user can check where they came from.

A Windows MSI was asked for too, but `pcb-ui` does not run on Windows (`cmd/pcb-ui/main_other.go` exits 2) and `pcb login`, `sync` and `serve` exit 20 there (no file lock, no control socket). The Windows port comes first, as its own project, and the MSI follows it [author].

## 2. Goals, non-goals, anti-goals

Goals:
- Pushing a `vX.Y.Z` tag on a commit of main whose CI passed publishes, with no other manual step: a GitHub Release with the archives, `checksums.txt` and `install.sh`; the multi-arch image `ghcr.io/barben360/proton-calendar-bridge` with `X.Y.Z`, `X.Y` and `latest`; an attestation for every file and for the image.
- `curl -fsSL https://github.com/Barben360/proton-calendar-bridge/releases/latest/download/install.sh | sh` installs the latest stable release on Linux (amd64, arm64) and macOS (amd64, arm64), with no sudo by default.
- Anyone can pull the image with no login.
- A developer can publish an image of any commit by hand.

Non-goals (not designed here):
- The Windows port of `pcb` and `pcb-ui`, and the MSI [author: next project].
- A macOS `pcb-ui` (`.app`, tray, Keychain): the Fyne build is Linux only, so macOS gets `pcb` alone [call]. A macOS UI would be a port like Windows.
- Windows archives of `pcb`: it builds, but cannot log in or serve, so shipping it would mislead [call].
- Authenticode or Apple signing and notarisation: a self-signed certificate gives Windows users nothing, and the paid or vetted options (SignPath Foundation, Azure Trusted Signing, an Apple Developer ID) are for the Windows project or later.
- Distribution packages (`.deb`, AUR, Homebrew), auto-update, an `--uninstall` flag (the docs list the files to remove) [call].
- `install.sh` setting up the service: it places files only; `pcb install` or `pcb-ui` stays the user's next step, as today [call].

Anti-goals (failure even if it works):
- A release, or a `latest`, built from a commit that is not on main, or whose CI failed.
- A tag that moves: a published `vX.Y.Z` (Git tag, Release or image tag) is never rebuilt or overwritten.
- `install.sh` installing a file whose checksum does not match, or a half-installed set (new `pcb`, old `pcb-ui`).
- `latest` pointing at a pre-release.
- A secret, a machine name or a personal path in a workflow, an archive or a log (CONTRIBUTING, AGENTS.md).

## 3. Constraints

- `pcb` is `CGO_ENABLED=0` and static (`cmd/pcb/arch_test.go`); it cross-compiles for every target from one Linux runner.
- `pcb-ui` needs cgo and the GL/X headers, and links them dynamically: it must be built natively per architecture, on an old glibc so it runs on most distributions.
- Workflow actions are pinned by commit SHA with the version in a comment, as in `.github/workflows/ci.yml`; workflow files are pushed over SSH (AGENTS.md).
- GitHub's `releases/latest` excludes pre-releases, which gives the installer "latest stable" for free.
- macOS `/bin/sh` and `bash` 3.2 lack GNU tools (`sha256sum`, `install -D`, `readlink -f`); `curl` does not set the quarantine attribute, so a binary downloaded by the script runs without a Gatekeeper prompt; Go ad-hoc signs darwin/arm64 binaries itself.
- Changing a GHCR package to public, and deleting its versions, are done by the author in the package settings (the REST API does not change a user package's visibility).

## 4. Approaches

Build tooling:
- **A. Our own workflow** [author, agent's pick]: tag → checks → per-architecture builds on native runners → image → Release. Same style as the current CI (pinned actions, `make` targets), cgo builds are plain, and the refusals of §5.1 fit in.
- B. GoReleaser: one config file, but `pcb-ui` must build natively per architecture, and merging builds from several runners is, as far as known, a paid feature; it would end up wrapped in our own workflow anyway.

Signing:
- **A. GitHub artifact attestations (Sigstore, keyless)** [author, agent's pick]: prove a file or image was built by this repository's release workflow from a given commit; cover the archives, `install.sh` and the image; no key to keep; checked with `gh attestation verify` or `cosign`.
- B. minisign over `checksums.txt`, as backsight: proves the key holder signed; binaries only, the image needs a second tool; one secret to guard.

## 5. Design

### 5.1 The release workflow (`.github/workflows/release.yml`)

Trigger: push of a tag matching `v*`.

1. **Checks** (one job, before anything builds) [call]. Refuse, with an `::error::` naming the cause:
   - a tag that is not `vMAJOR.MINOR.PATCH` or `vMAJOR.MINOR.PATCH-PRERELEASE` (semver 2.0 without build metadata);
   - a tag whose commit is not an ancestor of `origin/main`;
   - a tag whose Release already exists;
   - a commit where the `ci` workflow did not succeed on the push to main (looked up through the API; `image` no longer runs on main, and the release smoke-tests its own image). A commit whose run is still going is refused; the author deletes the tag and pushes it again once main is green, possible only while nothing was published.
2. **Builds** (matrix):
   - `pcb` for linux/amd64, linux/arm64, darwin/amd64, darwin/arm64: `CGO_ENABLED=0`, stamped by `make build` with `VERSION=<tag>`.
   - `pcb-ui` for linux/amd64 on `ubuntu-22.04` and linux/arm64 on `ubuntu-22.04-arm`, directly on the runner with the CI's headers: glibc 2.35, so it runs on Ubuntu 22.04+, Debian 12+, Fedora 36+ and Arch; RHEL 9 (2.34) and Debian 11 are not covered [call, review: Debian 12's 2.36 would have excluded Ubuntu 22.04]. When GitHub retires the 22.04 images, the build moves to a container of the oldest supported Ubuntu.
   - Each binary is checked: `pcb version --json` and `xvfb-run pcb-ui --version` print the tag on Linux; the darwin binaries are checked by the macOS job of §5.5, which runs them from the release's own archives before the Release is published (`go version -m` cannot: `-trimpath` drops `-ldflags` from the build info) [review].
3. **Archives** [call]: `pcb_<os>_<arch>.tar.gz`, flat, with no version in the name, so `releases/latest/download/<name>` resolves with no API call (the version is in the Release and in `pcb version`); each holds:
   - Linux: `pcb`, `pcb-ui`, `pcb-ui.desktop` (from `packaging/`), `pcb-ui.svg` (from `internal/ui/desktop/icons/app.svg`), `LICENSE`, `README.md`;
   - macOS: `pcb`, `LICENSE`, `README.md`.
   `checksums.txt` (SHA-256, `sha256sum` format) covers every archive and `install.sh`.
4. **Image**: §5.2, release mode.
5. **Attestations**: `actions/attest-build-provenance` on every archive, `install.sh` and `checksums.txt`, and on the image digest with `push-to-registry: true` (permissions `id-token: write`, `attestations: write`).
6. **Release**: a draft with `gh release create --draft --generate-notes --verify-tag`, marked pre-release when the tag has a `-` suffix; the job re-downloads the draft's files and checks them against `checksums.txt`, then publishes it (backsight's pattern) [call]. Notes come from the squash-merged pull request titles; the author edits them on GitHub afterwards when needed [author]. The Release's "latest" flag follows the image's rule (§5.2): `--latest=false` unless the tag is the highest stable version, so `releases/latest`, and with it `install.sh`, never goes back to an older line [call, review: GitHub otherwise marks the newest-created release latest].

Order matters: the image is pushed and its tags moved only after every build succeeded; the Release is published last, so `releases/latest` (what `install.sh` uses) changes only when everything is out [call].

### 5.2 Images (`.github/workflows/image.yml`)

One build path shared by both modes (a reusable workflow or a composite action: builder's choice) [call]:

- The Dockerfile is unchanged apart from the version labels below: each architecture builds natively, so no cross-compiling stage is needed [review].
- Each architecture is built and smoke-tested (`scripts/container-smoke.sh`) on its native runner (`ubuntu-latest`, `ubuntu-24.04-arm`), pushed by digest, and the tags are put on a manifest list (`docker buildx imagetools create`) once both pass [call].

Modes:

| Trigger | Tags pushed |
|---|---|
| pull request | none (smoke test only, as today; amd64 is enough) |
| release tag `vX.Y.Z` | `X.Y.Z`, `X.Y`, `latest` |
| release tag `vX.Y.Z-rc.N` (pre-release) | `X.Y.Z-rc.N` only |
| `workflow_dispatch` with input `ref` (commit, branch or tag; default the dispatching branch) | `sha-<12 hex>` of the resolved commit |
| push to main | none (removed) [author] |

`X.Y` moves to the newest patch; `latest` moves to the newest stable release by version, not by date: a patch release of an older line (`v1.2.4` after `v1.3.0`) does not take `latest` [call]. The image carries `org.opencontainers.image.version` and `.revision` labels.

### 5.3 The installer (`scripts/install.sh`, attached to each release)

POSIX `sh`, shellchecked (`make shellcheck`, same pinned image) [call].

```sh
curl -fsSL https://github.com/Barben360/proton-calendar-bridge/releases/latest/download/install.sh | sh
curl -fsSL …/install.sh | sh -s -- --no-desktop
```

| Flag | Effect | Default |
|---|---|---|
| `--prefix DIR` | install under `DIR/bin`, `DIR/share/…` | `~/.local` |
| `--version vX.Y.Z` | that release instead of the latest stable | latest stable |
| `--no-desktop` | no desktop entry nor icon [author] | entry installed |
| `--no-ui` | `pcb` only, no `pcb-ui`, no desktop entry (servers) [call] | both binaries |
| `--verify` | require the attestation check (fail when `gh` is missing or not logged in) [call] | check when `gh` is logged in, else skip with a line |
| `-h`, `--help` | usage | |

Steps:
1. Detect OS and architecture (`uname -s`, `uname -m`: `x86_64`/`amd64`, `aarch64`/`arm64`); refuse anything else, naming what was found. On macOS, `--no-desktop` and `--no-ui` are implied and the script says `pcb-ui is Linux only for now`.
2. Download the archive and `checksums.txt` into a temporary directory (`mktemp -d`, removed on exit) with `curl` or `wget`; from `releases/download/<tag>/`, or `releases/latest/download/` without `--version` (no API call, no rate limit).
3. Check the archive's SHA-256 (`sha256sum`, else `shasum -a 256`); refuse on mismatch or a missing line.
4. Attestation: `gh attestation verify <archive> --repo Barben360/proton-calendar-bridge` per §5.3's `--verify` rule.
5. Install, never with sudo; an unwritable prefix fails with a hint (`--prefix`, or run as a user who can write it). Each file is copied to a temporary name beside its target, then renamed over it, so a running `pcb-ui` keeps its old file and no half-written binary exists; all files are staged first and renamed together, so a failure leaves the previous version whole [call].
   - `pcb` → `PREFIX/bin/pcb` (0755); `pcb-ui` → `PREFIX/bin/pcb-ui` (0755);
   - desktop entry → `PREFIX/share/applications/pcb-ui.desktop` with `Exec=` set to the absolute path, quoted per the Desktop Entry spec when the path has a space (fixes OPEN.md's unquoted `Exec=`); icon → `PREFIX/share/icons/hicolor/scalable/apps/pcb-ui.svg`; `update-desktop-database` when present.
6. After install:
   - warn when `PREFIX/bin` is not on `PATH`, with the line to add;
   - Linux with `pcb-ui`: warn and name the packages (`docs/install.md` table) when `ldd pcb-ui` reports a missing library;
   - print `pcb version` and the next step (`pcb-ui`, or `pcb install` for a service, or `pcb login`);
   - when a `pcb serve`, service or `pcb-ui` is running (`pcb status --json --daemon` answers), say to restart it to use the new version.

A test hook, `PCB_INSTALL_BASE_URL`, points the script at a local directory or server instead of GitHub; it is documented in `docs/development.md` only [call].

### 5.4 Tag protection

A repository ruleset on `refs/tags/v*`: creation, update and deletion restricted, the repository admin (the author) as the only bypass [author]. Created by the author or, with the author's go, by `gh api` in the implementation. The workflow checks of §5.1 hold even if the ruleset is changed.

### 5.5 Testing (tests that bite)

- `install.sh` [call]: a script test (`scripts/install_test.sh`, run in CI's `check` job and by a make target) against a fake release built locally under `PCB_INSTALL_BASE_URL`, each case asserting the observable result:
  - default install places both binaries, the entry with the absolute `Exec=`, and the icon, under a temporary prefix;
  - `--no-desktop` places no entry nor icon; `--no-ui` places only `pcb`;
  - a corrupted archive and a missing checksum line are refused, exit non-zero, and leave a previous install untouched;
  - an unknown architecture and an unknown flag are refused;
  - `--verify` with no `gh` fails;
  - a prefix with a space yields a quoted `Exec=`.
- macOS [call, review: where it runs]: a `macos-latest` job (free on a public repository) in two places:
  - `ci.yml`, on every pull request and push to main: `install_test.sh` with darwin archives built in the job, then `pcb version --json` and `pcb doctor --json` on the installed binary;
  - `release.yml`, before the Release is published: `install.sh` against the draft's real darwin archive, then `pcb version --json` must print the tag. A failure blocks the release.
  It proves the script and that the binary starts, not that sync works; the author's tester covers that.
- Release workflow: a dry run on a pre-release tag (`v0.1.0-rc.1`) before the first stable release: archives, checksums, attestations (`gh attestation verify`), the image's `0.1.0-rc.1` tag pulled anonymously, `latest` untouched.
- The refusals of §5.1: each is a small shell function in a script (`scripts/release/check.sh`) with a test of its own (wrong tag form, commit off main, existing release), so they are not tested only by pushing tags [call].

### 5.6 Documentation

- `docs/install.md`: prebuilt releases first (the `curl` line, flags, where files go, verifying with `gh attestation verify`, upgrading by re-running the script, removing the files); building from a clone second; macOS: `pcb` only, untested, no service management, `xattr -d com.apple.quarantine` when the archive was downloaded by a browser.
- `docs/container.md`: public pull, the tag table, `latest` is the newest stable.
- `docs/development.md`: how to release (merge, wait for main green, `git tag -s vX.Y.Z`, push the tag, watch one release run) and the manual image workflow.
- README: the `curl` line.
- `spec/decisions/0017-release-process.md` and the SPEC amendments listed above; `spec/OPEN.md` keeps "macOS untested".

### 5.7 One-time steps by the author (after merge, before the first tag)

1. Create the tag ruleset (§5.4) if the implementation did not.
2. Push the first pre-release tag (`v0.1.0-rc.1`, §5.5): it adds the first release version to the package.
3. Delete the `:main` and `sha-…` versions published from main [author: agreed to the agent's suggestion] (they would stay frozen and visible once public; cannot be undone). Deleting them only after step 2 keeps the package alive: removing every version may delete the package, and a recreated package is private again [call, review].
4. Make the package public: https://github.com/users/Barben360/packages/container/package/proton-calendar-bridge (Package settings → Danger Zone → Change visibility), then check the rc image pulls with no login.

## 6. Left to the builder

- Reusable workflow versus composite action for the image build; job names and layout.
- Exact action versions (pinned by SHA), the Debian 12 image digest for `pcb-ui`.
- Messages of `install.sh` and of the release checks, within the rules above.
- Whether the release workflow calls `image.yml` or shares the build steps otherwise, as long as the release has one image build.
