# Initial-version audit (2026-11-12)

The last quality gate of the initial version (M0 to M5, SPEC §18): what was audited, how, what was found, what became of each finding and what held up.

## Scope and method

- **Scope**: six areas of main at 557fe84 (the commit M5 merged at): `proton` (transport, login, keys, calendar API client, the fake), `state` (store, sealed secrets, cache, locks, durability), `surface` (the CalDAV listener, TLS, access control, the control socket), `read` (sync, render, the iCalendar merge, tokens), `write` (the PUT and DELETE path, the trust model) and `ux` (the CLI, doctor, install, exit codes, messages, README and SPEC against the binary).
- **Method**: six read-only auditors (Sonnet, one per area), each on a detached checkout, with the protontest fake only and no network call to Proton. A finding needed a severity, `file:line`, a concrete scenario (inputs and state to wrong output), evidence (a quoted code line, a test, a command output), a one-line fix and a confidence (Confirmed or Plausible). Known open items (`spec/OPEN.md`) were not to be re-reported. A triage by the controller ruled each finding fixed (F), parked (P), accepted (A) or docs-only (D); the fixes were made test first, each with a recorded sabotage, and each task was reviewed by a separate Sonnet reviewer.
- **Plan and evidence**: `spec/plans/2026-11-12-audit-fixes.md` (the rulings table); the auditors' reports and the task reports are in the session workspace (`.superpowers/sdd/2026-11-12-audit-fixes/`, not in the repository).
- **Counts**: 0 Critical, 17 Important, about 48 Minor. Important by area: proton 2 (I1, I2), state 6 (I1 to I6), surface 2, read 1, write 3, ux 3.

## Process incident

The `ux` auditor ran `pcb login` once with fake credentials (`PCB_USERNAME=u PCB_PASSWORD=p`) to see the non-TTY path. The command is not offline: it contacted Proton's real API and got a CAPTCHA challenge back. No real account and no secret was involved, the state was in a scratch directory that was deleted, and nothing was stored. It is also a finding in itself: `pcb login` has no offline mode, so the CLI's prompt flow cannot be tried against nothing. Every auditor, implementer and reviewer prompt now forbids `pcb login` against the real Proton API (the protontest fake only). A second incident of M4 (a reviewer fetching GPL client code) is in `OPEN.md`, "Still open for the author", item 4.

## Outcome per finding

Legend: **fixed** (commit on branch `audit-fixes`, subject in brackets), **parked** (reason in `spec/OPEN.md`, "Parked by the initial-version audit"), **accepted** (as is, reason here), **docs** (this commit).

Commits: T1 `bc93229` and `e0a5c83` (Bound and classify the login calls); T2 `a417c8d` and `3c6716e` (Keep the state readable and durable across versions and crashes); T3 `ecc3fa4` (Bound the network surface) and `edc511b`; T4 `3ba69b6` and `6f6ee08` (Fix the read path); T5 `e82bf54` and `18c8ba3` (Log write refusals and stop self-conflicts; fail closed on a blank If-Match); T6 `adfa6ee` and `edd9428` (Consistent exit codes and next steps); docs: `📝 Record the initial-version audit`.

### proton

| Finding | Outcome |
|---|---|
| I1 no timeout on login, 2FA, AuthInfo and unlock calls | fixed T1: a 30 s deadline per call |
| I2 any 422 means "wrong password" | fixed T1: only code 8002, as the scope unlock already did |
| M1 refresh holds `c.mu` up to 30 s, `Tokens()` ignores ctx | parked |
| M2, M7 Retry-After lost on a login 429; HTTP-date form ignored | fixed T1: `ParseRetryAfter`, clamp 1 h, the wait is named |
| M3 key passphrase not zeroed | fixed T1 |
| M4 = state-I3 future-dated login stamp | fixed T1: a stamp over 5 min ahead is stale, warned once, never blocks |
| M5 passphrase verify time against cards time 0 | parked: crypto semantics, needs a live check |
| M6 9001 on an authenticated read untyped | fixed T1: alert `human-verification` |

### state

