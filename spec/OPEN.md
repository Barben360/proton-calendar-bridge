# Open questions and pending items

State at the close of M5 (2026-11-12), updated after the initial-version audit fixes (2026-11-12, `spec/audits/2026-11-12-initial-version.md`).

## Answered

- Client identity, deployment, roadmap scope, login modes and the test account: decisions 0007 to 0010 (2026-11-10).
- How the agent reads the test-account credentials: password-manager references in local copies of `scripts/lab.env.example` and `scripts/seed.env.example` (`~/.config/pcb-live/*.env`, never in the repository), run through `pass-cli run --env-file` (ruling R9); no secret in argv or files. Only exercised so far up to the first login's CAPTCHA answer.
- Can a non-official identity reach the calendar API: yes for `Other`, on the fork's evidence from the author's account (gate E03 passed, R13; decision 0011, provisional).
- What can seal secrets on a desktop and on a headless server: E15, SPEC §7.3.
- What Thunderbird sends and requires, and whether it needs VTIMEZONE (no): E08 from source (SPEC §9, E11); built and contract-tested in M3 (E08, "What the bridge does (M3)").
- Library for the CalDAV adapter: our own server, go-webdav as a test client only (E17, M3-R5).
- Model: iCalendar-native for v1, decision 0012 accepted by the author on 2026-11-11 (JSCalendar deferred to the first non-CalDAV adapter).
- Does decryption need a sealed decrypted cache (E09): no, offline 0.2 to 0.4 ms per event; the live cold-start figure is still pending.
- Where pcb runs and how it is looked after: built in M5 (SPEC §13, §15): `pcb install`, `uninstall` and `service`, the control socket behind `pcb status` and `pcb doctor`, `pcb serve --wait-for-login`, the container image; smoke-tested in CI, the live run is `spec/HUMAN-CHECKS.md` step 7.
- Can CalDAV credentials be looked up again: yes, decision 0013 (2026-11-12), `pcb client show`.
- How CalDAV writes reach Proton, and what is refused: built in M4 (SPEC §8.1, §8.4, §9; `spec/mapping.md`), contract-tested against the fake Proton; the live confirmations are listed below.

## Still open for the author

