# Install

How to get `pcb` (and the desktop app `pcb-ui`) onto a machine, keep it up to date, and log in the first time. Once it is installed, pick how it runs: the [desktop app](desktop.md), a [systemd service](service.md) or a [container](container.md).

This is still an early version: do not point pcb at a Proton account you care about yet. Use a test account. What is built and checked so far is in [status](status.md).

## Requirements

- **Linux, amd64 or arm64.** These are the supported platforms.
- **Go** (the version in `go.mod`) and `make`, to build from a clone. There are no prebuilt release binaries.
- **systemd 252 or later** (Debian 12 or 13) to run pcb as a service ([service](service.md)).
- **Docker** with Compose to run the container image ([container](container.md)).
- For the desktop app only: a C compiler and the OpenGL and X11/Wayland headers ([build dependencies](#build-dependencies-of-the-desktop-app)), and a desktop with a StatusNotifierItem tray ([desktop](desktop.md)).

### macOS and Windows

macOS and Windows only build: `GOOS=darwin` and `GOOS=windows` compile `./cmd/pcb`, but neither is tested and neither has service management.

Off Unix there is no file lock and no control socket, so on Windows `pcb login`, `pcb sync` and `pcb serve` exit 20 (`run pcb on Linux or macOS`). The rest of the CLI runs offline on the files.

## Build from a clone

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

The image is published at `ghcr.io/barben360/proton-calendar-bridge`, with the tags `:main` and `:sha-<12 hex>`. CI pushes it only from main.

It is a **private** package: pulling it needs `docker login ghcr.io` with a token that can read packages. Everyone else builds it from a clone:

```sh
make image                   # the image, tagged pcb:local
```

How to run it: [container](container.md).

## Where pcb keeps its files

| | Default | Override |
|---|---|---|
| Config directory (`config.toml`, the key file) | `~/.config/pcb` | `PCB_CONFIG_DIR`, or `$XDG_CONFIG_HOME/pcb` |
| State directory (sessions, cache, CalDAV passwords, certificate, logs) | `~/.local/state/pcb` | `PCB_STATE_DIR`, or `$XDG_STATE_HOME/pcb` |

A system service uses `/etc/pcb` and `/var/lib/pcb` ([service](service.md#run-it-as-a-system-service)); the container keeps both under `/data` ([container](container.md)).

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

Pull, rebuild and reinstall (`make install`, and `make install-ui` for the desktop app), then restart what runs pcb: `pcb service restart`, Quit and start `pcb-ui` again, or recreate the container from the new image.

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
