# Desktop app

How to use `pcb-ui`, the desktop app for Linux and Windows: the tray icon, the window, adding an account, starting at login, its logs, and removing it.

An account, with its adapters, the CalDAV connection to copy into Thunderbird, and its calendars:

<p align="center">
<img alt="pcb-ui showing an account: its adapters, its CalDAV connection and its calendars" src="assets/ui/account-light.png#gh-light-mode-only" width="720">
<img alt="pcb-ui showing an account: its adapters, its CalDAV connection and its calendars" src="assets/ui/account-dark.png#gh-dark-mode-only" width="720">
</p>

Adding an account: the address and password first, then a 2FA code, the mailbox password of a two-password account, or a CAPTCHA when Proton asks for one:

<p align="center">
<img alt="pcb-ui's add-account wizard: address and Proton password" src="assets/ui/add-account-light.png#gh-light-mode-only" width="720">
<img alt="pcb-ui's add-account wizard: address and Proton password" src="assets/ui/add-account-dark.png#gh-dark-mode-only" width="720">
</p>

The window follows your desktop's light or dark theme.

`pcb-ui` is the desktop face of the same bridge, in the spirit of Proton Mail Bridge. It is a separate binary that runs the daemon itself: no `pcb serve` is needed, and `pcb status`, `pcb client show` and Thunderbird work the same while it runs.

