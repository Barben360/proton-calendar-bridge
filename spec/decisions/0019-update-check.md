# 0019 — Tell that a newer stable release exists

- **Date**: 2026-11-15
- **Decided by**: author (`pcb-ui`, `pcb version --check` and `pcb doctor` check, never the daemon; stable releases only, read from GitHub's `releases/latest` redirect, not the API; "Later" hides the panel 48 h and it comes back for the same version, for security; the release page on every OS, the copyable `install.sh` line only where it applies; the switch kept in pcb-ui's Settings; one maintained release line), the rest the agent's calls (design `spec/designs/2026-11-15-update-check-design.md`)
- **Status**: accepted; built 2026-11-15; amends SPEC §3 (auto-update stays not planned), §13 and §19, and decision 0015 (pcb-ui's panel, tray line and Settings switch)

pcb tells its user that a newer stable release exists, and where to get it. It never downloads nor installs one.

- **Who checks.** `pcb-ui` (at start, then hourly against a 24 h cache), `pcb doctor` (an `update` line from the cache) and `pcb version --check` (asks now). The daemon never does: a headless server calls github.com only when someone runs those commands.
- **The source.** `https://github.com/Barben360/proton-calendar-bridge/releases/latest`, redirect not followed: its `Location` names the newest stable tag. No API, no token, one request a day at most, nothing sent but the request (`User-Agent: pcb`).
- **What pcb-ui shows.** A panel at the top of the window ("Open release page", "Copy update command" on Linux, "Later"), a tray menu line above "Open pcb", one notification per version. The tray icon keeps meaning bridge health only.
- **Later.** 48 h, saved across restarts, then the panel is back, also for the same version: a fix protects only those who install it.
- **How to update, by install.** The `install.sh` line rebuilt with this install's flags (`--prefix`, `--no-ui`, `--no-desktop`) on Linux and macOS; pulling the image in the container (`PCB_INSTALL=container` in the image); the release page alone on Windows (a zip or the MSI).
- **The switch.** `update.json` in the state directory (switch and cache, under a file lock); Later and the notified version in pcb-ui's preferences. `pcb update-check on|off|status`, pcb-ui's Settings, and `PCB_UPDATE_CHECK=off` for one process (the smoke scripts set it, so CI and release runs never call github.com).

Why:
- A security fix only helps once installed, and nobody watches the repository.
- The redirect sees stable releases only, which is the channel wanted, and needs no API.
- A check from the daemon would make every server call github.com on its own.

Consequences and limits:
- `releases/latest` is the newest stable of the newest line: a user on an older line is pointed at the newest one, never at a fix of their own line. Fine while one line is maintained.
- macOS has no `pcb-ui`: only `pcb doctor` and `pcb version --check` tell.
- A change in GitHub's redirect makes the check fail silently (no alert), and nobody is told of updates until it is fixed.
- Cost if wrong: `internal/update`, `internal/ui/update.go`, the panel and tray line in `internal/ui/desktop`, `internal/cli/update.go`; removing the feature leaves an unused `update.json`.
