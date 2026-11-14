# M7-live — The desktop app and the keyring sealer against the test account

Status: **HUMAN-CHECKS step 9 run on 2026-11-13** (9.1 to 9.12; 9.2.4 and 9.11.5 were optional and not run; 9.2.4, 9.5.2, 9.5.3 and 9.12.3 were run by the agent on 2026-11-14, last section). Ten findings, all fixed (table at the end, each fix named by its commit subject: the merge into main keeps the subjects, not the branch's hashes); one check could not pass because of a defect (9.12.3, F10) and is for the next live run. Feeds SPEC §7.3, §13, §15, §16 and `spec/OPEN.md` "Parked by M7".

## Setup

- A throwaway desktop VM: Debian 13 with GNOME 48, the "AppIndicator and KStatusNotifierItem Support" extension enabled, Seahorse and Thunderbird, fenced like step 7 (its own run root, ledger and `live --vm` guard). The author worked at its console (virt-manager's viewer).
- Build under test: main **9bb1f79** (the `live-fixes` merge), `pcb` and `pcb-ui` built into the run root. `make rehearse` PASS, `make live-preflight ... STEP7=1` ok before 9.1 (ledger 0/3), 127.0.0.1:1080 free.
- pcb ran with the VM's default directories (`~/.config/pcb`, `~/.local/state/pcb`).
- Hybrid mode, as in step 7: the author did everything at the desktop (the window, the tray, Seahorse, Thunderbird, the web app) and typed the login; from 9.5 on, at the author's request, the agent ran the `pcb` commands over SSH with the session's bus where a step needed it (the lock of 9.5, the reads of 9.5 to 9.12). The agent never saw a credential.
- All times are UTC. Everything written to the account was titled `pcb-m7 ...` and deleted at 9.12.
- **Logins: 3**, the step's whole budget. 9.3's first attempt reached Proton and got a CAPTCHA (login 1), its third attempt got a CAPTCHA (login 2) and the retry after it was solved logged in (login 3); the second attempt was refused by pcb's own login guard before any Proton call. Ledger rows 2 and 3 were added by hand in both ledgers (the wizard's logins are typed, not run by `live`). The stop rule "a second CAPTCHA stops the step" was met on paper only: the first attempt's CAPTCHA was never shown to the author (F2). Step 10, which shares this ledger, has no login left without `--extra-login`.

## 9.1 A fresh state picks the keyring: PASS on the second attempt (F1)

- **First attempt, 22:41Z**: `pcb-ui` chose `keyfile`. The VM's `login` collection was listed on the bus but not usable: GNOME's own start of the keyring daemon had vanished at the first login (22:38:10Z) and a socket-activated daemon ran instead, so the keyring was not unlocked by the login. `[secrets] backend = auto` fell back to the key file **with no trace**: no log line, and `pcb doctor` said only `sealer: keyfile` (**F1**). Recovered by hand: `systemctl --user restart gnome-keyring-daemon`, the author unlocked the Login keyring in Seahorse; no login had happened, so the state was removed and the preflight run again (ledger still 0/3). HUMAN-CHECKS gains a prerequisite: the Login keyring loaded and unlocked.
- **Second attempt, 22:48Z**: 9.1.1 and 9.1.2 as expected. 9.1.3: `layout.json` says `secret-service`, `pcb doctor` ok, no `secrets.key`. Seahorse's Login had **no** `pcb sealing key` item and the bus listed none for `application=pcb`: correct, since SPEC §7.3 creates the item at the first seal (the login), and HUMAN-CHECKS 9.1.3 wrongly expected it at the start; the check moves to 9.3.3.
- 9.1.4: `pcb-ui.log` 0600 with its startup line, no password; but no line for anything the login did (**F3**, below).

## 9.2 Tray states: PASS (9.2.4 not run)

- 9.2.1 (~22:50Z): the attention icon with no account, the menu `Open pcb` and `Quit`, pcb's own icons. 9.2.3: `Open pcb`, the X hides, the process and the daemon stay.
- 9.2.2 (with 9.3.3): syncing, then ok; the account's line in the menu; a click opens the window on it.
- 9.2.4 (no extension) was optional and not run: the tray without the AppIndicator extension stays unchecked live.
- From the first start (22:41:45Z) gnome-shell logged an AppIndicator `PropertyNotFound` error every 30 s (**F4**). The tray worked.

## 9.3 The wizard: PASS on the third attempt (F2, F5)

- Ledger slot taken at 22:52:54Z.
- **Attempt 1** (Sign in, `Repair the session without me` unticked): under the unticked box the window showed `unattended relogin is on: whoever can use this desktop session's keyring ...`, a sentence that was false (**F5**). Proton answered with a CAPTCHA; notifications came; the author clicked the `needs-captcha` one, and the window fell back to an empty, logged-out wizard with no CAPTCHA step ever seen (**F2**, confirmed with the author during 9.4).
- **Attempt 2**: refused at once, `too soon, try again in 10 s` (pcb's 60 s login guard; no Proton call).
- **Attempt 3**: the CAPTCHA step; `Open verification` opened the browser at 22:54:55Z; solved; `I solved it` then logged in (the `last-login` stamp 22:56:38Z). The 2FA screen came and took the code. Client `default` created 22:56:51Z, first pass 22:57:27Z (403 events added), then every 30 s.
- **9.3.3** (23:03Z): the account `ready`, CalDAV on; `pcb doctor` exit 0 with `sealer: secret-service`, `keyring` unlocked, `secrets sealed with the desktop keyring`, no config directory and no `secrets.key`; `pcb status --json` `secret-service`, `sealed`, `""`; Seahorse **one** `pcb sealing key` item. Observation: pasting `pcb doctor`'s output from the SPICE console showed its glyphs as literal `✓` text (the console's clipboard or locale; not investigated).
- 9.3.5: `Show`, the mask again after a pane change, `Copy`. The author asked for `Copy` buttons for the URL and the username too (**F6**).

## 9.4 Thunderbird by copy and paste: PASS

9.4.1 to 9.4.3 (reported by the author at about 23:2xZ): the calendars appear and match; an event edit (`pcb-m7 ...`) reached the web app; `New password` asked to confirm, Thunderbird re-prompted on its next refresh and synced again.

## 9.5 and 9.11 The keyring lock: PASS (F7; the click stays open)

- `last_login` before the lock: 22:56:50.098Z. The Login keyring was locked at 23:13:53Z through the Secret Service's own `Lock` call on the bus (the agent; the same as Seahorse's menu for pcb).
- **Noticed within 14 s**: at 23:14:07Z `sync pass failed ... the desktop keyring is locked`, then a Warn **`waiting for a login`** (**F7**: no login is needed), and `pcb status` said `serve: waiting for a login since 23:14:07` (F7 again). From 23:14:37Z, `waiting for the desktop keyring ... retry=30s` every 30 s (9 lines up to 23:18:37Z).
- `pcb status`: phase `ready`, `secrets: locked`, the alert `error keyring-locked` with its fix. `pcb doctor`: `! keyring: the desktop keyring is locked` with the fix, the alert as `✗`, exit 10.
- 9.5.1 and 9.5.2 (author): the notification arrived. Its click "did nothing" visible: the window was already open on that account. So whether GNOME delivers a click to an app whose window is hidden was **not observed** (it stays open in `OPEN.md`). The window showed the alert and its fix. 9.5.3 (no second notification) was not recorded.
- 9.11.2: Thunderbird showed nothing until its refresh, then its calendars as unavailable; the web app's session list did not change; no login.
- **9.11.6** (23:14:47Z, SSH without a session bus): `pcb status` showed **the running `pcb-ui`'s view** (`secrets: locked`, the alert `keyring-locked`, phase `ready`, never `needs-login`), not `secrets: keyring-unavailable`: that is the offline view's (no daemon answering), so HUMAN-CHECKS 9.11.6 expected the wrong view (its text is corrected). `pcb sync`: `the desktop keyring is not available` with the desktop-session hint, exit 20, as expected.
- 9.11.3 and 9.11.4: unlocked in Seahorse at about 23:18:4xZ. Serving again at 23:19:07Z, a pass at 23:19:09Z; `secrets: sealed`, no alert, `last_login` unchanged (22:56:50.098Z: no login), `layout.json` still `secret-service`, one keyring item. Minor: nothing is logged when the keyring comes back (a line `keyring unlocked, resuming` would close the wait in the log). 9.11.5 (lock again) was optional and not run.

