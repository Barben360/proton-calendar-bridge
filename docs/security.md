# Security and secrets

How pcb handles your Proton password, 2FA code and session, where it keeps the sealing key (a key file or the desktop keyring), how CalDAV passwords are stored, and how the certificate is checked.

## Log in

### Where secrets come from

Secrets never come from a flag. They come from:

| Source | How |
|---|---|
| a terminal prompt | `pcb login` asks for what is missing |
| a file descriptor | `pcb login --password-fd 3 3<pwfile` |
| the environment | `PCB_USERNAME`, `PCB_PASSWORD`, `PCB_TOTP` (a code or an `otpauth://` URI); a template is `scripts/pcb.env.example` |

### Login through the daemon

When `pcb serve` runs (or the service, or `pcb-ui`), `pcb login` goes through it, over a control socket only its own user can open (a named pipe on Windows, [below](#on-windows-files-and-the-pipe)).

- A login that waits for its 2FA code stays in the daemon for 5 minutes. `PCB_TOTP=<code> pcb login resume --totp` finishes it; the code is never an argument.
- With no daemon, `pcb login` says so and logs in on its own.

### Unattended relogin

```sh
pcb login --unattended-relogin
```

It seals the password and, with 2FA, `PCB_TOTP` as an `otpauth://` URI (a one-time code is refused). A session Proton ends is then repaired by one automatic login, at most one per 60 s.

Without it, a login stores tokens only, and you log in again when Proton ends the session. `pcb login resume` takes the same flags.

Only `pcb login` offers it. `pcb-ui` and the login page an AI agent opens never store your password: a login there turns unattended relogin off for that account, and `pcb login --unattended-relogin` turns it back on.

### Scripts: `--non-interactive`

`--non-interactive` never prompts: it exits 10 naming what is missing, or the CAPTCHA URL and the `pcb login resume` command to run.

### CAPTCHA, timeouts and rate limits

- **CAPTCHA.** Solve it, then `pcb login resume --captcha-token T`. With `--json` the envelope carries `captcha_token` and `captcha_url` in `data`. A login that hits a CAPTCHA while the stored session is still working leaves that session as it was.
- **Timeouts.** Each Proton call of a login has a 30 s deadline: exit 20, run the command again.
- **Rate limits.** A Proton 429 names the wait (`Retry-After`).

## Log out

`pcb logout` revokes the session, through the daemon when it runs.

- When the stored secrets were written by a newer pcb or another sealer, pcb cannot revoke the session. It says so, asks you to end the session in the Proton web app (Settings, Security, Sessions), then to type `discard` (or run `pcb logout --yes`) to remove the local files.
- If another process is syncing, the logout finishes and tells you to run it again once that process stops.
- `pcb logout` keeps the CalDAV clients and the key; `pcb uninstall --delete-data` removes them.

## The sealing key

pcb seals your Proton session (and, with `--unattended-relogin`, your credentials) in `secrets.sealed`, and the CalDAV passwords in `clients.json`, with one key. That key is either:

- a **key file**, by default in the config directory, beside the data; set `[secrets] key_file` to keep it on another mount ([container](container.md#the-key-file));
- or an item in the **desktop keyring** ([below](#the-desktop-keyring-secret-service));
- or, on Windows, a key **protected by DPAPI** for your Windows account ([below](#on-windows-dpapi)).

The key file must be exactly 32 bytes, and readable by pcb's user only: pcb refuses a key file others can read (mode 0444 included; on Windows, an access list that lets another account in).

| Problem | What happens |
|---|---|
| the key file is lost or unreadable | exit 10; `pcb doctor` says so on its `key file` line |
| the key has the wrong size | exit 10, with its size |
| no key file, but `secrets.sealed` present | `pcb client add` and `pcb client rotate` refuse (exit 10): minting a new key would make the stored session unreadable. Restore the key file, or run `pcb logout` then `pcb login`. |
| `secrets.sealed` or `state.json` written by a newer pcb | exit 10: upgrade pcb |

## The desktop keyring (Secret Service)

A state created on a desktop session whose keyring (GNOME Keyring, KWallet) is unlocked keeps its sealing key there, as one item named `pcb sealing key`, instead of a key file beside the data.

### Choose the backend

`[secrets] backend` in `config.toml` chooses, **once, when the state is created**:

| Value | Effect |
|---|---|
| `auto` (default) | the keyring when an unlocked one answers within 2 s, else the key file |
| `keyfile` | always the key file |
| `secret-service` | the keyring; refuses to create the state (exit 10) when the keyring cannot be used |

- The choice is recorded in `layout.json` and never probed again: an existing state, one created before the keyring sealer included, keeps its key file.
- The first command that opens a new state decides, so run it where you want it. A `pcb status` over SSH, without a session bus, fixes the key file.
- `pcb doctor` shows `sealer:`, and says `not chosen yet` before that first command.
- When `auto` falls back to the key file it says why: a warning in the log (`Warning: sealer chosen ...` on a command's stderr) and `pcb doctor`'s `sealer: keyfile (chosen at creation: <reason>)`.

Check that the login keyring is unlocked (Seahorse, Passwords, Login) before the first start.

### What the keyring protects

`pcb` and `pcb-ui` both open what the other sealed. Any process of your login session can read the item, so against a same-user process it is no stronger than a key file; it keeps the key off the disk beside the data.

### When the keyring is locked

A locked keyring (a service started before you logged in) is not an error to fix. The account waits, shows the alert `keyring-locked`, makes no Proton login, and retries every 30 s until you unlock it. pcb never prompts for the keyring.

### When the keyring is not available

When no keyring answers at all (`pcb` over SSH, or a service started outside the desktop session), commands on a keyring state exit 20 with `the desktop keyring is not available` and the hint to run pcb inside the desktop session.

`pcb status` shows `secrets: keyring-unavailable` and the alert `keyring-unavailable`, never `needs-login`: the stored session is intact, so do not log out.

A login checks the keyring first and, when it is locked or not available, refuses before contacting Proton.

### Removing the keyring item

- `pcb uninstall --delete-data` removes the item (a `remove keyring` step after the state directory). When the keyring is locked or not available it deletes the data anyway, and its output says how to delete the item by hand in Seahorse.
- `pcb account remove` keeps it, since the key belongs to the state.

## On Windows: DPAPI

On Windows a new state keeps its sealing key in `secrets.key.dpapi` in the state directory (`%LOCALAPPDATA%\pcb`), encrypted by Windows for your account (DPAPI): only your Windows account, signed in, can decrypt it, and a copy of the file opens nowhere else.

`[secrets] backend` chooses, once, when the state is created:

| Value | Effect |
|---|---|
| `auto` (default) | DPAPI |
| `keyfile` | the key file, a plain 32-byte key whose access list lets only you in (portable, so back it up like a password) |
| `secret-service` | refused, exit 10: the desktop keyring exists on Linux only |

- As with the key file, any program running as you can use the key: DPAPI protects it against other accounts and against a copy of the disk, not against your own processes.
- When Windows cannot decrypt the key in the current session (you signed in without your password, for example over SSH with a key), commands exit 20 with `DPAPI could not unprotect the sealing key for this Windows user`, and `pcb status` shows `secrets: key-unavailable` and the alert `key-unavailable`, never `needs-login`. Nothing is lost: sign in to Windows once with your password, then retry. Do not log out. Creating the key at the first login fails the same way (exit 10, `DPAPI could not protect the sealing key`).
- A reset Windows password (by an administrator, not changed by you) or a new Windows profile loses the key for good: if `key-unavailable` stays after a sign-in with your password, run `pcb logout`, then `pcb login`.
- `pcb uninstall --delete-data` deletes the key with the state directory.

### On Windows: files and the pipe

- **Directories.** Every directory pcb creates (the state and config directories and its own subdirectories, the logs directory included) gets its own access list, not inherited from their parent: full control for your Windows account and for the system (`SYSTEM`), nothing else, with your account as their owner, also when pcb runs as administrator. What they hold inherits that list.
- **Checks.** Where Linux checks a file's mode or owner, Windows checks its access list: a file or directory that lets in an account other than yours, `SYSTEM` or `Administrators` is refused with the same exit codes, and `pcb doctor` prints the `icacls` command that removes that account. A directory must be owned by you (or by Administrators, accepted only while pcb itself runs elevated).
- **Elevated runs.** pcb does not refuse to run as administrator, but `pcb doctor` warns: run it from a normal terminal.
- **The control pipe.** The CLI reaches `pcb-ui` or `pcb serve` over a named pipe whose access list lets only your account in, and which refuses connections from other machines. The bridge checks the account of each client, and the CLI checks that the pipe is owned by your account before it sends anything (a login or a 2FA code crosses it): a pipe owned by another account is refused (exit 20).

### A state moved between systems

A state created on Windows with DPAPI cannot be opened on Linux or macOS, and a state sealed by a Linux desktop keyring cannot be opened on Windows: pcb exits 10 and names the system the state was created on. Use it there, or move the state directory aside and run `pcb login` to start a new one. A key-file state moves with its key file.

## CalDAV passwords

Each CalDAV app password is 24 random bytes (base64url), kept in `clients.json` (0600; on Windows, the state directory's private access list) in the state directory twice:

- as a salted SHA-256, for the check;
- sealed with the key that seals your Proton session, so `pcb client show` can print it again.

Whoever can read both the state directory and that key can read these passwords; the same access already exposes the Proton session. Clients created by a version from before passwords could be shown again keep only their hash. Managing them: [Thunderbird](thunderbird.md#caldav-app-passwords). Why they can be shown again: [viewable CalDAV credentials](../spec/decisions/0013-viewable-caldav-credentials.md).

## Brute-force limits

| What | Limit |
|---|---|
| wrong CalDAV passwords | five from one IP in a minute are refused until the minute ends; one limiter shared by all accounts |
| bad MCP tokens | after five in a minute from one address (IPv6 per /64), 503 with a `Retry-After` |
| logins started by an agent | one a minute per session |
| automatic relogins | at most one per 60 s |

Behind Docker's userland proxy every client shares one IP: see [container](container.md#wrong-passwords-behind-the-userland-proxy).

## TLS certificates

A loopback listener serves plain HTTP; any other address serves TLS 1.2+ only. Without a certificate of your own, pcb generates a self-signed one (ECDSA P-256, 825 days) in `tls/` of the state directory (directory 0700, key 0600; files of another user are refused).

Check the fingerprint: `pcb serve`, `pcb doctor` and `pcb client show` print its SHA-256 fingerprint. Thunderbird shows a certificate exception dialog on first use: confirm it only when its fingerprint is the printed one, and again after a replacement (`pcb serve` logs the old and the new fingerprint).

Full rules (renewal, hostnames, your own certificate): [Thunderbird](thunderbird.md#tls-and-the-certificate). For agents: [agents](agents.md#tls-or-an-ssh-tunnel).

## What never leaves pcb

- Secrets never go in argv, logs or errors.
- Logs carry IDs only. A refused write is logged with its code, calendar, href and reason, never the event's content.
- The MCP audit line never has a title, a description, a link or a secret ([agents](agents.md#audit)).
