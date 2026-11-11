# Open questions and pending items

State at the close of M2 (2026-11-11).

## Answered

- Client identity, deployment, roadmap scope, login modes and the test account: decisions 0007 to 0010 (2026-11-10).
- How the agent reads the test-account credentials: Proton Pass references in `scripts/lab.env.example` and `scripts/seed.env.example`, run through `pass-cli run --env-file` (ruling R9); no secret in argv or files. Only exercised so far up to the first login's CAPTCHA answer.
- Can a non-official identity reach the calendar API: yes for `Other`, on the fork's evidence from the author's account (gate E03 passed, R13; decision 0011, provisional).
- What can seal secrets on blackwater and morpheus: E15, SPEC §7.3.
- What Thunderbird sends and requires, and whether it needs VTIMEZONE (no): E08 from source (SPEC §9, E11).

## Still open for the author

1. Approval of SPEC.md draft v2.
2. Confirm decision 0011 (`Other` by default); it becomes final after the E03 live run.
3. Solve the test account's CAPTCHA (`spec/HUMAN-CHECKS.md`, step 1): the only author action that unblocks the live runs.

## Still open, run by the agent (deferred live runs, `spec/HUMAN-CHECKS.md` steps 2 to 4)

- E03: scope list for `Other`, the three other identity candidates.
- E02: is a solved CAPTCHA token valid in a new process, and after how long (row 1 pending).
- E01: access-token TTL, refresh rotation, old-token refusal (immediate), session lifetime under 6-hourly refresh (14-day timer).
- Synthetic data set: not yet applied to the test account (`spec/experiments/DATASET.md`).
- SPEC §5.2 markers on E01 and E02 stay `[assumed]` until then.

## Still open, later

- Live Thunderbird session against the bridge (E08 live confirmation): after the M3 CalDAV adapter (`spec/HUMAN-CHECKS.md`, step 6).
- TPM2 sealing is untested everywhere (morpheus lacks libtss2; installing was out of scope), as is system-scope `systemd-creds` on blackwater (sudo is interactive, ruling R18), any run inside a systemd service, and any container (E15).
- E10 (signature verification of calendar key parts) must be settled before M4 encrypts to a calendar key (ruling R17).
- M2 live session (E04, E05, E07): `pcb-lab probe feed` is built; the experiments are source-based until the run (`spec/HUMAN-CHECKS.md`, "M2 live session"). Markers on the feed, cursors, ETag stay `[assumed]` until then.
- E09 (decrypt cost) moved to M3 with event decryption (ruling R1 of M2).
- E06, E12 to E14, E16, E17 are not started (SPEC §17).