## 9.6 Quit and a signal: PASS

- 9.6.1 (23:21:12Z): after Quit no `pcb-ui` process, `pcb status` the offline view, exit 0, nothing on :1080, `pcb.sock` gone. Started again (as a user unit run by the agent): serving, `last_login` unchanged.
- 9.6.2: `SIGTERM`: gone in 2 s, a clean stop.

## 9.7 A second `pcb-ui`: PASS (checked twice)

The first try's window was not watched; re-checked at 23:25:30Z with `pcb-ui --background` running: the second `pcb-ui` returned exit 0 in 0.09 s, one process, and the author saw the window open.

## 9.8 and 9.9 `pcb serve` running, the dialog: PASS (F8, F9)

- 9.8.1: `pcb serve --wait-for-login` (as a user unit), serving.
- The first conflicting `pcb-ui` was launched over SSH, where the X display was set but not authorised: it printed Fyne's raw `failed to initialise GLFW ... Failed to open display` and exited 2 (a panic), with no pcb message (**F8**). Launched again inside the session.
- 9.8.2: the dialog's text, PID and hint exactly as written; `OK`: exit 20, no `pcb-ui` left. The dialog card was narrower than its window, with black bars left and right (**F9**).
- 9.9: closed with the title bar's X: exit 20 at 23:24:02Z, **no process left**. This settles the open item: a master window's close does quit.
- 9.8.3: `pcb serve` stopped; `pcb-ui --background` serving again at 23:24:59Z, `last_login` unchanged.