| Finding | Outcome |
|---|---|
| I1 a store this build cannot read is a dead end | fixed T2: names the cause, `upgrade pcb`; `pcb logout --yes` is the confirmed local wipe |
| I2 state.json with an unknown field hints "logout" | fixed T2: stays strict, says "written by a newer pcb: upgrade pcb" |
| I3 = proton-M4 | fixed T1 |
| I4 a CAPTCHA while ready overwrites a working state | fixed T1: the stored state is left as it was |
| I5 `synchronous(NORMAL)` lets the sequence roll back | fixed T2: `synchronous=FULL` (+5 to 8 ms per incremental pass, measured) |
| I6 the cache migration runs in two transactions | fixed T2: one transaction; a v2 file with rows and no pins is backfilled on open (T2 round) |
| M root run on a system install | fixed T2: refuse as root against another user's state (exit 10) |
| M logout does not take sync.lock | fixed T2, in a form that says so instead of waiting (see the T2 report; `sync_running`) |
| M stale `.tmp-*` and `.key-*` | fixed T2 (1 h sweep; key dir sweeps `.key-*` only) |
| M no directory fsync after removing secrets.sealed | fixed T2 (and the cache file) |
| M pcb.db mode not repaired | fixed T2 |
| M tombstones never pruned; login after a rollback forces a logout | parked |
| M7 (T2 review) logout of a no-key or corrupt store wipes unconfirmed | parked |

### surface

| Finding | Outcome |
|---|---|
| I1 property names echoed with unbounded length | fixed T3: 400 over 256 bytes |
| I2 corrupt clients.json gives a silent 401 | fixed T3: 503 with Retry-After, one error log per distinct failure, no limiter charge |
| M1 `0.0.0.0` also binds `[::]` | fixed T3: `tcp4` for an IPv4 literal |
| M2 SANs list every interface IP; M3 no regeneration when an IP changes; M5 no renewal while running; M6 a local user can lock out loopback Thunderbird | parked |
| M4 fingerprint change logged at Info | fixed T3: Warn with the old and the new fingerprint |
| M7 a WARN per failed TLS handshake | fixed T3: Debug |
| M8 `/.well-known/caldav` answers 308 to every method | accepted: harmless, a switch-order nit; the redirect sits behind authentication |
| M9 8 MiB PROPFIND and REPORT bodies, no connection cap | fixed T3: 1 MiB cap; the connection cap is parked |

### read

| Finding | Outcome |
|---|---|
| I1 render counters never pruned | fixed T4, with a fix round for the full and the incremental `Changes` path; the concurrent-snapshot undercount is parked |
| M1 duplicate RECURRENCE-ID both emitted | fixed T4: the most recently edited wins, the rest count as duplicates |
| M2 no control-character guard on read | fixed T4 (also a property whose name holds one; `FuzzMergeEvent` checks it) |
| M3 recurring timed placeholder in UTC | fixed T4: the row's own tzdata zone |
| M4 one malformed reminder makes a placeholder | fixed T4: that alarm is dropped and counted (`dropped_alarms`) |
| M5 O(N) per single GET; M7 a full listing holds every Raw; M8 first-wins ATTENDEE dedup | parked |
| M6 "LRU 2 000 by default" | docs: fixed at 2 000, not configurable (SPEC §7.2, E09) |

### write

| Finding | Outcome |
|---|---|
| I1 refusals not logged | fixed T5: one `refused a CalDAV write` Warn per refusal, no content |
| I2 UID column against parts divergence gives a 409 self-conflict | fixed T5 as a stop-gap: unescaped comparison, 503 at the object's own name; the full fix is parked behind live check W10 |
| I3 the personal part is ignored by the read-back and not re-sent | fixed T5: an update of such a row is refused, 403 `pcb:personal-part`, until L.4 #10 (needs a lab probe) |
| M1 the docs say an unchanged part is re-sent with its stored signature | docs: every part of an update is sealed again (mapping §9) |
| M2 README omits the silent 2xx for attendee parameter edits | docs: README "Writes from Thunderbird" |
| M3 mapping §8 `pcb:proton-refused` contradicts SPEC §8.1 step 9 | docs: aligned |
| M4 If-Match lists and weak tags | fixed T5: a list, strong comparison; a blank header is 412 (round) |
| M5 an alias is kept when a create never reached Proton | parked |
| M6 `AttendeeEdits` used only for a debug line | fixed T5: removed |

### ux

