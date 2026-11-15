# Install

How to get `pcb` (and the desktop app `pcb-ui`) onto a machine, keep it up to date, and log in the first time. Once it is installed, pick how it runs: the [desktop app](desktop.md), a [systemd service](service.md) or a [container](container.md).

This is still an early version: do not point pcb at a Proton account you care about yet. Use a test account. What is built and checked so far is in [status](status.md).

## Requirements

- **Linux (amd64, arm64)** is supported. macOS (amd64, arm64) gets `pcb` only and is untested; Windows 10 (1809 or later) and 11 (amd64) are supported too, both binaries ([below](#macos-and-windows)).
- **`curl` or `wget`**, to install a release.
- **Go** (the version in `go.mod`) and `make`, only to [build from a clone](#build-from-a-clone).
- **systemd 252 or later** (Debian 12 or 13) to run pcb as a service ([service](service.md)).
- **Docker** with Compose to run the container image ([container](container.md)).
- For the desktop app only: a C compiler and the OpenGL and X11/Wayland headers ([build dependencies](#build-dependencies-of-the-desktop-app)), and a desktop with a StatusNotifierItem tray ([desktop](desktop.md)).

### macOS and Windows

**macOS** gets `pcb` only (amd64 and arm64), from the same install line: no `pcb-ui` (Linux and Windows only), no desktop entry, no service management. It is untested by the author: CI runs the installer, `pcb version` and `pcb doctor` on macOS for every pull request, and a release runs the installer and `pcb version` (which must print the tag) on the real macOS archive before it is published, but nobody has tried logging in or syncing. A binary you download with a browser is quarantined by Gatekeeper; clear it with `xattr -d com.apple.quarantine pcb` (the install line, which uses `curl`, needs no such step).

**Windows** 10 (1809 or later) and 11, amd64, run both `pcb-ui` and `pcb`, from a zip on the releases page ([Windows](#windows)). This is supported: CI builds and tests both binaries on Windows, a release runs them from the zip before it is published, and the bridge was checked live on a Windows 11 desktop as a standard user (CalDAV with `curl`: Thunderbird for Windows' own window was not checked). There is no service: `pcb-ui`, started at login, runs the bridge.

## Install a release

```sh
curl -fsSL https://github.com/Barben360/proton-calendar-bridge/releases/latest/download/install.sh | sh
```

This installs the latest stable release for your system, with no sudo, under `~/.local`. Add flags after `sh -s --`:

```sh
curl -fsSL https://github.com/Barben360/proton-calendar-bridge/releases/latest/download/install.sh | sh -s -- --no-desktop
```

| Flag | Effect | Default |
|---|---|---|
| `--prefix DIR` (or `--prefix=DIR`) | install under `DIR/bin` and `DIR/share` | `~/.local` |
| `--version vX.Y.Z` | that release instead of the latest stable (the `v` is optional) | latest stable |
| `--no-desktop` | no desktop entry and no icon | both installed |
| `--no-ui` | `pcb` only, no `pcb-ui` and no desktop entry (servers) | both binaries |
| `--verify` | require the attestation check: fail when `gh` is missing, older than 2.49 or not logged in | check when `gh` (2.49 or later) is logged in, else skip with a line |
| `-h`, `--help` | usage | |

A prefix containing `%`, `"`, `$`, `\` or a backtick is refused. Exit codes: `0` done, `1` failed, `2` bad invocation.

It installs:

| File | Where |
|---|---|
| `pcb` | `PREFIX/bin/pcb` |
| `pcb-ui` | `PREFIX/bin/pcb-ui` |
| the desktop entry | `PREFIX/share/applications/pcb-ui.desktop`, with `Exec=` set to the absolute path |
| the icon | `PREFIX/share/icons/hicolor/scalable/apps/pcb-ui.svg` |

What it checks: the archive's SHA-256 against the release's `checksums.txt`, and its attestation when `gh` 2.49 or later is installed and logged in (always with `--verify`). A failed check stops the install and leaves the previous files as they were. If `PREFIX/bin` is not on your `PATH`, or `pcb-ui` lacks a system library, it says so and names the fix.

It does not set up the service: next, run `pcb-ui`, or `pcb install` for a service, then `pcb login`.

### Prebuilt `pcb-ui`

The release's `pcb-ui` is built on Ubuntu 22.04 (glibc 2.35). It runs on Ubuntu 22.04 and later, Debian 12 and later, Fedora 36 and later, and Arch. It does not run on RHEL 9 or Debian 11: [build from a clone](#build-from-a-clone) there, or install with `--no-ui`.

### Check a download yourself

Every archive, `checksums.txt` and `install.sh` carry a GitHub attestation: proof that this repository's release workflow (`release.yml`) built the file from a given commit. With the [GitHub CLI](https://cli.github.com/) 2.49 or later, logged in:

```sh
gh attestation verify pcb_linux_amd64.tar.gz --repo Barben360/proton-calendar-bridge \
  --signer-workflow Barben360/proton-calendar-bridge/.github/workflows/release.yml
```

The archives are `pcb_<os>_<arch>.tar.gz` with `os` `linux` or `darwin` and `arch` `amd64` or `arm64`, and `pcb_windows_amd64.zip`, on the [releases page](https://github.com/Barben360/proton-calendar-bridge/releases).

### Remove

There is no uninstall command. Stop whatever runs pcb first (`pcb uninstall` for a service, then quit `pcb-ui`), then delete the files in the table above, from the prefix you used. On Windows: [Remove it](#remove-it-from-windows).

## Windows

Windows 10 (1809 or later) and 11, amd64 ([above](#macos-and-windows)).

1. Download `pcb_windows_amd64.zip` from the [releases page](https://github.com/Barben360/proton-calendar-bridge/releases). It holds `pcb-ui.exe`, `pcb.exe`, `LICENSE` and `README.md`. To check it yourself, compare `Get-FileHash pcb_windows_amd64.zip` (PowerShell, SHA-256) with its line in the release's `checksums.txt`, or run `gh attestation verify` on it as [above](#check-a-download-yourself).
2. Unzip it into a folder of your own, for example `%LOCALAPPDATA%\Programs\pcb`. No installer and no administrator rights are needed.
3. Start `pcb-ui.exe`, then Add account in the window. Settings, "Start at login" starts it at each login ([desktop](desktop.md#start-at-login)).
4. Optional, for the terminal: add that folder to your `PATH` (search the Start menu for "Edit environment variables for your account", edit `Path`, add the folder), then open a new terminal: `pcb status`, `pcb client show`, `pcb doctor` talk to the running `pcb-ui`.

Until the binaries are signed, Windows SmartScreen may stop the first start with "Windows protected your PC" and an unknown publisher: choose More info, then Run anyway.

What is different from Linux:

| | On Windows |
|---|---|
| Files | config in `%APPDATA%\pcb`, state (sessions, cache, the sealing key, logs) in `%LOCALAPPDATA%\pcb`, only your Windows account (and the system) can open them |
| Sealing key | protected by Windows for your account (DPAPI), in the state directory ([security](security.md#on-windows-dpapi)) |
| Running at login | `pcb-ui`'s "Start at login"; `pcb install` and `pcb service` refuse (exit 20, `no service on Windows`) |
| Display | `pcb-ui` needs OpenGL 2.0: some virtual machines and remote desktops lack it ([troubleshooting](troubleshooting.md#pcb-ui-needs-opengl-20)) |
| Commands | the same as on Linux; hints and fixes are written for PowerShell |

### Remove it from Windows

Quit `pcb-ui` (its tray menu, Quit), then in a terminal in its folder:

```powershell
.\pcb.exe uninstall --delete-data --plan   # see what it would do
.\pcb.exe uninstall --delete-data          # it asks; or --yes
```

It logs out, deletes `%APPDATA%\pcb` and `%LOCALAPPDATA%\pcb` (the sealing key included) and turns "Start at login" off when it starts this folder's `pcb-ui.exe`. Then delete the folder you unzipped. Without `--delete-data` it only turns "Start at login" off. It refuses (exit 20) while `pcb-ui`, `pcb serve` or an agent session (`pcb mcp`) still serves the state.

## Build from a clone

For a platform with no release, or to run your own changes.

```sh
make install                 # bin/pcb -> ~/.local/bin/pcb (PREFIX=/usr/local to change it)
bin/pcb version
```

`make install` runs `make build` (`bin/pcb`, a static binary with no GUI dependency), then copies it to `$(PREFIX)/bin/pcb`; `PREFIX` defaults to `~/.local`.

The desktop app is a separate binary:

```sh
make ui                      # bin/pcb-ui (cgo: needs the packages below)
make install-ui              # pcb-ui, its desktop entry and its icon under $(PREFIX)
```

`make install-ui` installs:

| File | Where |
|---|---|
| the binary | `$(PREFIX)/bin/pcb-ui` |
| the desktop entry | `$(PREFIX)/share/applications/pcb-ui.desktop`, with `Exec=` set to the absolute path |
| the icon | `$(PREFIX)/share/icons/hicolor/scalable/apps/pcb-ui.svg` |

### Build dependencies of the desktop app

Fyne, the toolkit of `pcb-ui`, draws with OpenGL on X11 and Wayland, so `make ui` needs the C headers and a C compiler. `make build` and the container image do not.

| Distribution | Command |
|---|---|
| Debian, Ubuntu | `sudo apt-get install libgl1-mesa-dev libwayland-dev libxkbcommon-dev xorg-dev` (what CI installs) |
| Arch | `sudo pacman -S libglvnd libxcursor libxrandr libxinerama libxi wayland libxkbcommon` (from Fyne's documented list, not run here) |

Developers need them too: `make lint test` compiles `cmd/pcb-ui` ([development](development.md)).

## The container image

The image is a public package, so pulling it needs no login:

```sh
docker pull ghcr.io/barben360/proton-calendar-bridge:latest
```

`latest` is the newest stable release; the other tags are in [container](container.md#tags). Or build it from a clone:

```sh
make image                   # the image, tagged pcb:local
```

How to run it: [container](container.md).

## Where pcb keeps its files

| | Default | Override |
|---|---|---|
| Config directory (`config.toml`, the key file) | `~/.config/pcb` | `PCB_CONFIG_DIR`, or `$XDG_CONFIG_HOME/pcb` |
| State directory (sessions, cache, CalDAV passwords, certificate, logs) | `~/.local/state/pcb` | `PCB_STATE_DIR`, or `$XDG_STATE_HOME/pcb` |

A system service uses `/etc/pcb` and `/var/lib/pcb` ([service](service.md#run-it-as-a-system-service)); the container keeps both under `/data` ([container](container.md)). On Windows the config directory is `%APPDATA%\pcb` and the state directory `%LOCALAPPDATA%\pcb` (`PCB_CONFIG_DIR` and `PCB_STATE_DIR` still override them; `XDG_*` and `HOME` are not read there).

## First login

Whichever way pcb runs, the first login is the same command:

```sh
pcb login                    # asks for the username, password and 2FA code
pcb client show              # the URL, username and password to give Thunderbird
```

- The username decides: a known address is a relogin, a new one adds an account.
- A login with no CalDAV client yet creates the client `default`, whose password `pcb client show` prints again whenever you need it.
- When pcb already runs (`pcb serve`, the service, `pcb-ui`), `pcb login` goes through it.

Where the password and the 2FA code can come from, and what to do when Proton asks for a CAPTCHA: [security](security.md#log-in). Connecting Thunderbird: [Thunderbird](thunderbird.md).

## Upgrade

Run the install line again: it replaces the files with the latest release (or the one you name with `--version`), and says to restart a running pcb. From a clone, pull, rebuild and reinstall (`make install`, and `make install-ui` for the desktop app). Then restart what runs pcb: `pcb service restart`, Quit and start `pcb-ui` again, or recreate the container from the new image.

### Agents added without a profile now edit

`pcb mcp` with no `--profile` used to give the agent reads only. It now gives everything: reading, creating, editing and deleting events, and signing an account out. To keep an agent you added that way to reads, add it again with `--profile read-only` (`claude mcp remove pcb`, then `claude mcp add --scope user pcb -- pcb mcp --profile read-only`), or set its accounts to Read-only (`pcb account mcp read-only`, or the selector in `pcb-ui`). See [agents](agents.md#two-settings-the-stricter-wins).

### No downgrade after the multi-account layout

The first command of a version with several accounts per process moves the state of an older, single-account version into `accounts/<id>/`: the session, the cache and the CalDAV passwords come across. After that:

- the older binary sees no account;
- if it logs in anyway, the newer pcb refuses to start (exit 10) and names the files: `account files of an older pcb are beside the per-account layout`, hint `remove them or move the state directory aside: this pcb version cannot be downgraded`.

Likewise a pcb from before CalDAV passwords could be shown again refuses the version 2 `clients.json` that later versions write. Keep a copy of the state directory before upgrading if you may go back.

An account that was signed out when the upgrade moved it has no address. The first `pcb login` of an address pcb does not know takes it over, with its CalDAV clients. If that login is a different Proton account, the old Thunderbird password would read the new account's calendars: run `pcb client rotate NAME` for each client (`pcb client list`).

Thunderbird keeps working across the upgrade: the single-account URL (`/`, `/dav/calendars/me/...`) still answers ([Thunderbird](thunderbird.md#several-accounts)).

Why it works this way: [several accounts per process](../spec/decisions/0014-several-accounts-per-process.md).

## Next

| To | Read |
|---|---|
| use pcb on a Linux desktop, with a tray icon | [desktop](desktop.md) |
| run it as a systemd service | [service](service.md) |
| run it in Docker | [container](container.md) |
| connect Thunderbird | [Thunderbird](thunderbird.md) |
| connect an AI agent | [agents](agents.md) |
