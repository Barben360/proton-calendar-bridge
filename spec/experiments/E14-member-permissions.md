# E14 — Member `Permissions` bits

Status: **source-based; the M4 write gate relies on it; live run pending for owned calendars** (`spec/HUMAN-CHECKS.md` §6, step 6.1.4); shares (view, edit) need a second test account and are not planned (shared calendars are out of scope, SPEC §3). Marks as in E10. Evidence: `raw/m4-research.md` §H. Feeds ruling M4-R1, SPEC §5.4.

## Question

Which bit of a calendar member's `Permissions` allows writing events, and what value does our member have on a calendar we own, on a holidays calendar, on a subscribed one?

## Source evidence

| Point | Evidence | Mark |
|---|---|---|
| `Availability=1, Read=2, ReadMemberList=4, Write=8, Admin=16, Owner=32` | caloxide `protonmail/calendar.go:83-92` (no stated origin) | [claim: caloxide] |
| go-proton-api has the field with "TODO: What is this?" | `go-proton-api/calendar_types.go:81-91` | [source] |
| SevenOfNine treats any value above 0 as writable (wrong for an availability-only share) | `internal/provider/proton.go:80-84` | [source] |
| Where it is: `GET /calendar/v1` `Calendars[].Members[].Permissions`, and the v2 bootstrap's `Members[].Permissions` | research H | [source] |

## What the code assumes

`internal/proton/calendar/unlock.go`: `PermissionWrite = 8`, read from **our** member's entry in the bootstrap. The write gate is open only for a normal calendar, our member's `Permissions & 8`, a verified member passphrase signature (E10) and both write keys (rulings M4-R1, M4-R39). A Write bit that is off closes the gate: privileges read only, writes 403 `need-privileges`. Holidays and subscribed calendars are closed by their type whatever the bits. [verified: `TestWritableNeedsTheWriteBit`, `TestPermissionsAreOurOwnMembers`, `TestGateVerdicts`, `TestPrivilegesFollowTheWriteGate`]

The fake Proton serves 127 for our member and 1 for a foreign one, so the offline suite cannot tell whether 8 is the right bit.

## Live run that settles it

`spec/HUMAN-CHECKS.md` §6, step 6.1.4:
1. `bin/pcb calendar list --json`: the owned default calendar shows `read_only` false (the Write bit is set **and** E10's signature verifies); holidays and subscribed calendars `read_only` true.
2. Only if 1 fails, and only if the lab session of step 2a is still alive (no new login): `bin/pcb-lab probe calendar-access --record-dir …` records the calendar list (redacted; `Permissions` is kept) and the first calendar's v1 bootstrap; `jq` prints each calendar's type and member `Permissions`.
3. Shares: skipped (needs a second account).

Results: not run yet. Record the value of `Permissions` per calendar type when step 2 runs, else "owned calendar writable: [verified: live]".

## Decision it feeds

Another bit for writing: change `PermissionWrite` (test first). A value of 0 or without the bit on an owned calendar while the web app writes it: the bit is not where we read it (perhaps only in `/calendar/v1`'s member list); compare both sources before any change.