| Finding | Outcome |
|---|---|
| I1 "a sync is already running" while serve holds the lock; lock taken before the login check | fixed T6: login first; a daemon holder gives exit 0 `pcb serve is running and already syncs` (`daemon_syncs` in JSON) |
| I2 doctor exits 20 on a fresh install; a bad config gives 20 | fixed T6: needs-login while the daemon waits is a warning (exit 0); a config error is 10; 20 only when every error is transient |
| I3 key and state errors are 10 in serve, 20 elsewhere | fixed T6: 10 everywhere (`pcb status` reports and exits 0 on a key fault, by choice) |
| M1 wrong key size gives no cause in `client add` | fixed T6 |
| M2 doctor cannot see sealed copies the key no longer opens | fixed T6: a warning per client, `pcb client rotate NAME` |
| M3 Windows and macOS behaviour; supported platforms never stated | docs: README and SPEC §13 "Platforms" |
| M4 Go error chains leak | fixed T6 (also the CLI's warning logger) |
| M5 hint format, "did you mean", `--version`, rotate prints a Hint on success | accepted: the hint format is consistent enough (one runnable step after the cause); a typo suggestion and `--version` are polish, `pcb version` exists |
| M6 empty lists print a header only; Ctrl-D leaves "Error:" on the prompt line | fixed T6 |
| M7 `login --help` defaults; `--json` advertised where rejected | fixed T6 (the long `Short` texts and `--non-interactive` on commands that never prompt stay) |
| M8 the JSON envelope can carry data and error | docs: SPEC §13 |
| M9 unit paths with a space or a non-ASCII letter fail under `--plan` with exit 20 | fixed T6: exit 10 with the reason (the restriction stays: systemd would split the path) |
| M10 README and SPEC drift | docs: README opening and Status, `client show` default, `pcb status` line order, the CI wording |
| M11 dead condition in `printPass` | fixed T6 |

### Accepted at the fix rounds (not parked)

Reviewer Minors ruled accepted, with no code change: an in-process Sync holder that skips the lock (embedder only); a chmod EPERM on another user's `pcb.db` (the root guard covers it); the early 412 logging `calendar=""` (the adapter decides it before any service call); the service's own diagnostic warnings next to the new refusal line (HUMAN-CHECKS uses them as stop conditions); a 9001 on a read staying exit 20 (Proton asks in the browser; serve keeps retrying); the state-I4 fix limited to a stored `ready` login; zoned IPv6 untested; multiget hrefs echoed within 1 MiB; `pcb status` exiting 0 on a key fault (it reports; the container's health check uses `--daemon`); the CLI warning logger writing to stderr under `--json`; the `pflag` direct dependency; the missing-Type reminder covered at the iCalendar level only (the fake's Type is an integer).

## What held up

- **proton**: one transport with an explicit retry table and a test per rule; one refresh per request, concurrent 401s refresh once, a rotated refresh token sealed before any use; signature results used everywhere, unsigned or unverifiable fails closed, SRP server proof compared in constant time; OTP bounded and never quoting its input; the fake reuses the real server and real crypto.
- **state**: the secrets write order and the generation mirror (a crash between the two is tolerated, a rollback is detected, the AAD stops a downgrade); an acyclic lock order with `state.lock` as the leaf; login failure paths undo their partial writes; refresh tokens rotated under the secrets lock; the key file created by hard link, mode checked fail closed; the cache writes one transaction per calendar with its cursor.
- **surface**: the unauthenticated surface is OPTIONS only, everything else 401 before routing; a constant-time check over every client; a bounded limiter (IPv6 /64, no `X-Forwarded-For`, FIFO 4096) that let exactly five of 300 concurrent bad requests through; XML bounds (DOCTYPE refused, depth 32, 64 properties, 1 000 hrefs); path handling; TLS 1.2 minimum with a stable fingerprint; strict config; the control socket checked in both directions with `SO_PEERCRED`; the RFC 4791 and 6578 behaviours Thunderbird relies on; 503 and never a 4xx while not ready.
- **read**: folding by octets, unfolding, no double escaping; the sync-token design (epoch, sequence, render generation, per-calendar floor) with named tests; a pure dependency-free iCalendar codec with four round-trip fuzzers; an explicit failure policy (a placeholder for an undecryptable part, a personal part skipped and counted, an unverified one marked but served); listings and ETags without decrypting.
- **write**: single-send discipline and a read-back by decrypted plaintext (a 2xx only when the rows show the write done); no retry after a call may have reached Proton (one resend after a 401 answered before processing); per-calendar locks and budgets (20, 60 and 30 s); the key-packet pinning trust model matches SPEC §5.3; 4xx only for 412, 409, 403, 404, 413 and 415 as E08 says; refusals carry a code and a fixed reason.
- **ux**: one `Error:` and `Hint:` shape and a single JSON envelope with the same four keys everywhere; `--plan` of install and uninstall is faithful and changes nothing; secrets never in argv or logs, no echo at prompts, the terminal restored on Ctrl-C; serve's intro and `pcb status` give what Thunderbird needs; `cmd/pcb-lab` is a separate binary that never ships (not built by `make`, not in the image, no import from `./cmd/pcb`).

## Not covered by the audit

Everything that needs a live Proton session (the 422 codes at login, 429 headers, key-packet behaviour, UID column escaping, whether Proton keeps an omitted personal part, Thunderbird's real handling of 403 and 409 bodies), the real `install`, `uninstall` and `service` apply paths, the container's behaviour, macOS runtime, and the Secret Service, `systemd-creds` and TPM2 sealers (not built). These stay in `spec/OPEN.md` and `spec/HUMAN-CHECKS.md`.
