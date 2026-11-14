# 0017 — Tag-driven releases, attestations, a public image

- **Date**: 2026-11-14
- **Decided by**: author (a protected semver tag publishes the release; the image pushed only on release, plus a manual build of any commit; the package public; a `curl | sh` installer for Linux and macOS; GitHub attestations rather than minisign; no Windows MSI until `pcb` and `pcb-ui` are ported), the rest the agent's calls (design `spec/designs/2026-11-14-release-process-design.md`)
- **Status**: accepted; built on branch `release-process`, rollout pending (the tags and the package settings are the author's steps, design §5.7); amends SPEC §3 (updates and signed releases leave "not planned") and SPEC §13 ("Until the repository is public" and "Later, when public"); replaces the minisign [call] of §13

Releases are made by a tag, not by a push to main.

- **Tag-driven.** A tag `vX.Y.Z` (or `vX.Y.Z-rc.N`, a pre-release) runs `release.yml`. It refuses a tag off main, a tag whose `ci` run did not succeed, a tag already released, and anyone but the owner (a ruleset on `refs/tags/v*` restricts creation to the owner). It builds, attests, pushes the image and publishes the Release last.
- **Attestations, not minisign.** GitHub artifact attestations (Sigstore, keyless) cover the archives, `checksums.txt`, `install.sh` and the image digest: no key to keep, one tool (`gh attestation verify`) for files and image. This replaces the minisign [call] of §13.
- **Image only on release.** `X.Y.Z`, `X.Y` and `latest` (the newest stable by version) on a stable release; a pre-release gets only its own tag; a manual workflow pushes `sha-<12 hex>` for any commit. Nothing is pushed from main. The package is public.
- **Linux and macOS archives.** `pcb_<os>_<arch>.tar.gz` for linux and darwin, amd64 and arm64; `pcb-ui` in the Linux ones only, built on Ubuntu 22.04 (glibc 2.35). `install.sh` places files under `~/.local` with no sudo and does not run `pcb install`.
- **Windows after its port.** `pcb-ui` does not run on Windows and `pcb login`, `sync` and `serve` exit 20 there, so no Windows archive and no MSI until that port is done, as its own project.

Why:
- A tag is a deliberate act: a release cannot come from an unreviewed or red commit, and a published version is never rebuilt.
- Attestations cover the image too and need no secret; minisign signs binaries only and needs a key guarded forever.
- A private image on every push to main gave users nothing to pull and left frozen versions to expose once public.

Consequences and limits:
- macOS is untested by the author (`spec/OPEN.md`); CI checks the install and that the binary starts.
- A user without `gh` verifies nothing beyond the checksum, which comes from the same release.
- Deleting the old `:main` and `sha-` versions and making the package public are the author's steps in the package settings.
- Cost if wrong: `release.yml`, `image.yml`, `scripts/install.sh` and `scripts/release/`; the `main` push of the image is easy to restore.