1. Approval of SPEC.md draft v2 (§5.3 to §5.6, §7.2, §8, §9, §13, §15 and §18 now describe M4 as built; the write-side trust model and its accepted residual risks are in §5.3).
2. Confirm decision 0011 (`Other` by default); it becomes final after the E03 live run.
3. Solve the test account's CAPTCHA (`spec/HUMAN-CHECKS.md`, step 1): the only author action that unblocks the live runs.
4. Process incident during M4 (Task 4, third re-review): a reviewer agent fetched ProtonMail/WebClients `constants.ts` (GPL) to confirm the routing table. Nothing from it was copied into the repository (checked); the hard rule "never read or fetch WebClients" is now in the reviewer and re-reviewer instructions.
5. The M4 live session needs one author sitting in Thunderbird and the web app (`spec/HUMAN-CHECKS.md` step 6.3), after the M1 and M2 live sessions.
6. The M5 live session (`spec/HUMAN-CHECKS.md` step 7): a fresh Debian host or VM, the container, a revoked session, an idle Thunderbird, `uninstall --delete-data` (the author revokes and reads the web app's session list, and sits in Thunderbird for 7.4 and 7.7); and the GHCR package's visibility (7.8: done, the author confirmed Private on 2026-11-12; the unauthenticated and the logged-in `docker pull` are not recorded yet).

## Still open, run by the agent (deferred live runs, `spec/HUMAN-CHECKS.md` steps 2 to 4)

- E03: scope list for `Other`, the three other identity candidates.
- E02: is a solved CAPTCHA token valid in a new process, and after how long (row 1 pending).
- E01: access-token TTL, refresh rotation, old-token refusal (immediate), session lifetime under 6-hourly refresh (14-day timer).
- Synthetic data set: not yet applied to the test account (`spec/experiments/DATASET.md`).
- SPEC §5.2 markers on E01 and E02 stay `[assumed]` until then.

## Still open, later

- The live Thunderbird and M4 session (`spec/HUMAN-CHECKS.md` step 6, test account only): E08 and E11 live confirmation, E09 live half, and the M4 checks: E10 (signatures, R17), E12 (recurrence forms, DTSTART as first occurrence, DAILY BYDAY, BYDAY=2TU), E13 (unknown properties), E14 (the Write bit is 8), research L.4 #1 to #12 (#3, #5, the server-side halves of #6 and #8, and #10 by the 6.7 write matrix), the UID-escaping question, the ListByUID row equal to the feed row, `?UID=` matching exactly, X-PM-TOKEN and attendee statuses reused on a new exception row (Task 7a M6), whether Proton ever rewrites a row's key packets (R30, R35), whether it echoes part `Data` verbatim (R32), whether an unbumped text edit is dropped (R4), the personal part on update (L.4 #10), 429 behaviour if one is met.
- N2: master-first order in a batch; the live bisection's one-PUT case (master SEQUENCE:0 + exception) answered 204; confirm with the D7 rerun (`spec/HUMAN-CHECKS.md` 6.3 D7, with the exception update's refusal, assumed from the create's).
- A row stored without `Notifications` whose parts hold a VALARM cannot be updated through pcb against the fake: `ical.Patch` keeps a stored nested component (the VALARM) in its part verbatim, and the fake refuses any nested component in a part (2001). Proton's answer is unknown; no live row has that shape (Proton's writers put no VALARM in parts; the live cache had no row without `Notifications`). The planner sends a changed alarm of such a row as an explicit list (`TestUnchangedDefaultAlarmsKeepNull`, "a row without Notifications whose VALARM is changed"); the end-to-end case waits for a ruling: the fake accepting nested components, or pcb dropping a stored VALARM when it sends an explicit list.
- TPM2 sealing is untested everywhere (the E15 server lacks libtss2; installing was out of scope), as is system-scope `systemd-creds` on the E15 desktop (needs root; not run, ruling R18), and a sealed session inside a systemd service or a container (M5's smoke tests never log in; step 7; E15). Secret Service and `systemd-creds` sealing are not built: the key file is the only sealer.
- M2 live session (E04, E05, E07): `pcb-lab probe feed` is built; the experiments are source-based until the run (`spec/HUMAN-CHECKS.md`, "M2 live session"). Markers on the feed, cursors, ETag stay `[assumed]` until then.
- E06, E16 are not started (SPEC §17). E12 to E14 are written up, live pending (step 6).
- Fresh session keys on every update (rulings M4-R17, M4-R19): live check L.4 #3 answered that Proton refuses new key packets on an update (2000 "SharedKeyPacket is not allowed", the row keeps its packet; `[verified: live 2026-11-13]`, `spec/experiments/raw/L4/write-matrix.json`), so pinning stays; only another way to rotate a row's keys (to find) would allow minting new keys and sending their packets on every update instead of reusing the row's. That removes the reuse and the residual risks 1 to 5 of SPEC §5.3; pinning (and residuals 6 to 8) can then go. Until then key-packet pinning is the primary defence and the signature proof (`ical.ProvesKey`) the secondary one. pcb never sends a key packet on an update: `pcb-lab probe write-matrix` does (case `key-packets-on-update`), on the test account only, at `spec/HUMAN-CHECKS.md` step 6.7.
- Accepted by ruling M5-R16 (final review), documented: a login that completes while no CalDAV client exists creates `default` (decision 0013's rule), also after a person removed every client to cut access (a new random password, shown only by `pcb client show`); gate it on "never created before" (a flag in `state.json`) if that surprises someone.
- The CalDAV limiter counts wrong passwords per source IP (ruling M5-R16). Behind Docker's port publishing the DNAT path keeps the client's IP; with the userland proxy (rootless Docker, or traffic Docker's `userland-proxy` relays) every client shares one IP, so five typos from one device lock all of them for a minute. Revisit with a per-username counter or a trusted-proxy setting.
- Measured by the 6.7 write matrix (`pcb-lab probe write-matrix`, test account, `[verified: live 2026-11-13]`, raw `spec/experiments/raw/L4/write-matrix.json`; cases pcb refuses or normalises first): Proton enforces RRULE limits itself, 2001 on `FREQ=WEEKLY;COUNT=50`, `UNTIL` in 2038, `FREQ=HOURLY`, `FREQ=DAILY;INTERVAL=1000`, while `BYSETPOS=5` is stored as sent (pcb's refusal is conservative; L.4 #6); an unbumped **time** edit is refused, 2000 "Cannot update time data without also incrementing Sequence" (pcb always bumps one, R4); a second master with an existing UID is refused, 2500 (L.4 #5; pcb refreshes the UID first); STATUS:TENTATIVE 2000, a floating time 2001, a non-IANA TZID 2001, DURATION without DTEND 2011 (L.4 #8); `Notifications` absent on an update resets the row to null, so pcb never sends it absent and sends a row's reminders as a list (L.4 #10, Fix 6).
- Still open from the 6.7 matrix: what Proton does to a personal part an update leaves out (L.4 #10): the create carrying `PersonalEventContent` (a field name the research could not verify) answered 1000 but stored no personal part, so the update was not sent; pcb keeps refusing to update a row that holds one (`pcb:personal-part`, audit fix write-I3). INTERVAL bounds of the other FREQs are not measured (pcb's: `internal/ical/validate.go`).

## Parked by M3

Closed by M4:
- Writability from the member permissions (M3-R15): the write gate (M4-R1, M4-R39, M4-R40; SPEC §5.3, §9).
- Writes to a placeholder object: refused 403 `pcb:placeholder`, from the service's own state, never the `X-PCB-PLACEHOLDER` property (`spec/mapping.md` §8).
- R17, the member passphrase signature: verified, and it gates writes (live confirmation: E10).

Later (with E10 and a public-key lookup):
- Verify the parts authored by addresses that are not ours (invitations, other members of a shared calendar): until then they never mark an event `X-PCB-INTEGRITY:unverified`, so tampering with them goes unnoticed (M3-R36).

Closed by M5 (the items that were "For M5" here):
- `pcb serve` waits for a login (M3-R28): `--wait-for-login`, SPEC §8.1.
- An unbounded wait after the drain, and a second Ctrl-C: a second signal exits 20, `TimeoutStopSec=30s` (M5-P22).
- The render counters in `pcb status` and `pcb doctor` through the control socket (M3-R34), SPEC §15.
- `clients.json` removal at uninstall (M3-R17): `pcb uninstall --delete-data` deletes the state directory.

Still open from M3, parked past M5 (each needs more than the daemon):
- Plain HTTP inside a container behind a reverse proxy, as an explicit opt-in (M3-R28): not built; the image serves self-signed TLS on 1080, which a proxy can use as an https upstream. Revisit when a proxy setup or a UI needs it.
- Placeholder reason categories for `pcb doctor` (secret-free; M3-R4, Task 4): out of scope in M5; they need E10 or the final audit.
- Address-key rotation is seen only on restart, login or relogin, since the 10-minute refresh reads calendar bootstraps only (M3-R10, Task 4 fix round 1): out of scope in M5; revisit with E10.
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
- UID escaping: closed 2026-11-13. Proton stores `pcb-m4-a,b` for `UID:pcb-m4-a\,b` (live, W10); pcb identifies an event by its unescaped UID throughout the write path (SPEC §5.3). What stays of write-I2: a row whose column holds an escaped form of the body's UID (another client's data; Proton never writes one) is 503 at its own name, never a 409 naming itself, even when the body repeats the column's text: pcb names the event by the body's unescaped value, which the read-back and key proof could not find under that column. Lift it only if such a column is ever seen live. Assumed, not measured: Proton unescapes `\;`, `\\` and `\n` as it does `\,`; pcb refuses (403 `bad-data`) a UID with an undefined escape or a control character rather than guess (SPEC §5.3).
- R4: if Proton drops an unbumped text edit, bump SEQUENCE on every edit (every title edit would otherwise answer 503).
- R5/R12: if the web app shows a rewritten RRULE differently, refuse that form instead (E12).
- Personal parts (M4-R29): never re-sent on update; what Proton does to one is L.4 #10. Since the audit fix write-I3 an update of a row that holds one is refused (403 `pcb:personal-part`) until L.4 #10 is observed (`spec/HUMAN-CHECKS.md` 6.7, case `personal-part-left-out`); then lift `RefusePersonalPart` (`internal/service/writeplan.go`) or re-send the part.

Closed by M5 (the items that were "For M5" here):
- `pcb status` and `pcb doctor` name a calendar whose write gate stays undecided and why (M4-R39): alert `write-gate-undecided`.
- `pcb status` says when Proton asks for a human verification (M4-R44): alert `human-verification`.
- A log level for `pcb serve`: `[log] level` and `--log-level`.
- The write gate's last decision stays per process (M4-R40): accepted, not persisted (M5-P13); a stored verdict could advertise a revoked Write bit. After a restart, privileges are not advertised until the first bootstrap succeeds.

For the final audit (parked minors):
- The calendar lock is held across a GET's backoff (a 429's `Retry-After` up to 60 s, 5xx retries) during a write's refresh and read-back and the syncer's pass (`calLocks`, Task 7b): a slow Proton holds the other side waiting; releasing it there would let rows be stored between a refresh and a plan. Since M4-R42 a write waiting behind it gives up at its 20 s pre-send budget (503, nothing sent), and a write's answer outlives the listener's 60 s write timeout; the lock itself is unchanged.
- Pin pruning (M4-R35): pins of IDs tombstoned long ago are never removed until a new epoch.
- Aliases of deleted UIDs are never pruned (M4-R22, Task 6 M7): growth bounded by the events ever created at a client's own name.
- A read-only cache handle opened on a schema-1 file keeps "no aliases" after a writer upgrades the file (Task 6 M5): only long-lived read-only handles.
- A Thunderbird faked master (`X-MOZ-FAKED-MASTER:1`) that carries an RRULE is dropped, not refused; `INTERVAL=01` and duplicate days in a DAILY BYDAY are rewritten as sent (Task 3 re-review).
- `canonParams` (line identity in `ical.Patch`) can make two differently quoted parameter lists look equal (`X-FOO;A="B;C=d":v` ≡ `X-FOO;A=B;C=d:v`); contrived, Thunderbird does not write it (Task 3 M3).
- Memory hygiene: the address-key copy and the passphrase clone made for verification are not zeroed (Task 2 M6); zeroing of minted session keys is untested (Task 4 M5).
- The fake parses an unfolded UID line over 75 octets differently from a folded one: a row seeded with one gets 2001 on update (403 `pcb:proton-refused`), the folded form 204 (Task 8 re-review). Matters only if real stored rows carry unfolded long lines (look for one in the live data).
- The fake's `nullLists` check is case-sensitive while the typed decode is not (errs toward refusal; Task 1).
- Timing-sensitive tests (closed by the initial-version audit, commit "Make three timing-sensitive tests deterministic"): `TestBeginKeepsRetryAfter` (its window no longer absorbs the call's own duration; the 300 s arithmetic is pinned with a fixed clock) and `TestTimeoutAfterSendIsAmbiguous` (a response-header timeout, so the timeout is after the send by construction) were made deterministic; the keys tests' client timeout was raised for the SRP check under -race. Still open: `TestScopeUnlockRejectsBadServerProof` / `TestScopeUnlockEmptyServerProofFailsClosed` keep a 60 s SRP timeout; widen it if CI ever flakes.
- The write budget's last check (`sendable`, after the plan) has no test of the budget or a caller hang-up ending between the plan and the send (final re-review N1).
- `extendDeadline` runs before the CalDAV adapter's calendar lookup, so a cold-key bootstrap with 429 backoff eats into the 40 s margin of the 150 s write deadline; move it right before `PutObject`/`DeleteObject` (final re-review N2).
- The `refreshSettings` comment says a gate change changes ETags and tokens, which is untrue between undecided and closed (final re-review N3).

## Parked by M5

For the final audit (parked minors; the code is read-only until then):
- Closed by the initial-version audit: `pcb doctor` now warns per client whose sealed copy the key no longer opens (a lost key beside `clients.json` only, another key): `caldav client NAME: password cannot be shown`, fix `pcb client rotate NAME` (SPEC §13) [verified: `TestSealedClientsCheckWarnsPerClientTheKeyNoLongerOpens`]. It stays silent when the key file cannot be read or is missing beside `secrets.sealed`: the key file and secrets checks report that. (Was: `pcb doctor` did not check the sealed client copies; Task 7b, M5-R14 (3).)
- The stat-then-seal window: `pcb client add` and `rotate` refuse when the key file is absent while `secrets.sealed` exists, but a key deleted between that check and the seal is still minted anew (it needs a concurrent manual deletion; Task 7b fix round 1).
- `pcb uninstall --delete-data` on a symlinked state or config directory removes only the link: plan the link removal and a `keep` step that prints the `rm -r` for the target (Task 7 review).
- Non-ASCII home directories are refused by `pcb install` (a path with a character outside `[A-Za-z0-9/._-+@,:=~]` would be split or expanded by systemd): documented, kept (Task 7, M5 Minor 10); since the audit it is exit 10 with the cause, also under `--plan` (`PCB_CONFIG_DIR`/`PCB_STATE_DIR` are the way around, the binary path has none).
- `Local.Events` returns `ErrWaitingForLogin` in process while a `--wait-for-login` serve waits: only the CLI's in-process read path in the same process sees it (Task 4 re-review).
- `uninstall --system --delete-data` runs the logout as root on `pcb`-owned files (writes are atomic, no clobber found; Task 7).
- `pcb doctor` could flag a generation mirror at its bound (rollback detection off for that install; Task 6).
- `sunpath_other.go`'s comment names only macOS and the BSDs though it also covers Solaris, illumos and AIX, where 103 is cautious (Task 3 re-review).
- The user scope does not restart on a rebuilt binary at the same path: `install` prints a restart hint instead (Task 7).

## Parked by the initial-version audit (2026-11-12)

Findings of the six read-only auditors (main at 557fe84) that were ruled "park", each with its reason; the fixed and accepted ones are in `spec/audits/2026-11-12-initial-version.md`. Finding IDs are `<area>-<ID>` from the audit reports.

Parked by the plan:
- proton-M1: a token refresh holds `c.mu` for up to 30 s and `Tokens()` ignores its context, so every request waits behind a slow refresh on an uncancellable mutex. Bounded (the refresh has its own 30 s timeout and runs once at a time); a context-aware wait changes the transport's locking, which wants its own review.
- proton-M5: the passphrase signature is verified at one time and the cards at time 0. Crypto semantics of an expired or rotated key; needs a live check before any change, so a wrong "fix" cannot make pcb refuse good data.
- state-M tombstones are never pruned: the sync tokens' monotone sequence relies on it (a pruned tombstone would hide a deletion from an old token). Growth is bounded by the deletions ever seen, and `Changes` scans them by sequence; prune only together with a per-calendar floor, with the pins and aliases items above.
- state-M a login over a rolled-back or key-unreadable store returns the store error first, forcing an extra `pcb logout` although the login replaces the store. A person has an actionable hint; the daemon's `--wait-for-login` and the CLI could skip the step, which is a UX change for a rare state.
- surface-M2: the self-signed certificate lists every interface IP as a SAN. LAN peers know the IP anyway, and `hostnames` exists to narrow the names clients use.
- surface-M3: the certificate is not regenerated when an interface IP appears or changes (a name clients use is covered by `hostnames`); a restart after `hostnames` is edited renews it.
- surface-M5: no certificate renewal while running; the `tls-certificate-expiring` alert (30 days) says to restart.
- surface-M6: a local user can lock out the loopback Thunderbird by failing five passwords a minute (the limiter is keyed by source IP). Same shape as the Docker userland-proxy item (see "Still open, later"); revisit both with a trusted-proxy setting or a per-client counter.
- surface-M9 (second half): no cap on concurrent connections or concurrent PUT bodies (authenticated only; the PROPFIND and REPORT cap went to 1 MiB). Parked with the connection limit: `http.Server` has none, and a listener-level cap needs a design (per-IP versus global).
- read-M5: every single GET re-reads `state.json` and counts events per calendar (O(N) per read, O(N^2) for a client that GETs every object of a 10 000-event calendar). Measure first; the fix is a `Calendar(id)` lookup without the count and a cached account ID.
- read-M7: a full listing holds every row's `Raw` in memory, up to three times when the listing is unstable (about 100 MB for 20 000 rows at 5 KB, once per 24 h); stream pages into the transaction if calendars of that size appear.
- read-M8: the first-wins ATTENDEE dedup can drop the PARTSTAT of the token-carrying copy when the same address sits in two parts; no known Proton shape triggers it. Prefer the line that carries a token if one is seen live.
- write-M5: an alias stays recorded when a create never reached Proton (it is pruned only with the calendar or a new epoch); growth is bounded by the creates a client attempted. Same family as "Aliases of deleted UIDs are never pruned" above.

Parked by the rulings of the fix rounds:
- A5 M7 (state, pre-existing): `pcb logout` of a store with no key file or a corrupt `secrets.sealed` wipes it without a confirmation, because there is nothing to revoke with and the wipe is the way out of that state. A typed confirmation there would block the recovery; revisit if someone loses a session by it.
- A7 (read): the render counters' prune uses a snapshot taken before the render, so a row added and rendered by a concurrent request after the snapshot loses its mark until it is rendered again (a cached object is not re-marked). An undercount of a diagnostic number only, the same class as `keepDuplicates`.
- A9 M3 (write): the refusal log's `href` is the client-chosen object name (the escaped request path), quoted by slog so it cannot inject a line; authenticated clients only. Parked: say so in the log docs if someone logs it elsewhere.
- A9 M5 (write): a 413 or 415 answer on a PUT is not logged by `refused a CalDAV write` (that line covers each 403, 409 and 412, as documented).
- write-I2, the full fix, behind W10: see "Parked by M4", UID escaping. The stop-gap (a conflict check that compares unescaped UIDs, 503 at the object's own name) is built.
- surface's connection cap: see surface-M9 above.

Closed by the audit fixes (earlier items resolved):
- The doctor's sealed client copies: see "Parked by M5", first item, now closed.
- A state this pcb cannot read is no longer a dead end (a newer `secrets.sealed` or `state.json` says `upgrade pcb`; `pcb logout --yes` is the confirmed local wipe), and a crash can no longer leave a half-migrated cache or roll the change counter back (`synchronous=FULL`).
- The stat-then-seal window for a key deleted between the check and the seal (see "Parked by M5") is unchanged and stays accepted.
