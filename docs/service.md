# Run as a service

How to run pcb as a systemd service, for your user or for a whole server, change its settings, and remove it. On a desktop you may prefer the [desktop app](desktop.md); in Docker, see [container](container.md).

It needs systemd 252 or later (Debian 12 or 13) and `pcb` installed ([install](install.md)).

## Install the user service

```sh
make install                 # bin/pcb -> ~/.local/bin/pcb (PREFIX=/usr/local to change it)
pcb install --plan           # what it would do; changes nothing
pcb install                  # user unit (no root), directories 0700, a default config.toml; enables and starts it
loginctl enable-linger "$USER"   # so it runs without a login session; pcb install prints this line unless already on
pcb login                    # goes through the running service; creates the CalDAV client "default"
pcb client show              # the URL, username, password and fingerprint for Thunderbird
pcb service status           # also: start, stop, restart, logs -f
pcb doctor                   # every check, with the fix for each failure
```

The unit runs `pcb serve --wait-for-login`:

- it waits for your first login;
- it answers 503 while an account needs a login again;
- after a reboot or a restart it serves again by itself.

`pcb doctor` checks the service, the daemon, the listener, the certificate and the clients. It exits 0 while the service waits for your login (`needs-login` shows as `!`).

Without lingering, your user's units stop when you log out: `loginctl enable-linger "$USER"` keeps the service running.

Next: [connect Thunderbird](thunderbird.md#connect-thunderbird).

## Manage it

```sh
pcb service start|stop|restart|status|logs [-f]
```

## Change its settings

`pcb install` is idempotent: a second run keeps every file and never replaces an edited `config.toml`.

To change the listener, edit `[caldav]` (`listen`, `hostnames`) in `~/.config/pcb/config.toml`, then:

```sh
pcb service restart
```

The `[caldav]` keys are described in [Thunderbird](thunderbird.md#the-caldav-listener).

## Run it as a system service

For a server:

```sh
sudo pcb install --system
```

It:

- creates the `pcb` user;
- uses `/etc/pcb` (config) and `/var/lib/pcb` (state);
- copies the binary to `/usr/local/bin/pcb`;
- installs the fully hardened system unit.

The control socket answers only the daemon's own user, so run the CLI as that user, as `pcb install` prints it:

```sh
sudo -u pcb env PCB_CONFIG_DIR=/etc/pcb PCB_STATE_DIR=/var/lib/pcb /usr/local/bin/pcb login
sudo -u pcb env PCB_CONFIG_DIR=/etc/pcb PCB_STATE_DIR=/var/lib/pcb /usr/local/bin/pcb client show
```

| Command | Needs root with `--system`? |
|---|---|
| `pcb service start`, `stop`, `restart` | yes |
| `pcb service status`, `logs` | no |

A command that opens the state, run as root against a state directory owned by `pcb`, refuses (exit 10, `sudo -u pcb pcb ...`) rather than leave root-owned files the service cannot read.

With a system service installed, `pcb mcp` does not serve on its own: use the remote `/mcp` instead ([agents](agents.md#from-another-machine-remote-with-a-token)).

## When `pcb install` refuses a path

`pcb install` refuses a binary or home path that contains a space, `%`, `$`, a quote or a backslash (accented and other non-ASCII letters are fine) (exit 10, also under `--plan`): systemd would split or expand it. Move the binary, or install with a `PREFIX` without those characters.

## When a step fails

`pcb install` and `pcb uninstall` run in steps. When one fails, the command exits 40 and names it, hint `fix it, then run the command again: finished steps are kept`.

## When systemd is not running

Where systemd is not running (a container, typically), `pcb install` (and a `pcb uninstall` whose plan calls systemctl) exits 20 with `systemd is not running here`, and `--plan` ends with the note that applying the plan fails. In a container, run pcb serve as the entrypoint instead: [container](container.md).

## Uninstall

```sh
pcb uninstall --plan         # the steps, without running them
pcb uninstall                # stops the service and removes the unit; your data stays
```

To remove the data too:

```sh
pcb uninstall --delete-data  # it asks; or --yes
```

`--delete-data`:

1. logs out first, which revokes the Proton session;
2. deletes the config and state directories (with `--system`, also the `pcb` user);
3. with the desktop keyring as sealer, removes the state's `pcb sealing key` item ([security](security.md#the-desktop-keyring-secret-service)).

It removes only a directory named `pcb`, and keeps and names a key file kept elsewhere. It leaves `/usr/local/bin/pcb` and prints the `rm` line to delete it.

With no service installed, it deletes the directories this shell resolves and `pcb-ui`'s autostart entry ([desktop](desktop.md#remove-it)).

How it was tested: install and uninstall were run live on a fresh host on 2026-11-13 ([status](status.md)).
