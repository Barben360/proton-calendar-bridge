# Open questions and pending items

State at the close of M4 (2026-11-12).

## Answered

- Client identity, deployment, roadmap scope, login modes and the test account: decisions 0007 to 0010 (2026-11-10).
- How the agent reads the test-account credentials: Proton Pass references in `scripts/lab.env.example` and `scripts/seed.env.example`, run through `pass-cli run --env-file` (ruling R9); no secret in argv or files. Only exercised so far up to the first login's CAPTCHA answer.
- Can a non-official identity reach the calendar API: yes for `Other`, on the fork's evidence from the author's account (gate E03 passed, R13; decision 0011, provisional).
- What can seal secrets on blackwater and morpheus: E15, SPEC §7.3.
- What Thunderbird sends and requires, and whether it needs VTIMEZONE (no): E08 from source (SPEC §9, E11); built and contract-tested in M3 (E08, "What the bridge does (M3)").
- Library for the CalDAV adapter: our own server, go-webdav as a test client only (E17, M3-R5).
- Model: iCalendar-native for v1, decision 0012 accepted by the author on 2026-11-11 (JSCalendar deferred to the first non-CalDAV adapter).
- Does decryption need a sealed decrypted cache (E09): no, offline 0.2 to 0.4 ms per event; the live cold-start figure is still pending.
- How CalDAV writes reach Proton, and what is refused: built in M4 (SPEC §8.1, §8.4, §9; `spec/mapping.md`), contract-tested against the fake Proton; the live confirmations are listed below.

## Still open for the author

1. Approval of SPEC.md draft v2 (§5.3 to §5.6, §7.2, §8, §9, §13, §15 and §18 now describe M4 as built; the write-side trust model and its accepted residual risks are in §5.3).
2. Confirm decision 0011 (`Other` by default); it becomes final after the E03 live run.
3. Solve the test account's CAPTCHA (`spec/HUMAN-CHECKS.md`, step 1): the only author action that unblocks the live runs.
4. Process incident during M4 (Task 4, third re-review): a reviewer agent fetched ProtonMail/WebClients `constants.ts` (GPL) to confirm the routing table. Nothing from it was copied into the repository (checked); the hard rule "never read or fetch WebClients" is now in the reviewer and re-reviewer instructions.
5. The M4 live session needs one author sitting in Thunderbird and the web app (`spec/HUMAN-CHECKS.md` step 6.3), after the M1 and M2 live sessions.

## Still open, run by the agent (deferred live runs, `spec/HUMAN-CHECKS.md` steps 2 to 4)

- E03: scope list for `Other`, the three other identity candidates.
- E02: is a solved CAPTCHA token valid in a new process, and after how long (row 1 pending).
- E01: access-token TTL, refresh rotation, old-token refusal (immediate), session lifetime under 6-hourly refresh (14-day timer).
- Synthetic data set: not yet applied to the test account (`spec/experiments/DATASET.md`).
- SPEC §5.2 markers on E01 and E02 stay `[assumed]` until then.

## Still open, later

