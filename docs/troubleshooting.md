# Troubleshooting

What to do when pcb stops, refuses or misbehaves. Each entry gives the symptom, its cause and the fix, and links to the page with the details.

## Start with doctor

```sh
pcb doctor
```

`pcb doctor` runs every check (service, daemon, listener, certificate, clients, sealer, key file; on Windows also the access lists, `elevated`, the DPAPI key, the time zone and "Start at login") and prints the fix for each failure. It exits 0 unless a check fails, 10 when you must act, 20 when only the daemon is unreachable. `pcb status` shows the live state and the alerts, each with its fix. In a stopped container, run it through `docker compose run` ([container](container.md#when-the-container-stops-or-restarts-in-a-loop)).

## Index

| Symptom | Cause | Fix |
|---|---|---|
| A command exits 10 | You must act: a login, a lost or unreadable key file, files written by a newer pcb | Follow the hint it prints; `pcb doctor`. [Exit codes](commands.md#exit-codes) |
| A command exits 20 | Blocked, often transient: no daemon, port in use, a Proton call past its 30 s deadline | Retry; `pcb doctor` names the cause. [Exit codes](commands.md#exit-codes) |
| A command exits 2 with `several accounts: name one with --account` | Several accounts and none named | Add `--account ADDRESS` or set `PCB_ACCOUNT`. [Choose the account](commands.md#choose-the-account) |
| `pcb status` shows `needs-login`; Thunderbird gets 503 | Proton ended the session | `pcb login` (or `pcb-ui`'s wizard). [Thunderbird](thunderbird.md#keep-serving-while-logged-out---wait-for-login) |
| `needs-captcha` | Proton asks for a CAPTCHA | Solve it, then `pcb login resume --captcha-token T`. [Security](security.md#captcha-timeouts-and-rate-limits) |
| `human-verification`; Thunderbird gets 503 | Proton asks for a human verification | Open the Proton web app from the same network and complete it. [Thunderbird](thunderbird.md#503-and-412-answers) |
| `account-stopped`; that account answers 503 | The account stopped on an error | `pcb account restart`, or a login. [Several accounts](thunderbird.md#several-accounts) |
| `keyring-locked` | The desktop keyring is locked (a service started before you logged in) | Unlock it (Seahorse); pcb retries every 30 s. [Keyring](security.md#when-the-keyring-is-locked) |
| `keyring-unavailable`, exit 20 `the desktop keyring is not available` | No keyring answers: pcb over SSH, or a service outside the desktop session | Run pcb inside the desktop session. Do not log out: the session is intact. [Keyring](security.md#when-the-keyring-is-not-available) |
| `clients-file-unreadable`; CalDAV answers 503 | `clients.json` cannot be read | Repair or restore it. [Request limits](thunderbird.md#request-limits) |
| `pcb client show` cannot show a password; doctor warns `caldav client NAME: password cannot be shown` | The client was sealed with a lost key, or created by a version from before passwords could be shown again | `pcb client rotate NAME`. [Container](container.md#when-the-container-stops-or-restarts-in-a-loop) |
| `pcb client add` or `rotate` refuses, exit 10 | No key file, but `secrets.sealed` present | Restore the key file, or `pcb logout` then `pcb login`. [Sealing key](security.md#the-sealing-key) |
| `permission denied` or `mode 0444` on the key file | Its owner or mode (compose keeps the host's) | `sudo chown 65532:65532 pcb_key && sudo chmod 600 pcb_key`. [Key file](container.md#the-key-file) |
| Thunderbird disables a calendar after an edit | A refused write (403) on a calendar without offline support | Enable the calendar again, check "Offline support", look for `refused a CalDAV write` in the log. [Writes](thunderbird.md#when-thunderbird-disables-a-calendar) |
| An attendee's participation status reverts | Proton keeps the attendee's stored line | Change it in the Proton web app. [What Proton changes](thunderbird.md#what-proton-changes) |
| Thunderbird shows a conflict dialog (412) | The event changed elsewhere meanwhile | Choose in the dialog. [503 and 412](thunderbird.md#503-and-412-answers) |
| The container restarts in a loop | A fault exit 10 (repair) or 20 (may pass); Docker restarts after both | Stop it, `docker compose run --rm --no-deps --entrypoint pcb pcb doctor`, start it. [Container](container.md#when-the-container-stops-or-restarts-in-a-loop) |
| `pcb serve` exits 20 in the container | `/data` not writable by 65532, or `config`/`state` not 0700 | [Prepare /data](container.md#prepare-data) |
| No tray icon on GNOME | The AppIndicator extension is missing or off | Install and enable AppIndicator and KStatusNotifierItem Support. [Desktop](desktop.md#no-tray-icon-on-gnome) |
| `pcb-ui` on Windows exits 10, `pcb-ui needs OpenGL 2.0` | No OpenGL 2.0: a VM without 3D acceleration, a remote desktop session | Mesa's software `opengl32.dll` beside `pcb-ui.exe`. [Below](#pcb-ui-needs-opengl-20) |
| `pcb-ui` shows a dialog and exits 20 | `pcb serve` (the service, the container) already serves this state | `pcb service stop` first, or use one of them. [One server per state](desktop.md#one-server-per-state-directory) |
| `pcb is already serving (pid N, kind K)`, exit 20 | Another `pcb serve`, `pcb-ui` (`ui`) or agent session (`mcp`) holds the state | Stop it, or let commands go through it. [Thunderbird](thunderbird.md#what-pcb-serve-does) |
| `pcb-ui` says `bridge stopped: <error>` | An error in `config.toml` after "Restart bridge" | Fix the file, press "Restart bridge". [Desktop](desktop.md#edit-the-configuration) |
| Exit 20 at start, the port in use | Another program listens on the CalDAV port | Stop it, or change `[caldav] listen`. [Listener](thunderbird.md#the-caldav-listener) |
| Thunderbird shows a certificate exception | pcb's self-signed certificate, or a replaced one | Confirm only when its SHA-256 fingerprint equals the one `pcb client show` or `pcb doctor` prints. [Certificate](thunderbird.md#tls-and-the-certificate) |
| `pcb mcp` exits 20 asking for `pcb service start` | The user service is installed but did not answer within 30 s | `pcb service start`. [Agents](agents.md#on-this-machine-stdio) |
| `pcb install` refuses, exit 10 | A path with a space, `%`, `$` or a non-ASCII letter | [Service](service.md#when-pcb-install-refuses-a-path) |
| A command run as root refuses, `sudo -u pcb pcb ...` | The state belongs to the `pcb` user | Run it as `pcb`. [System service](service.md#run-it-as-a-system-service) |
| pcb refuses to start, `account files of an older pcb are beside the per-account layout` | An older binary was run after the upgrade | Remove those files or move the state directory aside. [Upgrade](install.md#no-downgrade-after-the-multi-account-layout) |
| Windows: "Windows protected your PC" at the first start | SmartScreen: the binaries are not signed yet | More info, then Run anyway. [Windows](install.md#windows) |
| Windows: `pcb install` or `pcb service` exits 20, `no service on Windows` | There is no Windows service | Use `pcb-ui`: Settings, "Start at login" runs it at each login. [Desktop](desktop.md#on-windows) |
| Windows: exit 10, `DPAPI could not protect the sealing key for this Windows user` at the first login | Windows signed you in without your password (over SSH with a key, for example), so it cannot protect a new key | Sign in to Windows once with your password, then retry; or set `[secrets] backend = "keyfile"` and move the state directory aside before the next `pcb login`. [Security](security.md#on-windows-dpapi) |
| Windows: `key-unavailable`; exit 20, `DPAPI could not unprotect the sealing key for this Windows user` | Windows cannot decrypt the sealing key in this session: a sign-in without your password; also another Windows account's key, or a profile reset | Sign in to Windows once with your password, then retry. Do not log out: the stored session is intact. If it stays after a password sign-in, the key is lost for good: `pcb logout`, then `pcb login`. [Security](security.md#on-windows-dpapi) |
| Windows: exit 10, `the stored secrets cannot be opened with the key file …secrets.key.dpapi`; `pcb doctor` says the key `is damaged or not pcb's` | The DPAPI key file was damaged, replaced, or protected for the whole machine instead of your account | Restore it from a backup together with `secrets.sealed`, or `pcb logout` then `pcb login`. [Security](security.md#on-windows-dpapi) |
| Exit 10, `the state in <dir> was created on Windows (sealer dpapi)` (or `on Linux`) | A state directory copied between systems: a DPAPI key opens on Windows only, a desktop keyring on Linux only | Use it on that system, or move the directory aside and `pcb login`. [Security](security.md#a-state-moved-between-systems) |
| Windows: exit 10, `its access list … lets <account> in` | Another Windows account may read a file or directory of pcb | `pcb doctor` prints, for each, the `icacls` command that removes that account (PowerShell). [Security](security.md#on-windows-files-and-the-pipe) |
| Windows: exit 10, `belongs to … Administrators (an elevated pcb made it)` (the group's name is in your Windows language) | pcb was once run as administrator and created that directory | Run pcb elevated again, or take ownership with the `icacls … /setowner` command it prints; then run pcb from a normal terminal |
| Windows: `pcb doctor` warns `elevated` | pcb runs as administrator | Run it from a normal (not elevated) terminal: what an elevated pcb creates is owned by Administrators |
| Windows: exit 20, `the control pipe … is owned by another user` | Another Windows account's process holds the name of pcb's control pipe | Run pcb as the account that runs `pcb-ui`; if that is you, find the process that created the pipe, or set `PCB_STATE_DIR` to another directory. [Security](security.md#on-windows-files-and-the-pipe) |
| Windows: exit 20, `the control pipe is already served` when `pcb-ui` or `pcb serve` starts | Another `pcb-ui` or `pcb serve` of this state, or another account's process, holds the pipe's name | Stop the other one; if none runs, set `PCB_STATE_DIR` to another directory |
| Windows: `pcb doctor` warns `the Windows time zone … has no IANA name in pcb's table`, or `the Windows time zone cannot be read` | pcb has no IANA name for this Windows time zone, so agents get times with UTC offsets | Set `[mcp] timezone` in `config.toml` to your zone, such as `Europe/Paris` |

## pcb-ui needs OpenGL 2.0

**Symptom.** On Windows, `pcb-ui` does not open. Started from a terminal it prints `Error: pcb-ui needs OpenGL 2.0, which this display does not offer` with the driver's own message and exits 10; started from Explorer or at login it shows the same text in a message box.

**Cause.** `pcb-ui` draws with OpenGL 2.0. Without a graphics driver that provides it, Windows offers only its own OpenGL 1.1: a virtual machine without 3D acceleration, some remote desktop sessions, a machine with only the basic display driver.

**Fix.** Install or enable the graphics driver (in a VM, turn on 3D acceleration). Where that is not possible, Mesa's software renderer works around it: download a Mesa build for Windows (x64, for example the `mesa-dist-win` release archive) and copy its `opengl32.dll`, with the DLLs the archive puts beside it, into the folder of `pcb-ui.exe`. Windows loads a DLL beside the program before its own, so only `pcb-ui` uses it. pcb does not ship Mesa. `pcb` itself needs no OpenGL.
