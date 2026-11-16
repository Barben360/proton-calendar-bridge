# 0021 — Two-password mode is supported

- **Date**: 2026-11-16
- **Decided by**: author (the mailbox password is asked in a step after the 2FA code, with an optional upfront value; an unattended relogin reuses the sealed key passphrase and stores no mailbox password; the credentials and 2FA wizard screens get labelled fields, focus on the first field and Enter to submit; the live check switches the test account to two-password mode and back), the rest the agent's calls (design `spec/designs/2026-11-16-two-password-mode-design.md`)
- **Status**: accepted; built 2026-11-16; amends decision 0010 (two-password mode "comes later" becomes supported), SPEC §3 (removed from "Not planned") and SPEC §5.2 (the `[mailbox-password]` step is built)

A Proton account in two-password mode has a login password (it signs in) and a mailbox password (it decrypts the account's keys). pcb logs such an account in from every entry: `pcb login`, `pcb-ui`'s add-account wizard and the MCP login page. FIDO2 is still refused.

- **When it is asked.** After the 2FA code (order: CAPTCHA, 2FA, mailbox password), and only when Proton answers `PasswordMode: 2`. Upfront, for a one-shot non-interactive login: `--mailbox-password-fd N`, else `PCB_MAILBOX_PASSWORD`; otherwise a prompt (`Mailbox password: `, no echo), a wizard screen ("Mailbox password", "Unlock") or the login page's `mailbox-password` step. A non-interactive login with none held by a daemon exits 10 with `pcb login resume --mailbox-password`; with no daemon it is cancelled.
- **What it does.** The login password drives SRP and the scope unlock (the key salts); the mailbox password drives only the key passphrase. Its check is local (the address keys open or not): a wrong one makes no Proton call, keeps the login held, and counts toward 3 tries of its own (the third revokes the session and cancels the login). One 5-minute deadline from `Login` covers the whole held login.
- **Never stored.** Not in a log line, an error, a `--json` envelope, argv, `state.json` or the sealed store; zeroed after the derivation, redacted in every request type. A mailbox password given upfront to a single-password account is ignored.
- **Unattended relogin.** The sealed key passphrase (`KeyPass`) is reused: SRP and 2FA with the sealed password and TOTP seed, then a check that `KeyPass` still opens the address keys. It no longer does: needs-login, "the mailbox password or the account keys changed", hint `pcb login`.
- **Interfaces.** State `needs-mailbox-password`; loginflow step `mailbox-password`; ctl `POST /v2/accounts/{id}/login/mailbox-password` (additive to v2); `pcb login resume --mailbox-password`; pcb-ui labels the phase "needs mailbox password".
- **Checks.** `cmd/pcb-fake` accounts take an optional `mailbox_password`; `make rehearse` logs one in (held, one wrong mailbox password, the right one, one SRP only). One live check on a throwaway Linux VM of its own (`spec/HUMAN-CHECKS.md` step 14).

Why:
- The reporter of the issue uses two-password mode as a Proton security feature; switching the whole account to single-password to use a bridge is the wrong trade.
- A step after 2FA mirrors Proton's own apps and the TOTP design, and asks only the accounts that need it; the upfront value keeps a one-command script possible.
- A retry must not cost a second SRP: each costs the 60 s login guard and risks a CAPTCHA.
- Storing the mailbox password would be one more secret that breaks exactly when the sealed key passphrase does (when the mailbox password changes).

Consequences and limits:
- A mailbox password changed in Proton ends unattended relogin for that account until a person runs `pcb login` with the new one; there is no way to recover on its own.
- The live check runs on Linux only, on a throwaway VM of its own (the CLI, then the `pcb-ui` wizard, then a single-password `pcb login`); the MCP login page runs the wizard's login flow and is covered offline, not live; Windows is trusted to CI's `windows` and `windows-test` jobs and `GOOS=windows go vet ./...`. On Windows a descriptor is only 0, 1 or 2, so a non-interactive login pipes the login password on 0 and gives the mailbox password through `PCB_MAILBOX_PASSWORD`.
- The live check needs the author to switch the test account's mode in Proton's settings twice, and takes exactly three logins 60 s apart (the CLI with a wrong then the right mailbox password, the wizard, a single-password `pcb login` after the switch back); `pcb-lab`'s write-matrix probe and the seed tool still derive the key passphrase from the login password, so the account must be single-password again for them.
- The fake derives the second passphrase its own way: the rehearsal shows the flow, not Proton's real key salts.
- Cost if wrong: `internal/service/login.go` and `session.go` (the held step, the relogin check), `internal/cli/login.go` and `creds.go`, `internal/loginflow`, the wizard screen in `internal/ui`, the login page step, `cmd/pcb-fake`'s `mailbox_password`; removing it brings back the refusal for two-password accounts, whose owners then switch to single-password mode.
