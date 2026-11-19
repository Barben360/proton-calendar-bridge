# Security and secrets

How pcb handles your Proton password, 2FA code and session, where it keeps the sealing key (a key file or the desktop keyring), how CalDAV passwords are stored, and how the certificate is checked.

## Log in

### Where secrets come from

Secrets never come from a flag. They come from:

| Source | How |
|---|---|
| a terminal prompt | `pcb login` asks for what is missing |
| a file descriptor | `pcb login --password-fd 3 3<pwfile` |
| a file descriptor | `pcb login --mailbox-password-fd 4 4<mbfile`, the mailbox password of a two-password account |
| the environment | `PCB_USERNAME`, `PCB_PASSWORD`, `PCB_TOTP` (a code or an `otpauth://` URI), `PCB_MAILBOX_PASSWORD` (two-password accounts only); a template is `scripts/pcb.env.example` |

### Login through the daemon

When `pcb serve` runs (or the service, or `pcb-ui`), `pcb login` goes through it, over a control socket only its own user can open (a named pipe on Windows, [below](#on-windows-files-and-the-pipe)).

- A login that waits for its 2FA code, or for the mailbox password of a two-password account, stays in the daemon for 5 minutes in all. `PCB_TOTP=<code> pcb login resume --totp` or `PCB_MAILBOX_PASSWORD=<password> pcb login resume --mailbox-password` finishes it; neither is ever an argument. A wrong mailbox password is checked on your machine and costs no new Proton login; the third wrong one cancels the login.
- With no daemon, `pcb login` says so and logs in on its own.

### Unattended relogin

```sh
pcb login --unattended-relogin
```

It seals the password and, with 2FA, `PCB_TOTP` as an `otpauth://` URI (a one-time code is refused). A session Proton ends is then repaired by one automatic login, at most one per 60 s.

A two-password account works too, and the mailbox password is never stored: pcb keeps the key passphrase derived from it, sealed like the password, and a relogin checks that it still opens the account's keys. If you change the mailbox password in Proton, the relogin stops with "the mailbox password or the account keys changed" and `pcb login` (you, with the new mailbox password) repairs it.

Without it, a login stores tokens only, and you log in again when Proton ends the session. `pcb login resume` takes the same flags.

Only `pcb login` offers it. `pcb-ui` and the login page an AI agent opens never store your password: a login there turns unattended relogin off for that account, and `pcb login --unattended-relogin` turns it back on.

### Scripts: `--non-interactive`

`--non-interactive` never prompts: it exits 10 naming what is missing, or the CAPTCHA URL and the `pcb login resume` command to run.

### CAPTCHA, timeouts and rate limits

- **CAPTCHA.** Solve it, then press Enter at the `pcb login` prompt, or run `pcb login resume --captcha-token T`. With `--json` the envelope carries `captcha_token` and `captcha_url` in `data`. A login that hits a CAPTCHA while the stored session is still working leaves that session as it was.
- **Spacing.** pcb starts at most one login attempt per 60 s. A retry with the token of the CAPTCHA Proton just asked for comes sooner: 10 s after the previous attempt, and `pcb login` and the desktop window say how long they wait when you solved it faster. Any other token waits the full 60 s, a token already sent to Proton included: a rejected one is not retried every 10 s.
- **A login waiting for its code survives a refused retry.** A second `pcb login` (or a wizard retry) inside the 60 s is refused and leaves the login waiting for its 2FA code or mailbox password as it was; once the 60 s have passed, a new login replaces it.
- **A Proton error after the password.** A 5xx, a timeout or a network error while Proton is asked for the 2FA code, the key salts or the user keeps the login waiting for its 2FA code (the session is valid and a code may be spent): submit the code again in a moment. After 3 such errors the login is cancelled, as it is after 3 wrong codes. When the error is a 429 on the 2FA code, the code is not sent again before Proton's `Retry-After` is over (the submit is refused with the wait), and a wait longer than the login's 5 minutes cancels it.
- **Timeouts.** Each Proton call of a login has a 30 s deadline: exit 20, run the command again.
- **Rate limits.** A Proton 429 names the wait (`Retry-After`). When Proton answers a login, or the 2FA code of a login, with a 429, pcb records the wait (at most one hour) next to the login spacing: no login, automatic relogin included, starts before it is over, even from another process or after a restart, and the refusal says how long is left. A GET that Proton answers 5xx with a `Retry-After` is not resent before it either; one over 60 s is reported at once.

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
| automatic relogins | at most one per 60 s, and none before a login 429's `Retry-After` is over |

Behind Docker's userland proxy every client shares one IP: see [container](container.md#wrong-passwords-behind-the-userland-proxy).

## TLS certificates

A loopback listener serves plain HTTP; any other address serves TLS 1.2+ only. Without a certificate of your own, pcb generates a self-signed one (ECDSA P-256, 825 days) in `tls/` of the state directory (directory 0700, key 0600; files of another user are refused).

Check the fingerprint: `pcb serve`, `pcb doctor` and `pcb client show` print its SHA-256 fingerprint. Thunderbird shows a certificate exception dialog on first use: confirm it only when its fingerprint is the printed one, and again after a replacement (`pcb serve` logs the old and the new fingerprint).

Full rules (renewal, hostnames, your own certificate): [Thunderbird](thunderbird.md#tls-and-the-certificate). For agents: [agents](agents.md#tls-or-an-ssh-tunnel).

## The update check

`pcb-ui` and `pcb doctor` ask github.com once a day whether a newer stable release exists; `pcb version --check` asks when you run it. pcb never downloads nor installs anything.

| | |
|---|---|
| What is sent | One request to `https://github.com/Barben360/proton-calendar-bridge/releases/latest`, with `User-Agent: pcb`: no version, no system, no cookie. GitHub sees your IP address, as for any visit. |
| What is read | Only where that page redirects (the newest stable release's tag). Pre-releases are never offered. |
| When | At most once a day: the answer is kept in `update.json` in the state directory. A build that is not a release (`dev`, a git describe) never asks. |
| Turn it off | `pcb update-check off`, or Settings, "Check for updates" in `pcb-ui`; `PCB_UPDATE_CHECK=off` in the environment for one process. `pcb version --check` still asks: it is an explicit request. |
| A failed check | Says nothing: no alert, no change to the tray or to an exit code. |

## Crash reports

When pcb crashes, or hits an error that points to a bug in pcb, it keeps a report on disk and offers it to you. Nothing is sent until you say yes (or choose "always"). The report lets the author fix the bug.

| | |
|---|---|
| What is kept | A fatal crash of `pcb` or `pcb-ui` (a panic or a Go runtime error), an unexpected error that pcb cannot classify, and a panic in a CalDAV or MCP request handler. Not kept: Proton being down, rate limits, network errors, wrong credentials, usage errors. A build without a crash report address (one built from source, a dev build) keeps nothing. |
| What a report holds | Only fields pcb picks, one by one: the release (`pcb@vX.Y.Z`), the operating system name and the CPU architecture, the Go version, which binary (`pcb` or `pcb-ui`), which command (`pcb serve`, never its arguments), how pcb was installed (container, script, installer or other), the kind of failure, the names of pcb's functions and their file and line in the stack, and how many times it repeated. The text of a panic or error is dropped: it only stays when it is a message of Go's runtime (`index out of range`, `nil pointer dereference`). |
| On your machine meanwhile | While a `pcb` or `pcb-ui` process runs, Go's runtime writes a crash's full traceback to a file in `reports/` of the state directory (mode 0600). It can hold the panic text, argument words and the paths of your machine. The next start of any pcb process turns it into the stripped report and deletes it. With the mode `never` (or `PCB_CRASH_REPORTS=never`) that file is not written at all. |
| What a report never holds | Calendar events, account or calendar names, addresses, Proton IDs, tokens, passwords, your user name, the machine's name, any path of your machine, environment variables, arguments. |
| Where it goes | `https://errors.bbarrois.tech`, the author's own error tracker (Bugsink). Nowhere else. The server sees your IP address, as for any visit; it is configured not to record it. |
| When | After you answered "Send" or "Always send" in `pcb-ui`, or ran `pcb crash-reports send`, or with the mode `always`. In `always`, `pcb serve` and `pcb-ui` send in the background, at start and when a report is saved; a short command (`pcb status`, a health check, a cron `pcb sync`) never sends. |
| See it | `pcb crash-reports show` prints exactly what would be sent (the saved report is that very text); "Show report" in `pcb-ui`. |
| Turn it off | `pcb crash-reports never`: no report and no crash file is kept, and those waiting are deleted. `PCB_CRASH_REPORTS=never` does it for one process (`always` forces sending the same way). Back to asking: `pcb crash-reports ask`. |
| Delete | `pcb crash-reports discard`, or "Don't send" in `pcb-ui`. |
| Limits | At most 10 reports are sent a day per install, at most 20 wait (the oldest goes first), and a report that failed 3 sends is dropped. A server's "slow down" (`Retry-After`) is honoured. The same cause is kept once a day. A failed send says nothing outside `pcb crash-reports send`. |

The modes, kept in `reports.json` in the state directory (the reports wait in its `reports/` folder, with lock files beside them):
- `ask` (the default): reports wait for your answer. `pcb-ui` asks in a dialog at start, or in a panel when a report is saved while it runs. Closing the dialog changes nothing: it asks again at the next start.
- `always`: reports are sent as they come. Choose it in the dialog, with "Send crash reports" in Settings, or `pcb crash-reports always`.
- `never`: nothing is kept.

A command that is not `pcb crash-reports` never asks. While reports wait in `ask` mode, `pcb doctor` shows a warning on its `crash reports` line, `pcb serve` logs `crash reports waiting: pcb crash-reports show` at start, and a command you run in a terminal (without `--json`) prints `pcb kept a crash report: pcb crash-reports show` on stderr, at most once a day. A pcb run as root refuses to keep reports in another user's state directory, like its other files.

## What never leaves pcb

- Secrets never go in argv, logs or errors.
- Logs carry IDs only. A refused write is logged with its code, calendar, href and reason, never the event's content.
- The MCP audit line never has a title, a description, a link or a secret ([agents](agents.md#audit)).
- Crash reports carry none of that either, and leave only after you agree ([crash reports](#crash-reports)).