- The live Thunderbird and M4 session (`spec/HUMAN-CHECKS.md` step 6, test account only): E08 and E11 live confirmation, E09 live half, and the M4 checks: E10 (signatures, R17), E12 (recurrence forms, DTSTART as first occurrence, DAILY BYDAY, BYDAY=2TU), E13 (unknown properties), E14 (the Write bit is 8), research L.4 #1 to #12, the UID-escaping question, the ListByUID row equal to the feed row, `?UID=` matching exactly, X-PM-TOKEN and attendee statuses reused on a new exception row (Task 7a M6), whether Proton ever rewrites a row's key packets (R30, R35), whether it echoes part `Data` verbatim (R32), whether an unbumped text edit is dropped (R4), the personal part on update (L.4 #10), 429 behaviour if one is met.
- TPM2 sealing is untested everywhere (morpheus lacks libtss2; installing was out of scope), as is system-scope `systemd-creds` on blackwater (sudo is interactive, ruling R18), any run inside a systemd service, and any container (E15).
- M2 live session (E04, E05, E07): `pcb-lab probe feed` is built; the experiments are source-based until the run (`spec/HUMAN-CHECKS.md`, "M2 live session"). Markers on the feed, cursors, ETag stay `[assumed]` until then.
- E06, E16 are not started (SPEC §17). E12 to E14 are written up, live pending (step 6).
- Fresh session keys on every update (rulings M4-R17, M4-R19): once live check L.4 #3 (research L.4) shows Proton accepts new key packets on an update, mint new keys and send their packets on every update instead of reusing the row's. That removes the reuse and the residual risks 1 to 5 of SPEC §5.3; pinning (and residuals 6 to 8) can then go. Until then key-packet pinning is the primary defence and the signature proof (`ical.ProvesKey`) the secondary one. L.4 #3 is not reachable with M4's tools (pcb never sends a key packet on an update): it needs a `pcb-lab` probe that does, run on the test account only.
- Not reachable through pcb, which refuses or normalises first (record "not tested" in the live session, or build a lab probe): whether Proton itself enforces the RRULE limits (L.4 #6); whether it drops an unbumped **time** edit (pcb always bumps one, R4); a second master with an existing UID (L.4 #5; pcb refreshes the UID first); STATUS:TENTATIVE's 2000, floating times, non-IANA TZIDs, DURATION (L.4 #8); whether `Notifications` absent on an update keeps the row's (L.4 #10; pcb sends it absent only when the row has none).

## Parked by M3

Closed by M4:
- Writability from the member permissions (M3-R15): the write gate (M4-R1, M4-R39, M4-R40; SPEC §5.3, §9).
- Writes to a placeholder object: refused 403 `pcb:placeholder`, from the service's own state, never the `X-PCB-PLACEHOLDER` property (`spec/mapping.md` §8).
- R17, the member passphrase signature: verified, and it gates writes (live confirmation: E10).

Later (with E10 and a public-key lookup):
- Verify the parts authored by addresses that are not ours (invitations, other members of a shared calendar): until then they never mark an event `X-PCB-INTEGRITY:unverified`, so tampering with them goes unnoticed (M3-R36).

For M5 (daemon and ops):
- `pcb serve` waits for a login instead of refusing to start (M3-R28); restart after a stop error.
- Plain HTTP inside a container behind a reverse proxy, as an explicit opt-in (M3-R28): today a non-loopback bind needs TLS.
- An unbounded wait after the drain, and a second Ctrl-C to force exit (Task 7 review, minor 10).
- Placeholder reason categories for `pcb doctor` (secret-free; M3-R4, Task 4).
- The render counters (placeholders, unverified, duplicate masters, keys errors) in `pcb status` and `pcb doctor` through the local API socket: until then only `pcb serve`'s log has them (M3-R34).
- Address-key rotation is seen only on restart, login or relogin, since the 10-minute refresh reads calendar bootstraps only (M3-R10, Task 4 fix round 1): revisit.
- `clients.json` removal at uninstall (M3-R17).
- Integrity against a malicious server (M3 final re-review): bind each part's `Author` to the key that signed it, and refuse or mark unsigned cleartext attendees/personal parts, once E10 says which parts Proton signs with which key (SPEC §5.3). The write side already refuses any unsigned or unverified part of ours (M4-R18, M4-R19).
- Final audit: `TestServeReadsOnItsOwnHandle` does not prove reads use Serve's handle (stays green with that branch disabled); a deleted calendar answers 503 instead of 404 while another calendar is pending or the account has none.

For the final audit (parked minors):
- Duplicate-count writes from unordered snapshots (diagnostic only, Task 4).
- Over-long comment in `serve.go` `refreshSettings` (Task 4).
- Limiter: an expire, re-fail, evict test; a "no touch after Close" test inside `touch()` (Task 5).
- A malformed token on a pending calendar answers 403, not 503 (race only); a stale token on a pending calendar is untested; a home-set Depth 0 service failure logs a warning per request (Task 6).
- M3-R18: on platforms without filelock, `clients.json` writers take an in-process mutex only, so a concurrent CLI and `pcb serve` write can drop a `LastUsed` update (documented; Task 5).
- Decision (Task 7): `pcb doctor` keeps an Error, not a Warning, for a too-wide `tls/` directory or key: a readable key may already have been exposed, and the repair at start cannot undo that.
- `go-vcard` moved to `v0.0.0-20230815062825-8fda7d206ec9` through go-webdav (test-only import, MVS): only README and `example_test.go` differ (E17); re-check when go-webdav changes.

## Parked by M4

For the live session (step 6), each a code change test first if Proton differs:
- UID escaping: if Proton's UID column holds `pcb-m4-a,b` for `UID:pcb-m4-a\,b`, compare unescaped UIDs in the write path (`ical.ProvesKey`, the planner's row check, `ListByUID`); until then such an event is not confirmed (503) and not editable through pcb (step 6.2 W10).
- R4: if Proton drops an unbumped text edit, bump SEQUENCE on every edit (every title edit would otherwise answer 503).
- R5/R12: if the web app shows a rewritten RRULE differently, refuse that form instead (E12).
- Personal parts (M4-R29): never re-sent on update; what Proton does to one is L.4 #10.

