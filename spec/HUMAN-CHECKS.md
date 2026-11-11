# Live runs and human checks

One ordered sequence. **Only step 1 needs the author** (solve a CAPTCHA, and a fresh one if Proton refuses the old token); the M1 live session at the end adds at most three short author actions (a CAPTCHA if one is asked, adding the holidays and subscribed calendars, revoking a session). Everything else is run by the agent alone, from this repo, once the author says "captcha solved at HH:MM". The per-experiment detail (what each probe records, how to fill the results) is in the "Pending live run" section of each experiment file; this file is the order and the rules.

Nothing below has been run: no live result exists yet.

## Rules for the whole run

- Test account only (vault `pcb-dev`, item `proton-calendar-bridge-test`), credentials through `pass-cli run --env-file scripts/lab.env.example --` (never typed, never in argv). `pass-cli` must be logged in on blackwater (pre-check in step 2).
- At most one login per step, **at least 60 s between any two logins** (the guard refuses sooner with exit 20; it is per state dir, so across dirs the agent keeps the time of the last login by hand). Honour `Retry-After`.
- Keep a login log while running (step, UTC time, outcome); E02 counts logins from it and E03's pending section asks for it.
- Stop conditions (stop the whole sequence, record what happened, report; do not retry in a loop):
  - any login or probe ends in a CAPTCHA (exit 10): record it as a new row in `E02-captcha-token.md` (protocol there), ask the author for a fresh solve (the only other author step); the remaining steps wait;
  - any 429 or `Retry-After`: wait it out, never faster; a second one in the same step: stop;
  - `pcb-lab probe refresh` non-zero (refresh refused, session gone): stop the sequence; resume from the next step only after a fresh guarded login;
  - `Other` without calendar access on the test account while the fork's evidence says it works: investigate the account state before anything else;
  - no candidate gets calendar access: stop M0 and report (the E03 gate);
  - `seed apply` exit 40 (rate limit): wait a few minutes, rerun unchanged; a failed command naming an id (`s346.e`, ...) is a dataset fix, not a retry.
- A candidate without calendar access is an expected measurement (probe exit 20): note it and go on.

## 1. Author: show and solve the test account's CAPTCHA (the only author step)

The first `pcb-lab login` on blackwater (2026-11-10 23:47 UTC) was answered with a Proton CAPTCHA, as a new client usually is.

1. Show the verify URL (the token in it is a CAPTCHA challenge, not an account secret):
   `python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.local/state/pcb-lab/hv.json")))["url"])'`
2. Open it in any browser, solve the CAPTCHA, close the tab.
3. Tell the agent "captcha solved at HH:MM".

The agent then continues alone with step 2. If Proton refuses the hours-old token (new HV at step 2), the agent starts one fresh login, you solve the new URL right away, and the agent resumes within a minute (E02 learns the limit; protocol in `E02-captcha-token.md`).

## 2. Agent: pre-check, build, then the E03/E02/E01 login sequence