## 9.10 Autostart: PASS

- 9.10.1: `~/.config/autostart` created 0700, `pcb-ui.desktop` with `Exec=<the run root's pcb-ui> --background`, its comment and icon.
- 9.10.2: GNOME logout and login at about 23:26:30Z: `pcb-ui` started by itself at 23:26:44Z, tray only, the keyring unlocked by the login this time, serving at 23:26:45Z with a pass at once, `last_login` unchanged. Observation O2: the `pcb-ui` the agent had started as a user unit logged no clean stop at the GNOME logout (killed with the session); not the autostart path.
- 9.10.3: off: the file gone, the directory (0700) kept.

## 9.12 Cleanup: partly (F10)

- 9.12.1 and 9.12.2 (about 23:29Z): the `pcb-m7` event and the calendars deleted in Thunderbird; the window's `Remove` dialog as written; `pcb account list` empty; the web app's session list without the pcb session.
- 9.12.3: Quit; the keyring item was kept after `Remove`, as designed. Then **no pcb command could clean this install** (**F10**): `pcb uninstall --delete-data --yes` exit 10 `pcb is not installed as a service`, and `--user --delete-data --plan` exit 10 `pcb.service sets no absolute PCB_CONFIG_DIR and PCB_STATE_DIR`. Cleaned by hand at about 23:3xZ: the `pcb sealing key` item (application `pcb`, the state's path) deleted through the Secret Service's `Delete` call, `~/.local/state/pcb` removed; `~/.config/pcb` had never been created; the autostart directory was empty. So `pcb uninstall --delete-data` removing a real keyring item is **still unverified live** (HUMAN-CHECKS 9.12.3 now runs it without a service).

## Findings and their fixes

| # | Finding | Fix | Commit (subject) |
|---|---|---|---|
| F1 | `[secrets] backend = auto` fell back to the key file silently (the probe's error was dropped) | the choice is logged (Warn with the reason on a fallback), the reason kept in `<state>/sealer-choice.json`, `pcb doctor`'s `sealer: keyfile (chosen at creation: <reason>)` | "Say why [secrets] backend = auto chose the key file" |
| F2 | clicking pcb-ui's own `needs-captcha` notification for the pending add ran `Window.Show` → `selectAccount` → `closeWizard` → `Wizard.Cancel` → `Hub.Remove`: the add was deleted | a wizard on the 2FA or CAPTCHA step is kept when an account is shown (notification, tray line, the add's row); the Notifier sends nothing for a pending add | "Keep a wizard waiting for 2FA or a CAPTCHA when an account is shown"; after review, "Keep a Sign in in flight from an agent's login" |
| F3 | no login event was logged anywhere | the Hub logs each login's start (`entry`) and outcome, removals with `by`, and pcb-ui each notification sent; local IDs, no secret or address | "Log every login, the adds removed and the notifications sent"; after review, "Log a login refused before it starts as login refused" |
| F4 | gnome-shell's AppIndicator logged `PropertyNotFound` every 30 s: the tray re-sent an unchanged icon, and Fyne's systray lacks `IconAccessibleDesc` | the tray re-sends its icon and menu only on a change; the upstream gap in `OPEN.md` | "Re-send the tray icon and menu only when they change" |
| F5 | `unattended relogin is on ...` under the unticked box, worded for the keyring on any state | a sentence true either way, for the state's sealer (`If ticked: whoever can ...`), in the wizard and on the MCP login page | "Word the unattended-relogin line for the box and the state's sealer" |
| F6 | (the author's request) no Copy for the URL and the username | a Copy for each, in the account pane and on the wizard's Done screen | "Copy the CalDAV URL and username like the password" |
| F7 | a locked keyring was logged `waiting for a login`, and `pcb status` said `serve: waiting for a login since ...` | `waiting for the desktop keyring` in both (the health's `waiting_for: keyring`) | "Say a locked keyring is waited for, not a login" |
| F8 | no usable display: Fyne's raw GLFW error and a panic, exit 2, after opening the state | GLFW is opened first; `pcb-ui cannot open the display (...)` with the hint, exit 10, nothing touched | "Say what is missing when pcb-ui cannot open the display" |
| F9 | the startup error dialog had black bars left and right | the error is the window's own content, the window sized to it | "Size the startup error window to its message" |
| F10 | `pcb uninstall --delete-data` could not clean a desktop-only install (keyring item by hand only) | without a service it deletes this invocation's directories, the keyring item and the autostart entry; refused while a daemon serves the state | "Let pcb uninstall --delete-data clean a desktop-only install"; after review, "Keep what a desktop uninstall does not own, and name all it deletes" |
| — | HUMAN-CHECKS step 9's text (9.1.3's Seahorse timing, 9.11.6's view, the keyring prerequisite, the F2 check in 9.3) | corrected | "Correct HUMAN-CHECKS step 9 after its live run" |

## What the next live run checks

9.12.3 with the fixed `pcb uninstall --delete-data` (the real keyring item removed); 9.3 with a CAPTCHA, clicking the add's row and tray line (no notification comes for it any more); the startup window without black bars (9.8.2); no `PropertyNotFound` every 30 s (9.2.3); the login lines in `pcb-ui.log` (9.1.4); a notification click with the window hidden (9.5.2); the tray without the extension (9.2.4).

## Follow-up run, 2026-11-14 (agent): 9.2.4, 9.5.2, 9.5.3 and 9.12.3

The three checks the run above left open, run by the agent alone on the same desktop VM (GNOME Shell 48.7, Wayland, GDM autologin), build v0.1.0-4-ge8adf96 (`pcb-ui` built on the VM from that commit). **Against `pcb-fake`, not Proton**: these checks are about GNOME and GNOME Keyring, not about Proton, so the account was the fake's (a loopback `pcb-fake` seeded with the data set; `[proton] base_url` in the VM's default `config.toml`, CalDAV on 127.0.0.1:1081). No Proton login. The VM's own default dirs and real login keyring were used, through `run.sh --vm`. The agent saw the screen through `virsh screenshot` and clicked through QEMU's absolute pointer (`input-send-event`); notifications were traced with `dbus-monitor` on the session bus; the keyring was locked with the Secret Service's `Lock` call and unlocked with `gnome-keyring-daemon --unlock` (the session password on stdin). Times UTC.

- **9.5.2: GNOME delivers a click to `pcb-ui` while its window is hidden, from the banner and from the message tray: PASS.** The window hidden with its X (the process and tray icon stayed), the keyring locked at 19:25:55Z; the notification (`Notify`, app `pcb`, title the account, body `the desktop keyring is locked unlock it (log in to the desktop session), pcb…`, a `default` action) came at 19:26:20Z (25 s). A click on the banner: `ActionInvoked` `default` reached `pcb-ui`, and the window opened on that account with `keyring locked` and the fix. Second occurrence (unlocked, resumed, hidden, locked again at 19:27:32Z, notified 19:27:50Z): the banner left to expire, the entry clicked in the message tray (opened from the clock) at about 19:28:10Z: the same `ActionInvoked`, the same window. The tray entry was removed after its click.
- **9.5.3: one notification per lock: PASS.** After the second lock, no further `Notify` in the 100 s that followed while it stayed locked.
- **9.2.4: no tray (AppIndicator extension disabled): PASS.** `pcb-ui --background` opened the window anyway, with the line `your desktop shows no tray icons; on GNOME install the AppIndicator extension` above the account. Closing it with the X quit `pcb-ui` within a second (`pgrep -x pcb-ui` empty) and `pcb status` showed `daemon: not running (offline view)`. A second `pcb-ui --background`: the window opened without that line. The extension was enabled again. Seen on the way: this process's notifications were labelled `io.github.barben360.pcb-ui` (the app ID) where the earlier process's were labelled `pcb`; harmless.
- **9.12.3: `pcb uninstall --delete-data` removes the real keyring item: PASS** (F10's fix, live). The state was made by `pcb login` (to the fake) with `backend = auto` on the unlocked keyring: one item `pcb sealing key` in the `login` collection. With `pcb-ui` running: `pcb uninstall --delete-data --yes` exit 20 `pcb is already serving (pid N, kind ui)`, nothing deleted. After `pcb-ui` stopped, from an SSH shell given the session bus: `--plan` listed `run logout`, `remove dir ~/.config/pcb`, `remove dir ~/.local/state/pcb`, `remove keyring … the "pcb sealing key" item of this state in the desktop keyring` and the note `pcb is not installed as a service`; `--yes` did it, exit 0. Afterwards the `login` collection held **no item**, both dirs were gone, no autostart entry. No `systemctl`.
- Left as in the run above: nothing is logged when the keyring comes back (`pcb-ui` showed `synced just now` again within 35 s of the unlock).
