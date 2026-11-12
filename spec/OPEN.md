# Open questions and pending items

State at the close of M3 (2026-11-11).

## Answered

- Client identity, deployment, roadmap scope, login modes and the test account: decisions 0007 to 0010 (2026-11-10).
- How the agent reads the test-account credentials: Proton Pass references in `scripts/lab.env.example` and `scripts/seed.env.example`, run through `pass-cli run --env-file` (ruling R9); no secret in argv or files. Only exercised so far up to the first login's CAPTCHA answer.
- Can a non-official identity reach the calendar API: yes for `Other`, on the fork's evidence from the author's account (gate E03 passed, R13; decision 0011, provisional).
- What can seal secrets on blackwater and morpheus: E15, SPEC §7.3.
- What Thunderbird sends and requires, and whether it needs VTIMEZONE (no): E08 from source (SPEC §9, E11); built and contract-tested in M3 (E08, "What the bridge does (M3)").
- Library for the CalDAV adapter: our own server, go-webdav as a test client only (E17, M3-R5).
- Model: iCalendar-native for v1, decision 0012 accepted by the author on 2026-11-11 (JSCalendar deferred to the first non-CalDAV adapter).
- Does decryption need a sealed decrypted cache (E09): no, offline 0.2 to 0.4 ms per event; the live cold-start figure is still pending.

## Still open for the author

1. Approval of SPEC.md draft v2 (§6, §9, §13 to §15 now describe M3 as built).
2. Confirm decision 0011 (`Other` by default); it becomes final after the E03 live run.
3. Solve the test account's CAPTCHA (`spec/HUMAN-CHECKS.md`, step 1): the only author action that unblocks the live runs.

## Still open, run by the agent (deferred live runs, `spec/HUMAN-CHECKS.md` steps 2 to 4)

- E03: scope list for `Other`, the three other identity candidates.
- E02: is a solved CAPTCHA token valid in a new process, and after how long (row 1 pending).
- E01: access-token TTL, refresh rotation, old-token refusal (immediate), session lifetime under 6-hourly refresh (14-day timer).
- Synthetic data set: not yet applied to the test account (`spec/experiments/DATASET.md`).
- SPEC §5.2 markers on E01 and E02 stay `[assumed]` until then.

## Still open, later

- Live Thunderbird session against the bridge (E08 and E11 live confirmation, E09 live half, the ten checks of step 6): **after M4** (M3-R1), `spec/HUMAN-CHECKS.md` step 6, test account only.
- TPM2 sealing is untested everywhere (morpheus lacks libtss2; installing was out of scope), as is system-scope `systemd-creds` on blackwater (sudo is interactive, ruling R18), any run inside a systemd service, and any container (E15).
- E10 (signature verification of calendar key parts) must be settled before M4 encrypts to a calendar key (ruling R17).
- M2 live session (E04, E05, E07): `pcb-lab probe feed` is built; the experiments are source-based until the run (`spec/HUMAN-CHECKS.md`, "M2 live session"). Markers on the feed, cursors, ETag stay `[assumed]` until then.
- E06, E12 to E14, E16 are not started (SPEC §17).
- Fresh session keys on every update (rulings M4-R17, M4-R19): once live check L.4 #3 (research L.4) shows Proton accepts new key packets on an update, mint new keys and send their packets on every update instead of reusing the row's. That removes the residual risks of SPEC §5.3 (a row already substituted when first read; an encrypted plaintext the server guesses, or learnt elsewhere for the same UID, confirmed with its clear signature and replayed as proof of a key it chose). Until then key-packet pinning (Task 7) is the primary defence and the signature proof (`ical.ProvesKey`) the secondary one.

## Parked by M3

For M4:
- Derive writability from the member permissions (M3-R15): until then every owned normal calendar advertises read and write and every write is refused (M3-R1). Must be in place before M4 enables writes, with E14 for view-only shares.
- Refuse every write to a placeholder object (412 or 403, M3-R4), so an edit in Thunderbird can never overwrite the real event.

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
- Integrity against a malicious server (M3 final re-review): bind each part's `Author` to the key that signed it, and refuse or mark unsigned cleartext attendees/personal parts, once E10 says which parts Proton signs with which key (SPEC §5.3).
- M4: a decrypted part may itself carry `X-PCB-PLACEHOLDER`; the write refusal for placeholders must use the service's own placeholder state, never that property.
- Final audit: `TestServeReadsOnItsOwnHandle` does not prove reads use Serve's handle (stays green with that branch disabled); a deleted calendar answers 503 instead of 404 while another calendar is pending or the account has none.

For the final audit (parked minors):
- Duplicate-count writes from unordered snapshots (diagnostic only, Task 4).
- Over-long comment in `serve.go` `refreshSettings` (Task 4).
- Limiter: an expire, re-fail, evict test; a "no touch after Close" test inside `touch()` (Task 5).
- A malformed token on a pending calendar answers 403, not 503 (race only); a stale token on a pending calendar is untested; a home-set Depth 0 service failure logs a warning per request (Task 6).
- M3-R18: on platforms without filelock, `clients.json` writers take an in-process mutex only, so a concurrent CLI and `pcb serve` write can drop a `LastUsed` update (documented; Task 5).
- Decision (Task 7): `pcb doctor` keeps an Error, not a Warning, for a too-wide `tls/` directory or key: a readable key may already have been exposed, and the repair at start cannot undo that.
- `go-vcard` moved to `v0.0.0-20230815062825-8fda7d206ec9` through go-webdav (test-only import, MVS): only README and `example_test.go` differ (E17); re-check when go-webdav changes.