Pre-check first: `PROTON_PASS_AGENT_REASON="<why>" pass-cli info` must succeed. If not, log pass-cli in again with the PAT file `~/.config/pcb-dev/pass-pat`, passed through the environment variable `PROTON_PASS_PERSONAL_ACCESS_TOKEN` (never argv), then repeat the check. Then, from `/home/jduff/Workspace/worktrees/proton-calendar-bridge-m0`: `go build -o ~/.local/bin/pcb-lab ./cmd/pcb-lab`. Commands and result tables: `E03-client-identity.md` ("Pending live run"), where E03's steps 1 to 4 are 2a to 2d below and E03's step 5 (E01's dedicated login) is step 3 here. The login log is kept in that E03 section, as E02 and E03 expect.

| # | Login (state dir) | Then, on that session | Experiment |
|---|---|---|---|
| 2a | `Other` with the solved token, default dir (`login --captcha-token <token from hv.json>`) | `whoami --json` (scope list); `probe calendar-access`; **`probe refresh` once** (never again on this session) | E02 (token accepted in a new process, age at retry), E03 (`Other`), E01 (immediate results: `ExpiresIn`, rotation, old-token refusal) |
| 2b | `linux-mail@1.0.0`, +60 s | `probe calendar-access` | E03 |
| 2c | `web-calendar@5.0.131.2`, +60 s | `probe calendar-access` | E03 |
| 2d | `external-drive-caloxide@0.2.1-stable`, +60 s (negative control) | `probe calendar-access` | E03 |

Then fill the E03 results table and decide: set decision 0011 final (or revise it) and turn SPEC §5.1 to `[verified: E03]`; fill E02 row 1 and the E01 "Results" immediate part. Raw redacted recordings go to `spec/experiments/raw/E03/` and `raw/E01/` (redaction by `internal/lab/redact` only).

## 3. Agent: E01 long probe (its own dedicated session)

At least 60 s after 2d. Nothing else ever uses this state dir. Commands in `E01-session-lifetime.md`: `pcb-lab login --state-dir ~/.local/state/pcb-lab-e01`, first `probe alive` sample, install and enable the user timer `pcb-lab-e01.timer` (every 6 h, `Persistent=true`). It samples for 14 days; afterwards the agent copies `e01.jsonl` to `raw/E01/alive.jsonl`, fills the E01 results (age and code when the session ended, or "survived 14 days of 6-hourly refresh") and uninstalls the timer. E01's questions 1 to 3 are answered by step 2a; question 4 only after the 14 days.

## 4. Agent: seed the synthetic data set

At least 60 s after step 3's login. Commands and the wipe/reseed notes: `DATASET.md` ("Pending live run"): build the fork's `origin/main` as `proton-cal` in a scratch worktree, `proton-cal login --no-prompt` under `PROTON_CAL_CONFIG_DIR=~/.local/state/pcb-lab-seed` with `scripts/seed.env.example` (this login counts as a lab login), `pcb-lab seed plan`, then `pcb-lab seed apply` (about 370 writes, one per 2 s, about 12 minutes; stops on the first rate limit with exit 40, rerun resumes). Record the final counts in `DATASET.md`.

## 5. After the run (agent)

Update `E01`, `E02`, `E03`, `DATASET.md`, SPEC §5.1/§5.2 markers, `spec/OPEN.md`, commit on a branch. The E01 14-day window keeps running after this; its close-out is a separate, later commit.

## 6. Later: Thunderbird against the bridge (author, GUI; after the M3 CalDAV adapter exists)

Placeholder, to be written when the adapter exists. Scope: a real Thunderbird (local, then over the network) subscribes to the bridge, shows every calendar and stays in sync; confirms the E08 source findings live (discovery through `/.well-known/caldav`, Basic 401 challenge, `sync-collection` with the empty token, `calendar-multiget`, PUT/DELETE precondition handling, no VTIMEZONE needed in responses, dismissed alarms not re-firing after an edit). Needs the author because it is a GUI client. Source-based expectations: `spec/experiments/E08-thunderbird-caldav.md`.

## M1 live session (after steps 1 to 5; agent-run except three author actions)

Proves `pcb` against the test account. Worktree `/home/jduff/Workspace/worktrees/proton-calendar-bridge-m1` (or main once merged). Same rules as above, plus:

- Isolated dirs for every `pcb` command below: `export XDG_CONFIG_HOME=$HOME/.local/share/pcb-m1-live/config XDG_STATE_HOME=$HOME/.local/share/pcb-m1-live/state`. Run each login as `pass-cli run --env-file scripts/pcb.env.example -- bin/pcb ...` (username, password, TOTP URI from the environment; never argv).
- **At most 3 logins in the whole session** (L1 step M1.2, L2 step M1.3, L3 step M1.7), at least 60 s after the last M0 login and after each other. Keep the login log (step, UTC time, outcome) with the M0 one.
- Stop conditions are the M0 ones. A refresh or relogin answered by anything not listed in "signals to confirm" below: stop, record the raw status and Proton code, do not retry.
- The CAPTCHA is shared with step 1: if step 1's challenge was never solved, the author's single solve covers both; if M0 consumed it, M1.2 may meet a fresh one (then the author solves the new URL right away).

**M1.1 Build and pre-check.** `make build`. `bin/pcb version`. `PROTON_PASS_AGENT_REASON="<why>" pass-cli info` must succeed (step 2 pre-check).

**M1.2 Login (L1).** `pass-cli run --env-file scripts/pcb.env.example -- bin/pcb login --non-interactive --unattended-relogin --json > /tmp/pcb-m1-l1.json; echo $?`
- Exit 0, `data.state` `ready`: no CAPTCHA this time; record "no CAPTCHA seen" for E02, skip M1.3 (the budget keeps one spare login) and go to M1.4.
- Exit 10 with `error.message` naming `https://verify.proton.me/?methods=captcha&token=...` and `error.hint` `pcb login resume --captcha-token <T>` (T quoted only if needed), and `data.captcha_token` carries the token itself: the author solves the URL (`jq -r .error.message /tmp/pcb-m1-l1.json`), says "captcha solved at HH:MM"; the agent notes the challenge time (the L1 time) and goes to M1.3.
- Exit 10 for any other reason (wrong credentials, 2FA): record `error.message`, stop. Exit 20 "too soon": wait the 60 s and rerun once.

**M1.3 CAPTCHA resume (L2, E02 data point), only after a CAPTCHA at M1.2.** At least 60 s after L1, a new process: `T=$(jq -r .data.captcha_token /tmp/pcb-m1-l1.json); pass-cli run --env-file scripts/pcb.env.example -- bin/pcb login resume --captcha-token "$T" --non-interactive --unattended-relogin --json`. Expect `ready`. Record in `spec/experiments/E02-captcha-token.md` a new row: challenge time (L1), solve time (author's message), retry time, token age at retry, new process, accepted/refused (a fresh 9001 or code 12087 instead is a refusal; then the author solves the new URL at once and the agent retries within a minute: second row). If M1.3 is refused twice, stop.

**M1.4 Status and the M1 done criterion.** `bin/pcb status --json` (expect `phase` `ready`, `unattended` true, `last_login` set, no network call) then `bin/pcb calendar list --json`. **M1 is done when** the list shows every calendar of the test account with `type` (normal, subscribed, holidays), `read_only`, exactly one `default`, and `keys` `ok` for each. Save the redacted output (names stay; ids and colours are not secrets but keep only what the write-up needs). Any `keys` `error`: record `keys_error` and the calendar type; do not stop. Then the author adds one holidays and one subscribed calendar in the web app (author action, can be done any time before M1.5; skip if absent and note it) and the agent reruns `bin/pcb calendar list --json` (record types and flags). No login is used.

**M1.5 Recorded fixtures and contract test.** Every `pcb-lab probe` resumes the lab session left by M0 step 2a and may rotate its tokens (only `probe refresh` is limited to once, for E01). Run: `bin/pcb-lab probe calendar-access --state-dir ~/.local/state/pcb-lab --record-dir /tmp/pcb-m1-fixtures` (build `pcb-lab` if needed). It records the redacted `/calendar/v1` list, `/calendar/v1/{id}/bootstrap` and `/settings/calendar` answers (`internal/lab/record` refuses anything with a secret). Ruling R11m: only the list and the settings are fixtures and contract-tested; the probe's `/calendar/v1/{id}/bootstrap` is the old v1 shape, not the `/calendar/v2` bootstrap pcb uses, which is proven live by M1.4 (`pcb calendar list` showing `keys` `ok`), so the bootstrap file is not copied. If the lab session is gone, a fourth login is not allowed: defer this step and note it. Then:
1. `mkdir -p internal/proton/calendar/testdata && cp /tmp/pcb-m1-fixtures/*calendar-list.json /tmp/pcb-m1-fixtures/*settings-calendar.json internal/proton/calendar/testdata/`; open each file and check by eye that no token, key block, passphrase or email is in it.
2. Add `TestDecodersAcceptRecordedFixtures` to `internal/proton/calendar` (no network): serve each fixture's `body` at its recorded `path` from an `httptest` server, run `calendar.List` and `calendar.DefaultID` through `api.Client`, and assert at least one calendar, non-empty names, and a default id that is one of them (or none, if the settings fixture has none). It must go red when a field is renamed in a fixture (sabotage once, restore).
3. `go test ./internal/proton/calendar/...`, `make lint test`, commit `🧪 Record calendar fixtures from the test account`.

**M1.6 Signals to confirm.** For each item of "Signals to confirm" below, record the real HTTP status and Proton code seen in M1.2 to M1.7 (the JSON `error.message` of the failing command), or "not exercised". Items that need a deliberate failure (wrong TOTP, wrong password) are exercised only if the login budget has room (a wrong password at `pcb login` costs a login); otherwise leave them `[assumed]` and say so in the write-up. If Proton answers differently, adjust the classification in `internal/proton/auth` or `internal/proton/api` test first, in its own commit.

**M1.7 Unattended relogin after a revoke (L3).** The author revokes the `pcb` session (account.proton.me, Security, Sessions; the one created at M1.2/M1.3) and says "revoked at HH:MM". The agent, at least 60 s after the last login: `pass-cli run --env-file scripts/pcb.env.example -- bin/pcb calendar list --json`, then `bin/pcb status --json`. Expect exit 0 with the calendar list after exactly one unattended relogin (`status.last_relogin` set), or exit 10 with a CAPTCHA (`status.phase` `needs-captcha`, `captcha_url` set): both are results to record, and the first confirms the refusal signal for a revoked session (item "Refused refresh"). If the author is away, record "revoke skipped" and go on.

**M1.8 Logout and clean up.** `bin/pcb logout --json` (expect `revoked` true if the session was alive, then `bin/pcb status --json` shows `logged-out`). Check the revoke at account.proton.me if the author is present, else rely on `revoked`. Then `rm -rf ~/.local/share/pcb-m1-live`.

**M1.9 Write up.** Create `spec/experiments/M1-live.md` (what each step showed, redacted: exit codes, `phase`, calendar types and flags, the login log with times, the status and Proton code for every signal below); fill the E02 data point (M1.3) or "no CAPTCHA seen in N logins"; update SPEC §5.2 (locked scope on fresh sessions: `[verified]` only if M1.2 or M1.7 exercised it; session lifetime and CAPTCHA reuse markers per E01/E02 results) and §5.4 (calendar types and keys per type, from M1.4); README Status; commit `🧪 M1 live session against the test account`.

### Signals to confirm (M1 live run)

Real-Proton answers that `internal/proton/auth` classifies on assumption (ruling R6m); each one is recorded during M1.6/M1.7, and the code is adjusted if Proton answers differently.

- Wrong TOTP on `POST /auth/v4/2fa`: assumed HTTP 422 (fake uses code 12060), mapped to `ErrTOTPRejected`; record the real status and code.
- Wrong login password on `PUT /core/v4/users/unlock`: assumed HTTP 422 with code 8002, mapped to `ErrBadCredentials`; record the real status and code.
- Refused refresh on `POST /auth/v4/refresh` (spent or revoked refresh token): assumed HTTP 401 or 422, or code 10013, mapped to `api.ErrSessionEnded`; record the real status and code (any other answer must leave the session alone).
- Wrong password on `POST /auth/v4`: assumed HTTP 422 with code 8002, mapped to `ErrBadCredentials`; record the real status and code.
- Refused or expired CAPTCHA token on `POST /auth/v4`: assumed code 12087, mapped to `ErrCaptchaRejected` (a fresh 9001 challenge instead is also possible); record what Proton sends.
- Locked scope on fresh sessions: whether salts need `PUT /core/v4/users/unlock` (403 with code 9101) at all with this client identity; record whether elevation happened.
- Calendar bootstrap on holidays and subscribed calendars: whether member passphrase and keys exist for them (`keys` `ok` or `error` per type).
