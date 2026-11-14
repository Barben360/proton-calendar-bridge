# E02 — CAPTCHA token reuse

Feeds C1 auth and the CLI's resume flow (SPEC §5.2, §17).

## Question

Is a solved CAPTCHA (human-verification) token valid in another process, and for how long after it was issued?

## Method

Every human-verification (HV) event during a lab login is appended to `~/.local/state/pcb-lab/hv-log.jsonl` by `internal/lab/session` (`TestHVEventsAreLogged`): `{at, token_len, sent_captcha_token}`, never the token. `hv.json` (token, verify URL, time) is kept for the current challenge and removed after a successful login; the log is kept.

Protocol, applied to any HV during any lab login:

1. The login stops cleanly (exit 10, one `/auth/v4` call, no retry). The author solves the CAPTCHA at the URL in `hv.json` and says when ("captcha solved at HH:MM"). **The delay between the challenge (`at` in `hv-log.jsonl`) and the author's resolution is itself a measurement**: it is the age of the token when it is first retried.
2. The agent retries from a **new process**, at least 1 min after the previous login attempt (the guard enforces 60 s): `pass-cli run --env-file ~/.config/pcb-live/lab.env -- pcb-lab login --captcha-token <token from hv.json>`.
3. If that retry is answered by an HV again (a line with `sent_captcha_token: true` appears in the log), the token did not survive that age or a process change. The author solves the new challenge right away, and the agent retries after 30 min (new process) to test a different delay.
4. Each outcome is recorded below: challenge time, solve time, retry time, token age at retry, process (same or new), accepted or refused.

Single-process interactive fallback: whatever E02 finds, M1 keeps the flow where one process shows the URL, waits for the author and sends the token itself.

## Results

Rows 2 to 6 were read on 2026-11-14 from the records the live runs left (the run root's `hv-log.jsonl` files, the login ledgers and the saved `pcb login --json` outputs); no new login was made for E02. Times UTC. "Challenge" is the `hv-log.jsonl` line where there is one, else the ledger time of the login that met the CAPTCHA (the challenge comes within seconds of it). "Accepted" means the retry logged in with no new human-verification answer (no second `hv-log.jsonl` line, `state: ready` or a saved session).

| # | Challenge (UTC) | Token length | Solved | Retry | Token age at retry | Outcome |
|---|---|---|---|---|---|---|
| 1 | 2026-11-10 23:47:39 | 24 | never | never | — | dropped: superseded by HUMAN-CHECKS step 1's fresh challenge |
| 2 | 2026-11-12 22:33:34 (step 2a, `pcb-lab login`) | 24 | by the author, before the retry | 22:34:41, new process (`pcb-lab login --captcha-token`) | 1 min 07 s | accepted |
| 3 | 2026-11-12 22:49:27 (step 3, `pcb-lab login`) | 24 | by the author | 22:53:48, new process | 4 min 21 s | accepted |
| 4 | 2026-11-12 23:15:29 (M1.2, `pcb login`) | 24 | by the author | 23:21:56, new process (`pcb login resume --captcha-token`) | about 6 min 27 s | accepted (`state: ready`) |
| 5 | 2026-11-12 23:45:11 (M1.7, the unattended relogin of `pcb calendar list`) | 24 | by the author | 23:46:14, new process (`pcb login resume --captcha-token`) | about 1 min 03 s | accepted (`state: ready`) |
| 6 | 2026-11-13 22:54:00 (step 9, `pcb-ui`'s wizard) and 2026-11-13 23:41:30 (step 10, the loopback page) | — | by the author | the same process (the wizard's `I solved it`; the page's continue) | about 2 min 38 s; about 2 min 53 s | accepted |

Not a CAPTCHA case: HUMAN-CHECKS 2b, 2c, 2d and the seed's second `proton-cal` login met none or are proton-cal's own flow (not pcb's).

## Pending live run (HUMAN-CHECKS steps 1 and 2)

After the author solves the pending challenge and says when:

**Not to be pasted as is: run through `scripts/live/run.sh` (`live`), as HUMAN-CHECKS step 1 and 2a spell it (the login is ledgered and uses the run root's lab dir).**

```
cd <a checkout of this repository>
pass-cli run --env-file ~/.config/pcb-live/lab.env -- pcb-lab login --captcha-token <token from hv.json>
```

- Accepted: row 1 outcome "accepted, token age <retry time minus challenge time>, new process". Continue with the E03 sequence (`E03-client-identity.md`).
- Refused with a new HV: row 1 "refused at <age>"; a new `hv-log.jsonl` line appears with `sent_captcha_token: true`. The author solves the new URL right away; the agent retries within a minute (row 2, short delay), then if refused again after 30 min (row 3).
- Any HV later in M0 (E03 candidates, seeder) is a new row, with the protocol above.

## Pending M1 data point

`pcb login resume --captcha-token` (M1) is the second E02 measurement, run in the M1 live session (`HUMAN-CHECKS.md`, step M1.3): a CAPTCHA at the first `pcb login`, solved by the author, then resumed from a new process at least 60 s later. Add a row with the same columns; no CAPTCHA at all is recorded as "no CAPTCHA seen" with the login count.

## Conclusion

**Answered for short delays [verified: live 2026-11-12/13, rows 2 to 6]: a solved token is valid in a new process**, from about 1 minute up to about 6.5 minutes after its challenge (four cases, every one accepted, no second challenge); the same holds in one process (row 6). Longer ages (hours) were not measured: no run left a solved token unused that long, and provoking one would cost logins on the test account for no design change. `pcb login resume --captcha-token` from another process is therefore sound within minutes of the challenge. The paragraph below was the plan before the runs.

Open (as planned before the runs). If no further HV occurs after the first solved one, E02 stays open with "no CAPTCHA seen in N logins" (N = the number of logins in the live-run log kept in the pending section of `E03-client-identity.md`, cross-checked with the lines of `hv-log.jsonl`); M1 then keeps the single-process interactive fallback only.

## Feeds

C1 auth `needs-captcha(url, token)` state (§5.2): whether the CLI may resume from a stored token in another process, and for how long the stored token stays usable.