It runs on Linux (X11 and Wayland with a StatusNotifierItem tray) and on Windows 10 and 11 ([On Windows](#on-windows)). `pcb` itself stays one static binary with no GUI dependency. Building it: [install](install.md#build-from-a-clone).

## Start it

```sh
pcb-ui                       # the window and the tray icon
pcb-ui --background          # tray only (waits up to 15 s for the tray, then opens the window if none answers)
```

After `make install-ui` it is also in your applications menu. On Windows the installer puts a `pcb-ui` shortcut in the Start menu.

## What it shows

| Part | What it does |
|---|---|
| Tray icon | Three states: ok, syncing, needs attention. Its menu has Quit. |
| Window | Your Proton accounts. Per account: the CalDAV switch, the MCP selector (Off, Read-only or Read-write: what AI agents may do with the account, with the Claude Code command to copy under it), its calendars (each with a dot of its Proton colour, and a `read-only` tag when pcb cannot write to it: hover the info icon next to the tag for the reason), and the connection to copy into Thunderbird (URL and username, each with Copy, and the password hidden behind Show and Copy). |
| Add-account wizard | Password, 2FA code, mailbox password, CAPTCHA. |
| Notifications | A desktop notification when an account needs you, and one when a newer release is out. |
| Update available | A panel at the top of the window when a newer stable release exists: "Open release page", "Copy update command" (the `install.sh` line, with the flags of this install), and "Later", which hides it for 48 hours. It comes back after, for the same version too: a fix only protects those who install it. The tray menu shows "Update available: vX.Y.Z" above "Open pcb" as long as one exists. The tray icon does not change. ([security](security.md#the-update-check)) |
| Settings | "Start at login", "Check for updates", "Open config.toml", "Restart bridge", the log level. |

Closing the window hides it; Quit in the tray menu stops the bridge. Without a tray, closing the window quits (the same drain as Quit).

## Add an account

Open the add-account wizard from the window and type the address, the password, then the 2FA code, and solve the CAPTCHA when Proton asks for one. An AI agent that starts a login opens the same wizard with the address filled in ([agents](agents.md#log-in-from-an-agent)).

An account in Proton's two-password mode gets one more screen after the 2FA code, "Mailbox password":
- Type the second password (the one that decrypts your data), then press Unlock or Enter.
- A wrong one shows an error with the tries left and stays on that screen, with no new Proton login; the third wrong one cancels the login.
- Every field of the wizard has a label, the first one has the focus and Enter submits.
- The account list shows `needs mailbox password` while the login waits.

The window then shows the account's connection details. Copy them into Thunderbird as in [Thunderbird](thunderbird.md#connect-thunderbird).

## Connect an AI agent

`pcb-ui` has no port for agents to set. The agent starts `pcb mcp`, the command-line binary, which hands the session to the running `pcb-ui`. So `pcb` must be installed next to `pcb-ui`: the release installer puts both (on Windows, the MSI with `pcb (command line)` ticked, or the zip), while from a clone `make install-ui` alone does not (run `make install install-ui`).

For Claude Code:

```sh
claude mcp add --scope user pcb -- pcb mcp                         # read, create, edit, delete (the default)
claude mcp add --scope user pcb -- pcb mcp --profile read-only     # read only
```

`--scope user` makes it available in every Claude Code session, not only in the directory you ran the command in ([scopes](agents.md#set-up-claude-code)). Give the absolute path of `pcb` (`~/.local/bin/pcb`) when the agent's `PATH` lacks it.

The MCP selector in the window is not a server switch: it says what any agent may do with that account. The agent's profile says what that agent may do. Both apply, and the stricter wins: on Read-write, an agent added with the first command edits and one added with `--profile read-only` only reads; on Read-only, every agent only reads ([the two settings](agents.md#two-settings-the-stricter-wins)). Under the selector, the window shows the first command with a Copy button, and what agents may do with the setting chosen.

An agent on another machine uses the remote `/mcp` with a token: turn it on in `config.toml` (Settings, "Open config.toml", then "Restart bridge") and create the token with `pcb mcp token add` ([remote](agents.md#from-another-machine-remote-with-a-token)).

## No tray icon on GNOME

GNOME shows no tray icon unless the AppIndicator extension (AppIndicator and KStatusNotifierItem Support) is installed and enabled.

With `--background` (the autostart), `pcb-ui` first waits up to 15 seconds for the tray to register, as the AppIndicator extension or the Plasma panel often starts after it. Without a tray, `pcb-ui` opens its window and says so once, and closing that window quits `pcb-ui`, since nothing could bring it back.

## One server per state directory

`pcb-ui` and `pcb serve` (the systemd service, the container) cannot both serve one state.

| You start `pcb-ui` while | It does |
|---|---|
| another `pcb-ui` runs | shows the first one's window, then exits |
| `pcb serve` or the service runs | says so in a dialog and exits 20. Run `pcb service stop` first when pcb is installed as a service. |
| an agent session (`pcb mcp`) holds the state | refuses, saying an agent session holds the state: end it first ([agents](agents.md#on-this-machine-stdio)) |

`pcb doctor` warns when the desktop entry and the service are both installed.

## Start at login

Settings, "Start at login" writes `~/.config/autostart/pcb-ui.desktop` (`$XDG_CONFIG_HOME/autostart`) with the absolute path and `--background`. Switching it off deletes that file. On Windows it writes a registry value instead, the one the installer's "Start at login" box writes too ([below](#on-windows)).

## On Windows

`pcb-ui.exe`, from the installer or the release zip ([install](install.md#windows)), runs the same bridge on Windows 10 (1809 or later) and 11.

| | On Windows |
|---|---|
| Tray | Always there, in the notification area (it may sit under the arrow of hidden icons). Its menu has Quit. |
| Start at login | Settings, "Start at login" writes the value `pcb-ui` under `HKEY_CURRENT_USER\Software\Microsoft\Windows\CurrentVersion\Run`, holding the absolute path of `pcb-ui.exe` and `--background`. Switching it off deletes the value. The installer's "Start at login" box writes the same value, so the switch shows it and turns it off; a repair of the installation does not turn it back on, and an upgrade keeps it as the switch left it. Moving the zip's folder breaks it: switch it off and on again from the new place. |
| Update available | The same panel and tray line, without "Copy update command": the release page has the installer and the zip. The notification of a new release only informs (Windows reports no click). |
| Notifications | Windows notifications. With the installer, its Start-menu shortcut names `pcb-ui` to Windows, so they show `pcb-ui`'s name and icon (checked live). Started from the zip, without that shortcut, they are best effort: some Windows builds may not show them. The tray icon's attention state carries the same news. Each one's title names the account and its text says what happened and the fix. A click on it is not reported: open the window from the tray. |
| Sign out, shutdown | `pcb-ui` gets up to 2 seconds to stop cleanly; what is cut short is recovered at the next start. |
| `pcb-ui.exe --version`, `--help` | Print to the terminal they are typed in, or to a redirected file. Started from Explorer, `pcb-ui` has no console window. |
| Open config.toml, logs | Open in the program Windows associates with them. `.toml` often has none: Windows then asks which app to open it with (Notepad will do). |
| Display | `pcb-ui` needs OpenGL 2.0. A virtual machine without 3D acceleration or a remote desktop session may lack it: `pcb-ui` then says so and exits ([troubleshooting](troubleshooting.md#pcb-ui-needs-opengl-20)). |

## Edit the configuration

Settings, "Open config.toml" opens the file, made empty first when there is none (every setting then keeps its default); press "Restart bridge" afterwards. An error in the file stops the bridge:

- the window says `bridge stopped: <error>`;
- the tray shows attention;
- the account actions refuse until a Restart succeeds.

Fix the file and press "Restart bridge" again.

## The keyring

On a desktop session whose keyring (GNOME Keyring, KWallet) is unlocked, a new state keeps its sealing key in the keyring rather than in a key file. Check that the login keyring is unlocked (Seahorse, Passwords, Login) before the first start. A locked keyring makes the account wait with the alert `keyring-locked` until you unlock it. Details: [security](security.md#the-desktop-keyring-secret-service).

## Logs

`<state>/logs/pcb-ui.log` (on Windows `%LOCALAPPDATA%\pcb\logs\pcb-ui.log`), rotated at 10 MiB to `pcb-ui.log.1` (one rotation).

The log level in Settings lasts until the bridge restarts; set `[log] level` in `config.toml` to keep it.

## Remove it

Quit `pcb-ui`, then:

```sh
pcb uninstall --delete-data --plan   # see what it would do
pcb uninstall --delete-data          # it asks; or --yes
```

With no service installed, it logs out and deletes:

- the config and state directories this shell resolves (only directories named `pcb`);
- the state's `pcb sealing key` keyring item;
- `~/.config/autostart/pcb-ui.desktop`.

It refuses (exit 20, naming it) while `pcb-ui`, `pcb serve` or an agent session serves that state. The binary and desktop entry under `$(PREFIX)` stay; remove them by hand. On Windows (the installer's Settings, Apps entry, or the zip's folder): [install](install.md#remove-it-from-windows).

Why it works this way: [a separate desktop binary that runs the daemon in process](../spec/decisions/0015-pcb-ui.md).
