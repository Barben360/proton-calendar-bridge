# Two-password mode — design

- **Date**: 2026-11-16
- **Status**: built 2026-11-16 (approved 2026-11-16 by the author, reviewed by a builder check (issue https://github.com/Barben360/proton-calendar-bridge/issues/41); decision 0021)
- **Decided by**: author (choices marked "author"); the rest is the agent's call, marked [call]
- **Amends**: decision 0010 (two-password mode "comes later" becomes supported), SPEC §3 (removed from "Not planned"), SPEC §5.2 (the `[mailbox-password]` step is built); recorded as decision 0021 once built

## 1. Intent

A Proton account in two-password mode has a login password (it signs in) and a separate mailbox password (it decrypts the account's keys). pcb refuses these accounts today (`auth.ErrTwoPassword`, `internal/proton/auth/login.go`), so their owners must switch the whole account to single-password mode to use the bridge. The reporter of issue 41 wants to keep two-password mode: it is a Proton security feature they chose. pcb should log such accounts in from all three login entries (`pcb login`, `pcb-ui`'s add-account wizard, the MCP login page), alongside 2FA and CAPTCHA, without weakening how credentials are handled.

## 2. Goals, non-goals, anti-goals

Goals:
- A two-password account logs in through `pcb login` (interactive and non-interactive, with or without a running daemon), the `pcb-ui` wizard and the MCP login page.
- The mailbox password is asked only of accounts that need it, after the 2FA code, as Proton's own apps do.
- A wrong mailbox password can be retried without a new Proton login.
- Unattended relogin keeps working for a two-password account, without storing the mailbox password.

Non-goals:
- FIDO2 (still refused).
- Unattended relogin changes for single-password accounts (they keep re-deriving the key passphrase from the stored password).
- Switching an account between modes from pcb.

Anti-goals (failure even if it works):
- The mailbox password in a log line, an error message, a `--json` envelope, a command-line argument, `state.json` or the sealed store.
- One more Proton login (SRP) to recover from a missing or wrong mailbox password: each costs the 60 s login guard and risks a CAPTCHA.
- Single-password and 2FA logins behaving differently from today.

## 3. Constraints and facts

- The login password drives SRP (`auth.Begin`) and the scope unlock that fetches the key salts (`auth.Scope`, `PUT /core/v4/users/unlock`); its doc comment already says so for two-password mode.
- The mailbox password drives only the key passphrase: `keys.DeriveKeyPass(user, addrs, salts, mailboxPassword)`. Its check is local (the address keys open, else `keys.ErrKeysLocked`): a wrong one costs no Proton call.
- Proton answers `PasswordMode: 2` (`gpa.TwoPasswordMode`) in the `/auth/v4` response, before the 2FA step.
- The fake Proton (`internal/proton/protontest`) has `Options.TwoPassword` (answers `PasswordMode: 2`) but locks the keys with the login password.
- Repository rules: secrets never in argv (ruling M5-P11), exit 10 for what the user must act on, docs updated in the same pull request, fresh `pcb-ui` screenshots for a window change, live checks on the test account only.

## 4. Approaches

When the mailbox password is asked (author picked A):
- **A. A step after 2FA, plus an optional upfront value.** Asked only when Proton says two-password; `PCB_MAILBOX_PASSWORD` or `--mailbox-password-fd` give it upfront for a one-shot non-interactive login. Mirrors the TOTP design (prompt, or `PCB_TOTP` upfront).
- B. Upfront only: a second field for everyone, mostly left empty; a non-interactive login that forgot it fails after spending a Proton login.
- C. Step only: a script without a daemon cannot finish in one command.

Unattended relogin of a two-password account (author picked A):
- **A. Reuse the sealed key passphrase.** The relogin runs SRP and 2FA with the sealed login password and TOTP secret, then checks that the sealed `KeyPass` still opens the address keys. No mailbox password is stored.
- B. Also seal the mailbox password: one more secret stored, and it breaks exactly when A breaks (the mailbox password changed).

## 5. Design

### 5.1 auth

- `Begin` no longer refuses `PasswordMode == 2`: it sets `Pending.TwoPassword = true` and returns the pending login. `ErrTwoPassword` and its mapping in `internal/service/errors.go` are removed. FIDO2 refusal is unchanged.
- The `keys.ErrKeysLocked` mapping (`internal/service/errors.go`, hint "accounts in two-password mode are not supported", checked by `errors_test.go`) loses that hint: "check the password" for a single-password account, the mailbox-password message of §5.2 for a two-password one [call]. `keysChanged` (`internal/service/calendars.go`, "password changed?") becomes "password or mailbox password changed?" [call].
- `Scope` keeps the login password.

### 5.2 service (Local)

- `LoginRequest.MailboxPassword string` (redacted like the rest of the request). Ignored for a single-password account [call].
- New login state `StateNeedsMailboxPassword = "needs-mailbox-password"`, a `LoginResult` like needs-totp (`LoginExpires` set).
- Order: CAPTCHA, then 2FA, then the mailbox password.
- `complete` is split [call]: after 2FA it fetches the salts (with the scope unlock), the user and the addresses once, and keeps them in the `pendingLogin`. Then:
  - single-password: derive from the login password, as today;
  - two-password with a mailbox password at hand (upfront or submitted): derive from it;
  - two-password without one: the login is held (`hold`) waiting for it, `LoginResult{State: needs-mailbox-password}`.
- `Local.SubmitMailboxPassword(ctx, password string) (LoginResult, error)`: derives from the kept salts, user and addresses; no Proton call. On `keys.ErrKeysLocked`: exit 10 "the mailbox password does not unlock the account keys", hint with the tries left; the login stays held. After 3 wrong passwords in all [call, as `maxTOTPTries`] the session is revoked and the login cancelled ("run pcb login again"). Its own counter, separate from the 2FA tries.
- One expiry for the whole held login: the 5 minutes start at `Login` and are not reset by the 2FA step [call: one deadline, as today's `hold` keeps the first].
- The held login records which step it waits for; `Status` reports `needs-totp` or `needs-mailbox-password` from it (reason "a login is waiting for its mailbox password"). `waitingLogin` stays the "a login is held" test the Hub uses for pending accounts.
- `SubmitTOTP` on a login waiting for its mailbox password, and the reverse, is exit 10 "no login is waiting for a 2FA code" / "… for a mailbox password" [call].
- The pending login holds the mailbox password only from submission to derivation, then zeroes it, as the login password (`release`).
- A login that was given an upfront mailbox password and is held for its 2FA code keeps that mailbox password until it ends [call].

### 5.3 service (Hub), ctl

- `Hub.SubmitMailboxPassword(ctx, selector, password)`, picking the held login as `SubmitTOTP` does (`pickWaiting`, then `settle`); logged as "login mailbox password", outcome `needs-mailbox-password`, `ready` or `refused`, never the password.
- `Hub.logOutcome` maps `needs-mailbox-password` as it maps `needs-totp`.
- ctl: `POST /v2/accounts/{id}/login/mailbox-password` with `{"password": …}` and `Client.SubmitMailboxPassword`; additive to the v2 API.

### 5.4 Unattended relogin

- `reloginOnce` with `p.TwoPassword`: after SRP and 2FA it fetches the user and addresses (no salts, no scope unlock) and opens the keys with the sealed `acc.KeyPass` (`keys.Unlock`). Success: the new tokens are sealed with the same `KeyPass`.
- The sealed passphrase no longer opens the keys: needs-login, reason "the mailbox password or the account keys changed", hint "run pcb login".
- `pcb login --unattended-relogin` on a two-password account seals the login password and the TOTP URI as today, never the mailbox password.

### 5.5 CLI

- `pcb login`: after the 2FA step, on needs-mailbox-password, interactive: prompt `Mailbox password: ` (no echo), up to 3 times while the login stays held, as `promptTOTP`.
- Upfront: `PCB_MAILBOX_PASSWORD`, or `--mailbox-password-fd N` (read to end of input, as `--password-fd`); the flag wins over the variable [call]. The same descriptor for both flags is a usage error (exit 2) [call]. On Windows a descriptor is only 0, 1 or 2 (`internal/cli/descriptor_windows.go`), so a non-interactive Windows login pipes the login password on 0 and gives the mailbox password through `PCB_MAILBOX_PASSWORD`; `docs/commands.md` says so [call]. Sent in `LoginRequest.MailboxPassword`.
- Non-interactive, no value, daemon running: exit 10, the daemon holds the login, hint `pcb login resume --mailbox-password before HH:MM:SS (the daemon holds the login)`.
- Non-interactive, no value, no daemon: exit 10 "the account uses two-password mode: set PCB_MAILBOX_PASSWORD or pass --mailbox-password-fd", the login is cancelled.
- `pcb login resume --mailbox-password` (a switch, like `--totp`; exactly one of `--captcha-token`, `--totp`, `--mailbox-password`): reads `--mailbox-password-fd`, else `PCB_MAILBOX_PASSWORD`, else the prompt; refuses `--username`, `--password-fd`, `--unattended-relogin`. Needs a daemon, as `--totp`.
- `--json`: `state: "needs-mailbox-password"` with `login_expires` in the data of a held login.

### 5.6 loginflow, pcb-ui, MCP login page

- `loginflow`: `StepMailboxPassword = "mailbox-password"`, `Flow.SubmitMailboxPassword(ctx, password []byte)`; `settle` goes there on needs-mailbox-password (the login password is wiped, as for 2FA); a wrong password keeps the step while the Hub holds the login, else back to the credentials; `Cancel` treats it as `StepTOTP`. `Hub` interface gains `SubmitMailboxPassword`.
- `pcb-ui` wizard: a new screen, built like the 2FA one (existing widgets and theme, no new colours):

  ```
  Mailbox password
  This account uses two-password mode: enter its mailbox password
  (the second password, which decrypts your data).
  ⚠ The mailbox password does not unlock the account keys (2 tries left)
  Mailbox password
  [••••••••••••••••            ]
                                       [Cancel]  [Unlock]
  ```
  The field has a label, gets focus, Enter submits, and the error line sits above the field as on the other screens. The credentials and 2FA screens get the same in this pull request: a label per field, focus on the first field, Enter submits (author), so the wizard meets the accessibility rules throughout; `TestWizardScreensFollowSteps` and the screenshots follow. The account list labels the phase "needs mailbox password", like "needs 2FA code" (`internal/ui/model.go` `short()`) [call].
- MCP login page: a `mailbox-password` form step, a password input with a label, same text.

### 5.7 Fake Proton and tests

- `protontest.Options.MailboxPassword`: with `TwoPassword`, the user's keys are locked with the passphrase salted from it, not from the login password.
- `cmd/pcb-fake`: an account gains an optional `mailbox_password` (two-password when set), so `make rehearse` (`scripts/rehearse.sh`) can rehearse the new live step offline before the live run, as `spec/HUMAN-CHECKS.md` step 0 requires [call].
- Tests (each must go red if the behaviour goes): two-password login with the mailbox password upfront; held then submitted; a wrong one then the right one (no second SRP: the fake counts `/auth/v4` calls); three wrong ones cancel and revoke; 2FA then mailbox password; a single-password account ignores an upfront mailbox password; `SubmitTOTP` and `SubmitMailboxPassword` refused on the wrong step; expiry; relogin with the sealed passphrase (succeeds; and needs-login after the fake changes the keys); nothing sealed contains the mailbox password; the CLI paths of §5.5 (prompt, variable, fd, held-by-daemon hint, no-daemon refusal, resume); loginflow steps and Cancel; the wizard screen; the login page step.

### 5.8 Docs and live check

- Decision 0021 (two-password mode supported; mailbox password never stored), `INDEX.md`, a status note on 0010; SPEC §3 and §5.2.
- `README.md` (secrets line), `docs/commands.md` (`pcb login`, `pcb login resume --mailbox-password`), `docs/security.md` (variables table; unattended relogin seals no mailbox password), `docs/desktop.md` (wizard step), `docs/troubleshooting.md` if it quotes the old refusal; screenshots (`make screenshots`).
- `scripts/pcb.env.example` gains `PCB_MAILBOX_PASSWORD`; the live run reads it from the local copy in `~/.config/pcb-live/`, like the other test-account credentials [call].
- `spec/HUMAN-CHECKS.md`: a live step. The author switches the test account to two-password mode in Proton's settings; one `pcb login` (2FA, then mailbox password); one wrong mailbox password first; then the author switches the account back to single-password mode (author: pcb-lab's write-matrix probe and the seed tool derive the key passphrase from the login password, so they stay unchanged), and the step checks a single-password login works again. One login per run, 60 s between logins. The live check runs on the Linux test VM only: CLI, `pcb-ui` wizard and MCP login page there. No live check on Windows (author): Windows is trusted to CI's `windows` and `windows-test` jobs and `GOOS=windows go vet ./...`.

## 6. Left to the builder

- Function and file names within the packages above, the exact wording of messages and hints (plain words, no internal labels on user pages).
- How `complete` is split, as long as §5.2's "no Proton call on a retry" holds.
- How the fake derives the second passphrase.
