# Live runs and human checks

One ordered sequence. **Only step 1 needs the author** (solve a CAPTCHA, and a fresh one if Proton refuses the old token). Everything else is run by the agent alone, from this repo, once the author says "captcha solved at HH:MM". The per-experiment detail (what each probe records, how to fill the results) is in the "Pending live run" section of each experiment file; this file is the order and the rules.

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
