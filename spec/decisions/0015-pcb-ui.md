# 0015 — pcb-ui: a separate cgo binary that runs the daemon in process

- **Date**: 2026-11-13
- **Decided by**: author (a separate binary, the daemon in process, Linux first, the keyring sealer built in M7), the rest the agent's calls (M7 design, `spec/designs/2026-11-12-multi-account-and-ui-design.md` §5, and the plan `spec/plans/2026-11-13-m7-pcb-ui.md`)
- **Status**: accepted, built in M7 (live session pending, `spec/HUMAN-CHECKS.md` step 9, in the throwaway VM with a GNOME session); amends 0001 (GUIs are no longer only "later clients of the local API": one is built, and it runs the daemon itself), SPEC §3 and §7.3 (the Secret Service sealer is built)

`pcb-ui` is a desktop app for Linux (X11 and Wayland with a StatusNotifierItem tray): a tray icon, a window with the accounts, an add-account wizard, desktop notifications. It is a **second binary**, `cmd/pcb-ui`, built with cgo (Fyne draws through OpenGL). `pcb` stays `CGO_ENABLED=0` and the container image stays distroless static: `cmd/pcb/arch_test.go` (`TestPcbHasNoGUIDependency`) fails when `go list -deps ./cmd/pcb` holds a `fyne.io/` package.

- **The daemon in process.** `pcb-ui` runs `internal/daemon` itself (`ui.Bridge`, daemon kind `ui`), with the same CalDAV listener, control socket and `serve.lock` as `pcb serve`. No subprocess and no CLI call: the window reads and acts through the `service.Hub` in the same process. `pcb status`, `pcb client show` and Thunderbird work the same whichever of the two runs. Quit drains (5 s) and stops everything; closing the window only hides it.
- **One server per state directory.** `pcb-ui` and `pcb serve` (or the systemd service) cannot both serve one state. A second `pcb-ui` asks the first to show its window (`POST /v2/ui/show`, served only by a `pcb-ui`) and exits 0; a `pcb-ui` that meets `pcb serve` shows a dialog naming the PID and exits 20. `pcb doctor` warns when `pcb-ui.desktop` and the pcb unit are both installed.
- **The keyring sealer.** The same milestone builds the Secret Service sealer (SPEC §7.3) so a desktop state is sealed by the login keyring and not by a key file next to the data. `pcb` and `pcb-ui` both open what the other sealed, because the sealer is recorded in `layout.json` and its name is in the AAD of `secrets.sealed` and `clients.json`.
- **A thin toolkit layer.** Everything that can be tested without a display is in `internal/ui` (view model, actions, wizard, notification rules; never imports Fyne). `internal/ui/desktop` is the Fyne layer, tested on Fyne's headless driver. The GUI itself (a real tray, a real notification daemon, a real keyring prompt) is a human step (ruling R11, SPEC §16).

Why:
- The author wants Proton Mail Bridge's desktop experience and the same codebase headless. A subprocess `pcb serve` under a UI would need a second protocol for what the Hub already offers in process, and would let the two drift.
- A separate binary keeps cgo, the OpenGL and X11 headers and a large dependency tree out of `pcb`, the image and the units.
- Fyne is Go-only, has the tray (StatusNotifierItem), notifications and a headless test driver, and can follow to Windows later. Wails (HTML and CSS, closer to Bridge's look) would bring a JavaScript toolchain. The author may overrule.

Consequences and limits:
- Building `pcb-ui` needs the GL and X development headers (`README.md`, "Build"); it is not part of `make build`. CI installs them in the jobs that compile it.
- GNOME shows no tray icon without the AppIndicator extension; `pcb-ui` then opens its window and says so once.
- Linux only. Windows is next, macOS is not planned. The UI logic hard-codes Linux only in `platform_linux.go` and `cmd/pcb-ui/*_linux.go`.
- Fyne and godbus are new dependencies, credited in `README.md` (SPEC §20); nothing was copied.
- Cost if wrong: the Fyne layer is replaceable (`internal/ui` is toolkit-free); the daemon-in-process choice would cost a CTL client in the UI.
