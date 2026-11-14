# Desktop app

How to use `pcb-ui`, the Linux desktop app: the tray icon, the window, adding an account, starting at login, its logs, and removing it.

An account, with its adapters, the CalDAV connection to copy into Thunderbird, and its calendars:

<p align="center">
<img alt="pcb-ui showing an account: its adapters, its CalDAV connection and its calendars" src="assets/ui/account-light.png#gh-light-mode-only" width="720">
<img alt="pcb-ui showing an account: its adapters, its CalDAV connection and its calendars" src="assets/ui/account-dark.png#gh-dark-mode-only" width="720">
</p>

Adding an account: the address and password first, then a 2FA code or a CAPTCHA when Proton asks for one:

<p align="center">
<img alt="pcb-ui's add-account wizard: address, Proton password and the unattended-relogin box" src="assets/ui/add-account-light.png#gh-light-mode-only" width="720">
<img alt="pcb-ui's add-account wizard: address, Proton password and the unattended-relogin box" src="assets/ui/add-account-dark.png#gh-dark-mode-only" width="720">
</p>

The window follows your desktop's light or dark theme.

`pcb-ui` is the desktop face of the same bridge, in the spirit of Proton Mail Bridge. It is a separate binary that runs the daemon itself: no `pcb serve` is needed, and `pcb status`, `pcb client show` and Thunderbird work the same while it runs.

It is Linux only for now (X11 and Wayland with a StatusNotifierItem tray). `pcb` itself stays one static binary with no GUI dependency. Building it: [install](install.md#build-from-a-clone).

## Start it

```sh
pcb-ui                       # the window and the tray icon
pcb-ui --background          # tray only (the window opens anyway when no tray answers)
```

After `make install-ui` it is also in your applications menu.

## What it shows

| Part | What it does |
|---|---|
| Tray icon | Three states: ok, syncing, needs attention. Its menu has Quit. |
| Window | Your Proton accounts. Per account: the CalDAV and MCP switches, its calendars, and the connection to copy into Thunderbird (URL and username, each with Copy, and the password hidden behind Show and Copy). |
| Add-account wizard | Password, 2FA code, CAPTCHA. |
| Notifications | A desktop notification when an account needs you. |
| Settings | "Start at login", "Open config.toml", "Restart bridge", the log level. |

Closing the window hides it; Quit in the tray menu stops the bridge. Without a tray, closing the window quits (the same drain as Quit).

## Add an account

Open the add-account wizard from the window and type the address, the password, then the 2FA code, and solve the CAPTCHA when Proton asks for one. An AI agent that starts a login opens the same wizard with the address filled in ([agents](agents.md#log-in-from-an-agent)).

The window then shows the account's connection details. Copy them into Thunderbird as in [Thunderbird](thunderbird.md#connect-thunderbird).

## No tray icon on GNOME

GNOME shows no tray icon unless the AppIndicator extension (AppIndicator and KStatusNotifierItem Support) is installed and enabled.

Without a tray, `pcb-ui` opens its window and says so once, and closing that window quits `pcb-ui`, since nothing could bring it back.

## One server per state directory

`pcb-ui` and `pcb serve` (the systemd service, the container) cannot both serve one state.

| You start `pcb-ui` while | It does |
|---|---|
| another `pcb-ui` runs | shows the first one's window, then exits |
| `pcb serve` or the service runs | says so in a dialog and exits 20. Run `pcb service stop` first when pcb is installed as a service. |
| an agent session (`pcb mcp`) holds the state | refuses, saying an agent session holds the state: end it first ([agents](agents.md#on-this-machine-stdio)) |

`pcb doctor` warns when the desktop entry and the service are both installed.

## Start at login

Settings, "Start at login" writes `~/.config/autostart/pcb-ui.desktop` (`$XDG_CONFIG_HOME/autostart`) with the absolute path and `--background`. Switching it off deletes that file.

## Edit the configuration

Settings, "Open config.toml" opens the file; press "Restart bridge" afterwards. An error in the file stops the bridge:

- the window says `bridge stopped: <error>`;
- the tray shows attention;
- the account actions refuse until a Restart succeeds.

Fix the file and press "Restart bridge" again.

## The keyring

On a desktop session whose keyring (GNOME Keyring, KWallet) is unlocked, a new state keeps its sealing key in the keyring rather than in a key file. Check that the login keyring is unlocked (Seahorse, Passwords, Login) before the first start. A locked keyring makes the account wait with the alert `keyring-locked` until you unlock it. Details: [security](security.md#the-desktop-keyring-secret-service).

## Logs

`<state>/logs/pcb-ui.log`, rotated at 10 MiB to `pcb-ui.log.1` (one rotation).

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

It refuses (exit 20, naming it) while `pcb-ui`, `pcb serve` or an agent session serves that state. The binary and desktop entry under `$(PREFIX)` stay; remove them by hand.

Why it works this way: [a separate desktop binary that runs the daemon in process](../spec/decisions/0015-pcb-ui.md).