For M5 (daemon and ops):
- `pcb status` and `pcb doctor` must name a calendar whose write gate stays undecided (no usable write key: two calendar keys flagged primary, no unlocked primary address key, or no passphrase signature served) and why: every write to it is 503 and Thunderbird retries for ever (M4-R39, Task 8).
- A log level for `pcb serve`: what a write normalised (`write normalised`, dropped VALARMs, TENTATIVE, attendee edits ignored…) is logged at debug, which the default level never prints.
- The write gate's last decision is kept per process (M4-R40): after a restart, privileges are not advertised until the first bootstrap succeeds. Accepted; revisit with the daemon's state.

For the final audit (parked minors):
- The calendar lock is held across a GET's backoff (a 429's `Retry-After` up to 60 s, 5xx retries) during a write's refresh and read-back and the syncer's pass (`calLocks`, Task 7b): a slow Proton holds the other side waiting; releasing it there would let rows be stored between a refresh and a plan.
- Pin pruning (M4-R35): pins of IDs tombstoned long ago are never removed until a new epoch.
- Aliases of deleted UIDs are never pruned (M4-R22, Task 6 M7): growth bounded by the events ever created at a client's own name.
- A read-only cache handle opened on a schema-1 file keeps "no aliases" after a writer upgrades the file (Task 6 M5): only long-lived read-only handles.
- A Thunderbird faked master (`X-MOZ-FAKED-MASTER:1`) that carries an RRULE is dropped, not refused; `INTERVAL=01` and duplicate days in a DAILY BYDAY are rewritten as sent (Task 3 re-review).
- `canonParams` (line identity in `ical.Patch`) can make two differently quoted parameter lists look equal (`X-FOO;A="B;C=d":v` ≡ `X-FOO;A=B;C=d:v`); contrived, Thunderbird does not write it (Task 3 M3).
- Memory hygiene: the address-key copy and the passphrase clone made for verification are not zeroed (Task 2 M6); zeroing of minted session keys is untested (Task 4 M5).
- `TestSecond401GivesNoThirdRequest` fails by hanging, not by a clean assertion, when its guard is sabotaged: give it a bounded client timeout (Task 5).
- The fake parses an unfolded UID line over 75 octets differently from a folded one: a row seeded with one gets 2001 on update (403 `pcb:proton-refused`), the folded form 204 (Task 8 re-review). Matters only if real stored rows carry unfolded long lines (look for one in the live data).
- The fake's `nullLists` check is case-sensitive while the typed decode is not (errs toward refusal; Task 1).
- A `ListByUID` answer holding rows of another UID makes the write fail with 503 and the warning "the event cache cannot be opened", which names the wrong cause (found while writing the M4 live checklist).
