# Update availability check — design

- **Date**: 2026-11-15
- **Status**: built 2026-11-15 (decision 0019). Where the built code differs from §5, the paragraph says so, marked "Built:"
- **Decided by**: author (choices marked "author"); the rest is the agent's call, marked [call]
- **Amends**: decision 0015 (pcb-ui: a new banner, tray menu line and Settings switch), SPEC §3, §13, §19 (a new outbound host); recorded as decision 0019

## 1. Intent

A user of a released pcb should learn that a newer stable release exists, and where to get it, without watching the GitHub repository. Part of the reason is security (author): a fix only protects the people who install it, so the reminder must come back until the user updates.

No auto-update: pcb only tells, the user installs (author).

## 2. Goals, non-goals, anti-goals

Goals:
- `pcb-ui` shows, while a newer stable release exists: a panel at the top of the window, a line in the tray menu, one desktop notification per new version.
- `pcb version --check` and `pcb doctor` say the same on the command line (author: pcb-ui plus these two; no background check in the daemon).
- One switch turns the check off.

Non-goals:
- Auto-update, downloading or verifying a release.
- Pre-releases: the channel announces stable versions only (author).
- A background check in the daemon (`pcb serve`, systemd, container); container users update by pulling a new image.
- Telling pcb-ui that the running daemon and the installed binary differ after an update.

Anti-goals (failure even if it works):
- A check that makes anything fail: an offline machine, GitHub down or a changed redirect must never raise an alert, turn the tray to `attention`, change an exit code, or slow startup.
- A release or CI run that depends on the network because of this feature (plain `pcb version` stays offline: CI and `release.yml` run it to check the printed tag).
- A dev build nagging its developer.
- Sending anything that identifies the user or the installed version to GitHub.

## 3. Constraints

- Repository rules (`AGENTS.md`, `CONTRIBUTING.md`): `--json` envelope, exit codes, tests that bite, `make lint test`, gitmoji commits, squash-merged PRs.
- Import rules (`arch_test.go`): the new package follows the existing layering; `pcb` gains no cgo or GUI dependency.
- `pcb-ui` runs on Linux and Windows; `pcb` on Linux, macOS and Windows (the Windows port is in progress on `windows-port`). Windows installs from a zip with no install line (`docs/install.md`).
- Tests never reach github.com: the release host is a seam the tests point at an `httptest` server.

## 4. Approaches

