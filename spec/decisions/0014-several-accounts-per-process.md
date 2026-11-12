# 0014 — Several Proton accounts per process, per-account CalDAV URLs

- **Date**: 2026-11-12
- **Decided by**: author (several accounts in the core, so `pcb serve` and the CLI get them too), the rest the agent's calls (M6 design, `spec/designs/2026-11-12-multi-account-and-ui-design.md`)
- **Status**: accepted, built in M6; amends 0013 (the username shown is the account's address), SPEC §3 (GUIs are planned, M7), §4 and §13 (one account per daemon)

One `pcb serve` process holds every Proton account of a state directory (`service.Hub`, one `service.Local` per account) behind one CalDAV listener and one control socket.

- **Layout v2.** Each account lives in `<state>/accounts/<local-id>/` (`state.json`, `secrets.sealed`, `clients.json`, `pcb.db`, their locks and its own `sync.lock`). `<local-id>` is 8 random lowercase hex characters, never reused or renamed. `layout.json` `{"version": 2}` marks the layout; the first command that opens an M5 state moves it in, crash-safe, under `layout.lock` and the root `sync.lock`. No downgrade: an M5 binary sees no root `state.json` and, if it logs in, writes one beside `layout.json`; M6 then refuses and names the files (exit 10: a person must act; a newer or damaged `layout.json` too, ruling R36).
- **Per-account CalDAV URLs.** `/dav/<local-id>/` (realm `pcb <local-id>`), and the compatibility URLs `/`, `/.well-known/caldav`, `/dav/principals/me/`, `/dav/calendars/me/...` (realm `pcb`) where the password alone picks the account. A migrated install's Thunderbird keeps working untouched; a new setup ends on the per-account URL. The username shown is the account's Proton address (any username is accepted), which amends 0013.
- **Account selector.** `--account A` or `PCB_ACCOUNT` (address, case-insensitive, or local ID), optional with exactly one non-pending account; with several: exit 2, `several accounts: name one with --account`, the list in `data.accounts`; with none to choose (no account, or only pending adds): exit 10, `no account` (ruling R38).
- **Control socket `/v2/`**, `output.SchemaVersion` 2; no shim for readers of version 1 (private, pre-release).

Why:
- The author wants a desktop experience like Proton Mail Bridge (M7) and the same codebase headless; N accounts in one process is the shared part.
- Two accounts on one Thunderbird profile share host and port: one realm and one path set would let its HTTP auth cache send account A's password to account B's URLs. Distinct realms and prefixes keep them apart. That Thunderbird really keeps them apart is not verified (`spec/HUMAN-CHECKS.md` step 8, `spec/OPEN.md`); the design does not depend on the outcome, because a wrong password on an account's URL is a 401, never another account's data.
- The password-only compatibility URLs cost a few salted hashes per request over 192-bit random passwords (no ambiguity) and save every existing install a reconfiguration.

Consequences and limits:
- Multi-account is proven against the fake Proton only (two or more users). The author has no second Proton test account (decision 0005 stands; recorded 2026-11-12): the live checks use the one test account and cover the migration, one account and Thunderbird on the per-account URL.
- One failure limiter (per source IP) serves every account, so one account's wrong passwords count against all (every Thunderbird comes from 127.0.0.1). Two accommodations (rulings R28, R29): a client of an account whose CalDAV is off gets 401 without a charge, and while any account's `clients.json` is broken a password no other account holds is not charged.
- One login guard at the root spaces every account's logins by the same interval.
- Cost if wrong: a rename of the URL scheme (the compatibility URLs stay either way).
