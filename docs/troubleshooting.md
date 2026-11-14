# Troubleshooting

What to do when pcb stops, refuses or misbehaves. Each entry gives the symptom, its cause and the fix, and links to the page with the details.

## Start with doctor

```sh
pcb doctor
```

`pcb doctor` runs every check (service, daemon, listener, certificate, clients, sealer, key file) and prints the fix for each failure. It exits 0 unless a check fails, 10 when you must act, 20 when only the daemon is unreachable. `pcb status` shows the live state and the alerts, each with its fix. In a stopped container, run it through `docker compose run` ([container](container.md#when-the-container-stops-or-restarts-in-a-loop)).

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
| `pcb-ui` shows a dialog and exits 20 | `pcb serve` (the service, the container) already serves this state | `pcb service stop` first, or use one of them. [One server per state](desktop.md#one-server-per-state-directory) |
| `pcb is already serving (pid N, kind K)`, exit 20 | Another `pcb serve`, `pcb-ui` (`ui`) or agent session (`mcp`) holds the state | Stop it, or let commands go through it. [Thunderbird](thunderbird.md#what-pcb-serve-does) |
| `pcb-ui` says `bridge stopped: <error>` | An error in `config.toml` after "Restart bridge" | Fix the file, press "Restart bridge". [Desktop](desktop.md#edit-the-configuration) |
| Exit 20 at start, the port in use | Another program listens on the CalDAV port | Stop it, or change `[caldav] listen`. [Listener](thunderbird.md#the-caldav-listener) |
| Thunderbird shows a certificate exception | pcb's self-signed certificate, or a replaced one | Confirm only when its SHA-256 fingerprint equals the one `pcb client show` or `pcb doctor` prints. [Certificate](thunderbird.md#tls-and-the-certificate) |
| `pcb mcp` exits 20 asking for `pcb service start` | The user service is installed but did not answer within 30 s | `pcb service start`. [Agents](agents.md#on-this-machine-stdio) |
| `pcb install` refuses, exit 10 | A path with a space, `%`, `$` or a non-ASCII letter | [Service](service.md#when-pcb-install-refuses-a-path) |
| A command run as root refuses, `sudo -u pcb pcb ...` | The state belongs to the `pcb` user | Run it as `pcb`. [System service](service.md#run-it-as-a-system-service) |
| pcb refuses to start, `account files of an older pcb are beside the per-account layout` | An older binary was run after the upgrade | Remove those files or move the state directory aside. [Upgrade](install.md#no-downgrade-after-the-multi-account-layout) |
| `pcb login`, `sync` or `serve` exit 20 on Windows | No file lock or control socket off Unix | Run pcb on Linux. [Platforms](install.md#macos-and-windows) |
