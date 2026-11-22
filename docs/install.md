# Install

How to get `pcb` (and the desktop app `pcb-ui`) onto a machine, keep it up to date, and log in the first time. Once it is installed, pick how it runs: the [desktop app](desktop.md), a [systemd service](service.md) or a [container](container.md).

This is still an early version. On Linux its author uses it with their own Proton account, and every feature works there. What is built and checked so far is in [status](status.md).

## Requirements

- **Linux (amd64, arm64)** is supported. macOS (amd64, arm64) gets `pcb` only and is untested; Windows 10 (1809 or later) and 11 (amd64) are supported too, both binaries ([below](#macos-and-windows)).
- **`curl` or `wget`**, to install a release.
- **Go** (the version in `go.mod`) and `make`, only to [build from a clone](#build-from-a-clone).
- **systemd 252 or later** (Debian 12 or 13) to run pcb as a service ([service](service.md)).
- **Docker** with Compose to run the container image ([container](container.md)).
- For the desktop app only: a C compiler and the OpenGL and X11/Wayland headers ([build dependencies](#build-dependencies-of-the-desktop-app)), and a desktop with a StatusNotifierItem tray ([desktop](desktop.md)).

### macOS and Windows

**macOS** gets `pcb` only (amd64 and arm64), from the same install line: no `pcb-ui` (Linux and Windows only), no desktop entry, no service management. It is untested by the author: CI runs the installer, `pcb version` and `pcb doctor` on macOS for every pull request, and a release runs the installer and `pcb version` (which must print the tag) on the real macOS archive before it is published, but nobody has tried logging in or syncing. A binary you download with a browser is quarantined by Gatekeeper; clear it with `xattr -d com.apple.quarantine pcb` (the install line, which uses `curl`, needs no such step).

**Windows** 10 (1809 or later) and 11, amd64, run both `pcb-ui` and `pcb`, from an installer (`pcb_windows_amd64.msi`, from v0.2.0) or a zip, both on the releases page ([Windows](#windows)). This is supported: CI builds and tests both binaries on Windows, and the installer; a release runs them from the zip, and installs, checks and uninstalls its installer, before it is published; the bridge was checked live on a Windows 11 desktop as a standard user (CalDAV with `curl`: Thunderbird for Windows' own window was not checked), and so was the installer. There is no service: `pcb-ui`, started at login, runs the bridge.

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

What it checks: the archive's SHA-256 against the release's `checksums.txt`, and its attestation when `gh` 2.49 or later is installed and logged in (always with `--verify`). Downloads use https only (a redirect to http is refused). The checksums come from the same release as the archive, so alone they guard against a corrupted download, not a tampered release; the attestation check is what proves the archive came from this repository's workflow, and it needs `gh`: without it (or logged out) the install goes ahead with a note, unless you pass `--verify`. A failed check stops the install and leaves the previous files as they were. If `PREFIX/bin` is not on your `PATH`, or `pcb-ui` lacks a system library, it says so and names the fix.

It does not set up the service: next, run `pcb-ui`, or `pcb install` for a service, then `pcb login`.

### Prebuilt `pcb-ui`

The release's `pcb-ui` is built on Ubuntu 22.04 (glibc 2.35). It runs on Ubuntu 22.04 and later, Debian 12 and later, Fedora 36 and later, and Arch. It does not run on RHEL 9 or Debian 11: [build from a clone](#build-from-a-clone) there, or install with `--no-ui`.

### Check a download yourself

Every archive, the Windows installer, `checksums.txt` and `install.sh` carry a GitHub attestation: proof that this repository's release workflow (`release.yml`) built the file from a given commit. With the [GitHub CLI](https://cli.github.com/) 2.49 or later, logged in:

```sh
gh attestation verify pcb_linux_amd64.tar.gz --repo Barben360/proton-calendar-bridge \
  --signer-workflow Barben360/proton-calendar-bridge/.github/workflows/release.yml
```

The archives are `pcb_<os>_<arch>.tar.gz` with `os` `linux` or `darwin` and `arch` `amd64` or `arm64`, and `pcb_windows_amd64.zip` and (from v0.2.0) `pcb_windows_amd64.msi`, on the [releases page](https://github.com/Barben360/proton-calendar-bridge/releases).

### Remove

There is no uninstall command. Stop whatever runs pcb first (`pcb uninstall` for a service, then quit `pcb-ui`), then delete the files in the table above, from the prefix you used. On Windows: [Remove it](#remove-it-from-windows).

## Windows

Windows 10 (1809 or later) and 11, amd64 ([above](#macos-and-windows)), two ways:

| | Installer (`pcb_windows_amd64.msi`) | Zip (`pcb_windows_amd64.zip`) |
|---|---|---|
| Install | double-click, a few screens | unzip into a folder of your own |
| `pcb` in terminals | on your `PATH` (optional) | add the folder to `PATH` yourself |
| Start-menu shortcut | yes, which also names pcb-ui's notifications | no |
| Start at login | a ticked box in the installer, then pcb-ui's Settings | pcb-ui's Settings |
| Remove | Settings, Apps | `pcb uninstall`, then delete the folder |

Neither needs administrator rights. The installer was checked live on a Windows 11 desktop as a standard user: the double-click install and its screens, the shortcut and notifications under `pcb-ui`'s name, "Start at login" across a restart, a repair, upgrades and the uninstall. The zip stays as the portable option.

### The installer

1. Download `pcb_windows_amd64.msi` from the [releases page](https://github.com/Barben360/proton-calendar-bridge/releases) (from v0.2.0; for an older release use [the zip](#the-zip)). To check it, compare `Get-FileHash pcb_windows_amd64.msi` (PowerShell, SHA-256) with its line in the release's `checksums.txt`, or run `gh attestation verify` on it as [above](#check-a-download-yourself).
2. Open it. Until it is signed, SmartScreen may stop it with "Windows protected your PC" and an unknown publisher: choose More info, then Run anyway. No administrator prompt follows: it installs for your Windows account only.
3. The screens: the licence (MIT); the features, `pcb-ui` (always installed) and `pcb (command line)` (ticked: untick it for `pcb-ui` alone); "Start at login", ticked (`pcb-ui` starts in the tray when you sign in to Windows; its Settings change that later); then the install, and a last screen whose "Launch pcb-ui" box starts it.
4. In `pcb-ui`, Add account. In a terminal opened after the install, `pcb status`, `pcb client show`, `pcb doctor` talk to the running `pcb-ui`.

What it installs:

| What | Where |
|---|---|
| `pcb-ui.exe` | `%LOCALAPPDATA%\Programs\pcb`, always that folder, so the installer's `PATH` entry is the one you would add by hand; the screens offer no other folder, and `msiexec … INSTALLFOLDER=…` is refused (`pcb installs in %LOCALAPPDATA%\Programs\pcb only: leave INSTALLFOLDER unset.`) |
| `pcb.exe` (`pcb (command line)`) | the same folder, added at the end of your user `PATH` |
| the shortcut `pcb-ui` | the Start menu; it carries `pcb-ui`'s app ID, which Windows names its notifications after ([desktop](desktop.md#on-windows)) |
| "Start at login" (when ticked) | the registry value `pcb-ui` that `pcb-ui`'s own switch writes ([desktop](desktop.md#start-at-login)); if Windows refuses that write, the install still completes: turn it on in `pcb-ui`'s Settings |

Your accounts and settings stay where `pcb` keeps them ([below](#where-pcb-keeps-its-files)), not in the program folder. Each Windows account that wants the bridge installs it for itself.

From a terminal (PowerShell or `cmd`, in the download's folder), with no screens: `STARTATLOGIN=0` or `1` sets "Start at login" (only `1` turns it on), `REMOVE=Cli` and `ADDLOCAL=App,Cli` remove or add `pcb (command line)`:

```powershell
msiexec /i pcb_windows_amd64.msi /qn                    # both features, Start at login on
msiexec /i pcb_windows_amd64.msi /qn STARTATLOGIN=0     # Start at login off
msiexec /i pcb_windows_amd64.msi /qn REMOVE=Cli         # installed already: remove pcb.exe and its PATH entry
msiexec /i pcb_windows_amd64.msi /qn ADDLOCAL=App,Cli   # and put them back
```

Run `REMOVE=Cli` with the installer of the installed version: another version's installs nothing then, and the installed one stays as it was. `ADDLOCAL` names `App` too: with another version's installer the line is an upgrade, which installs only the features it names, so `ADDLOCAL=Cli` alone would remove `pcb-ui`.

**Upgrade.** Open the newer MSI the same way: it replaces the installed version. "Start at login" stays as `pcb-ui`'s switch left it, unless `STARTATLOGIN` is given on the command line. A running `pcb-ui` is closed first, cleanly, as at a Windows sign-out (with the screens, a `Files in Use` dialog names `pcb desktop app` first and offers to close it and "attempt to restart" it: the restart does not happen); nothing starts it again after a silent upgrade (`/qn`): it comes back at your next sign-in when "Start at login" is on, or start it from the Start menu. With the screens, the last screen's "Launch pcb-ui" starts it. The same version can be installed again (a release candidate and its final release share one installer version); an older one is refused: `A newer version of pcb is already installed. Uninstall it first to install this one.`

**Repair.** Open the installed version's MSI again and choose Repair (or `msiexec /fa pcb_windows_amd64.msi /qn`): it copies `pcb-ui.exe` and `pcb.exe` again and leaves "Start at login" as `pcb-ui`'s switch left it. Replacing both programs, it closes a running `pcb-ui` as an upgrade does, and nothing starts it again: the `Files in Use` dialog's "attempt to restart" does not, and a repair's last screen has no "Launch pcb-ui". Start it from the Start menu afterwards.

**From the zip to the installer.** Quit `pcb-ui`, run `.\pcb.exe uninstall` in the zip's folder (it turns "Start at login" off when that starts this folder's `pcb-ui.exe`, and keeps your accounts), then delete the folder and its `PATH` entry, and install the MSI: your accounts and settings carry over. Left in place, the zip's `PATH` entry comes before the installer's, so terminals would still find the zip's `pcb.exe`. A zip unzipped into `%LOCALAPPDATA%\Programs\pcb` itself is replaced: the installer puts its own `pcb-ui.exe` and `pcb.exe` there, even over a zip of the same version, and the uninstall also removes the zip's `LICENSE` and `README.md`. The installer itself only touches a "Start at login" value for another `pcb-ui.exe` when its box is ticked (it then starts the installed one); a `PATH` entry you made by hand for the expanded folder (for example `C:\Users\<you>\AppData\Local\Programs\pcb`, no trailing `\` and no `%VARIABLE%`) is not added twice, and goes at the uninstall.

### The zip

1. Download `pcb_windows_amd64.zip` from the [releases page](https://github.com/Barben360/proton-calendar-bridge/releases). It holds `pcb-ui.exe`, `pcb.exe`, `LICENSE` and `README.md`. To check it yourself, compare `Get-FileHash pcb_windows_amd64.zip` (PowerShell, SHA-256) with its line in the release's `checksums.txt`, or run `gh attestation verify` on it as [above](#check-a-download-yourself).
2. Unzip it into a folder of your own, for example `%USERPROFILE%\pcb`. No installer and no administrator rights are needed.
3. Start `pcb-ui.exe`, then Add account in the window. Settings, "Start at login" starts it at each login ([desktop](desktop.md#start-at-login)).
4. Optional, for the terminal: add that folder to your `PATH` (search the Start menu for "Edit environment variables for your account", edit `Path`, add the folder), then open a new terminal: `pcb status`, `pcb client show`, `pcb doctor` talk to the running `pcb-ui`.

Until the binaries are signed, Windows SmartScreen may stop the first start with "Windows protected your PC" and an unknown publisher: choose More info, then Run anyway.

### What is different from Linux

| | On Windows |
|---|---|
| Files | config in `%APPDATA%\pcb`, state (sessions, cache, the sealing key, logs) in `%LOCALAPPDATA%\pcb`, only your Windows account (and the system) can open them |
| Sealing key | protected by Windows for your account (DPAPI), in the state directory ([security](security.md#on-windows-dpapi)) |
| Running at login | `pcb-ui`'s "Start at login"; `pcb install` and `pcb service` refuse (exit 20, `no service on Windows`) |
| Display | `pcb-ui` needs OpenGL 2.0: some virtual machines and remote desktops lack it ([troubleshooting](troubleshooting.md#pcb-ui-needs-opengl-20)) |
| Commands | the same as on Linux; hints and fixes are written for PowerShell |

### Remove it from Windows

**Installed with the MSI.** Settings, Apps, Installed apps (Windows 10: Apps & features), `pcb`, Uninstall; or `msiexec /x pcb_windows_amd64.msi /qn`. It closes a running `pcb-ui` (with the screens, a `Files in Use` dialog asks first; its "attempt to restart" wording does not apply here), then removes the program files, the Start-menu shortcut, the `PATH` entry, and "Start at login" when it starts the installed `pcb-ui.exe`. It keeps `%APPDATA%\pcb` and `%LOCALAPPDATA%\pcb`, your accounts, sessions and settings: installing again finds them. Files you added to the program folder yourself (Mesa's DLLs, for example) stay; delete the folder afterwards.

To delete your data too, do it before the uninstall, while `pcb.exe` is still there (if you removed `pcb (command line)`, put it back first with `ADDLOCAL=App,Cli` as above): quit `pcb-ui` (its tray menu, Quit), then in a new terminal:

```powershell
pcb uninstall --delete-data --plan   # see what it would do
pcb uninstall --delete-data          # it asks; or --yes
```

**From the zip.** Quit `pcb-ui`, then in a terminal in its folder:

```powershell
.\pcb.exe uninstall --delete-data --plan   # see what it would do
.\pcb.exe uninstall --delete-data          # it asks; or --yes
```

Then delete the folder you unzipped.

Either way, `pcb uninstall --delete-data` logs out, deletes `%APPDATA%\pcb` and `%LOCALAPPDATA%\pcb` (the sealing key included) and turns "Start at login" off when it starts the `pcb-ui.exe` beside that `pcb.exe`. Without `--delete-data` it only turns "Start at login" off. It refuses (exit 20) while `pcb-ui`, `pcb serve` or an agent session (`pcb mcp`) still serves the state.

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

Run the install line again: it replaces the files with the latest release (or the one you name with `--version`), and says to restart a running pcb. On Windows, open the newer MSI ([upgrade](#the-installer)) or unzip the newer zip over the old files, with `pcb-ui` quit. From a clone, pull, rebuild and reinstall (`make install`, and `make install-ui` for the desktop app). Then restart what runs pcb: `pcb service restart`, Quit and start `pcb-ui` again, or recreate the container from the new image.

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