| | Source | Trade-off |
|---|---|---|
| A | GitHub REST API, list releases | Sees pre-releases (not wanted); unauthenticated API limit of 60 requests per hour per IP |
| **B (author)** | `GET https://github.com/Barben360/proton-calendar-bridge/releases/latest` without following the redirect; the `Location` header ends in `/releases/tag/vX.Y.Z` | No API, no JSON, one small request; stable only by construction (GitHub's "latest" skips pre-releases and drafts); relies on the redirect's shape, so a different shape counts as a failed check |
| C | A `latest.json` published by `release.yml` | Own format, but a workflow change and a second source of truth |

Picked: **B** (author), because the channel is for stable versions only and it needs no API. Verified on 2026-11-15: `302`, `location: https://github.com/Barben360/proton-calendar-bridge/releases/tag/v0.1.0`.

## 5. Design

### 5.1 The check (`internal/update`, new) [call]

- One request: `GET` the URL above, redirects not followed, 10 s timeout, a generic `User-Agent: pcb` (no version, no OS), no cookie, its own `http.Client` (never the Proton client). A `HEAD` is fine if GitHub answers it the same way [builder's call].
- The answer counts only if it is a `302` whose `Location` path is `/Barben360/proton-calendar-bridge/releases/tag/vMAJOR.MINOR.PATCH` (no suffix). Anything else is a failed check.
- Compared with `buildinfo.Version` by semver. An update exists when latest > installed. Examples: installed `v0.1.0-rc.1`, latest `v0.1.0` → update; installed `v0.2.0-rc.1`, latest `v0.1.0` → none.
- No check at all when the installed version is not a clean `vX.Y.Z` or `vX.Y.Z-PRERELEASE` (`dev`, `v0.1.0-3-gabc`, `-dirty`): the result is "skipped: dev build".
- The release page opened is the `Location` URL itself (the tag's page, with its notes).
- Result: `{latest, url, checked_at}` or a failure reason; never an error that callers must surface.

### 5.2 State and the switch [call]

- One new file in the state directory, `update.json` (owner-only like the rest of the state), written directly by whoever checks, with no daemon involved, so it works while the bridge is stopped. It holds the switch and the cache (when it last checked and what it found). A restart within 24 h reuses the saved result and sends no request. pcb has no per-machine settings store today (the adapter switches are per account, in `accounts/<id>/state.json`, and go through the daemon), and pcb-ui never writes `config.toml`, hence a file of its own.
- Several processes write it (pcb-ui, `pcb doctor`, `pcb version --check`, the switch command), so every write is a read-modify-write under `internal/filelock`, then `internal/atomicfile`: refreshing the cache never erases a switch just flipped (review gap 4).
- Built: a write never creates the state directory (a `pcb doctor` before the first login keeps no cache); `pcb update-check on|off` creates it as pcb does (`acl.MkdirAll`, after the state-owner check). A failed re-check keeps the last answer on screen in pcb-ui, so the panel does not come and go with the network.
- Writes are best effort: a state directory pcb cannot write (a system service's state read by another user, a read-only volume) only loses the cache, never fails a command.
- What only pcb-ui uses, the version already notified and the "Later" deadline, lives in pcb-ui's Fyne preferences, as `noTrayLineShown` does (`internal/ui/desktop/tray.go:103`).
- One switch, "Check for updates", on by default. pcb-ui Settings shows and flips it. On the CLI: `pcb update-check on|off|status` [name: builder's call]. `pcb doctor` and pcb-ui honour it; `pcb version --check` is an explicit request and always checks.
- `PCB_UPDATE_CHECK=off` in the environment turns it off for that process whatever the file says (except `version --check`). The smoke scripts that run `pcb doctor` set it where that process actually gets it, so no CI or release run calls github.com or waits on its timeout:
  - `scripts/release/macos-smoke.sh:48`: in its `env HOME=…` (CI stamps `v0.0.0-ci`, a clean version that would check);
  - `scripts/systemd-smoke.sh:45-47`: in the `as_tester` `env` list (an outer `export` does not cross `runuser`);
  - `scripts/container-smoke.sh:121`: at `docker run` (`docker exec` takes the container's environment).

### 5.3 pcb-ui

- When: at start (after the window or tray is up, never blocking it), then every 24 h while running.
- **Panel** at the top of the window, information style (not the alert style of the "bridge stopped" banner): `pcb v0.2.0 is available (you have v0.1.0).` Buttons:
  - `Open release page`, on every OS: opens the tag's page in the browser.
  - `Copy update command`, Linux only (pcb-ui has no macOS build): copies the install line of §5.5. Same Copy pattern as the `claude mcp add` box.
  - `Later`: hides the panel for **48 h**, saved across restarts; after that it comes back, also for the same version (author, for security). A newer version does not cut the 48 h short.
  - Where the "bridge stopped" banner also shows, it comes first (more urgent) and the update panel goes under it [call].
- **Tray**: the icon does not change (its colour stays bridge health only) [call]. The menu gets `Update available: v0.2.0` above `Open pcb`, shown as long as an update exists (Later doesn't hide it); it opens the release page.
- **Notification**: one per new version (not repeated every 48 h; the panel is the reminder) [call]. On Linux clicking it opens the release page; on Windows Fyne reports no click (`internal/ui/desktop/platform_other.go`), so there it only informs, and the panel and tray line carry the action [call]. Today a notification is an account alert (`ui.Notification{Account, Code, Title, Body}`, `internal/ui/notify.go:15`) and its click always shows that account (`cmd/pcb-ui/main_linux.go:146`): the notification gains a URL, and the click handler opens it when set, else shows the account.
- **Wiring** (review gaps 2, 3, 5):
  - The update result enters `ui.View` (a new field), so the window and the tray draw it like the rest. Built: it reaches them through `Window.SetUpdate` and `Tray.SetUpdate`, from its own loop (`ui.Updates.Run`): `ui.View` is built from the bridge's status.
  - `Tray.Update` skips a menu whose key did not change (`internal/ui/desktop/tray.go:51`, the GNOME F4 fix): the update line is part of that key, or it never appears or goes away.
  - The loop in `cmd/pcb-ui` redraws only on bridge changes (`follow()`, `main_linux.go:140`): a timer of its own runs the 24 h check and redraws when the "Later" deadline passes.
  - The doctor `update` check takes its release URL, HTTP client, clock and state path as parameters of `doctor.Checks`, never package globals. `doctor.Checks` also runs in pcb-ui's Doctor report (`internal/ui/model.go:356`), which then shows the line from the cache. No test reaches github.com, even one that stamps a clean version. Built: the line is appended by `pcb doctor` itself (`internal/cli/update.go`, after the daemon line), not in `doctor.Checks`, so pcb-ui's Doctor report sends no request and has no update line (its panel shows it); the checker is `cli.IO.Update`.

### 5.4 CLI

- `pcb version` unchanged and offline.
- `pcb version --check`: checks now (ignores the 24 h cache and the switch). Text: the version line, then `update available: v0.2.0 <url>` (plus the hint of §5.5), or `up to date`, or `check skipped: <reason>` / `check failed: <reason>`. `--json`: `data.update` with `latest`, `url`, `available`, `error`. Exit 0 in every case [call].
- `pcb doctor`: a line `update`: `ok` (up to date), `warn` with the release URL as fix (plus the hint of §5.5), or `skip` (switched off, dev build, or failed, with the reason). A `warn` does not change doctor's exit code (already so: only `error` does). Uses the 24 h cache.

### 5.5 The update hint, by install kind [call]

| Install | Hint after the release URL |
|---|---|
| Linux or macOS, from `install.sh` | the install line, rebuilt from what is installed: `--prefix DIR` when the binary is not under `~/.local/bin` (DIR is the parent of its `bin/`); `--no-ui` when no `pcb-ui` sits next to `pcb`; `--no-desktop` when `pcb-ui` is there but no `DIR/share/applications/pcb-ui.desktop`. With no flag: `curl -fsSL https://github.com/Barben360/proton-calendar-bridge/releases/latest/download/install.sh \| sh`; with flags, `\| sh -s -- FLAGS`. |
| Container image | `pull the new image: docker compose pull && docker compose up -d`. The image says it is one through `PCB_INSTALL=container`, a new `ENV` in the `Dockerfile` (no detection heuristics). |
| Windows (zip or the per-user MSI of `win-msi`) | none: the release page is the hint. |

### 5.6 Known limits [call]

- `/releases/latest` is the newest stable of the newest line (`release.yml` publishes an older-line fix with `--latest=no`): a user on v0.1.x is pointed at v0.2.0, never at a v0.1.y fix.
- macOS has no pcb-ui, so it gets only `pcb doctor` and `pcb version --check`.

### 5.7 Branch [call]

Built on `main`. The Windows-specific parts (no notification click, no install line) are a `runtime.GOOS` check, which `windows-port` picks up at its next merge of main.

### 5.8 Docs

- `docs/security.md`: the new outbound host (github.com), what is sent (nothing but the request), how to turn it off.
- `docs/desktop.md`: the panel, the tray line, Later's 48 h.
- `docs/commands.md`: `pcb version --check`, the `update` doctor line, the switch command, `PCB_UPDATE_CHECK`.
- `docs/container.md`: what `pcb doctor` says in the image.
- SPEC and decision 0015: amended for the new host and pcb-ui surfaces.

## 6. Tests that bite

- `internal/update` against an `httptest` server: a stable tag newer, equal, older; an rc installed below and above the latest stable; a `Location` with a pre-release tag, another repository, a missing header, a `200` or `404`; a timeout; dev versions skipped with no request sent (the server counts requests).
- The 24 h cache: a second call within 24 h sends no request; after 24 h it does.
- Later: hidden before 48 h, shown after, survives a restart, comes back for the same version.
- pcb-ui (existing fake-app test style): the panel and the tray line appear and disappear with the result; `Copy update command` exists on Linux and not on Windows; one notification per version.
- The hint: each row of §5.5, and each install-line flag added only in its case (default prefix, other prefix, no `pcb-ui`, no desktop entry).
- The switch: off in the file, and `PCB_UPDATE_CHECK=off`, each send no request (server counts); `version --check` still does.
- Concurrent writes: a cache refresh racing a switch flip keeps the flip.
- Tray: with accounts unchanged, the update line still appears and disappears (guards the F4 skip).
- Notification click: one with a URL opens it, one without shows its account.
- An unwritable state directory: the check still answers, the command does not fail.
- CLI: `version` sends no request; `version --check` text and `--json`; doctor's `update` line in each state and its exit code unchanged by a `warn`.

## 7. Left to the builder

- `GET` or `HEAD`, the name of the switch command, the JSON layout of `update.json`, panel styling within the existing theme.
