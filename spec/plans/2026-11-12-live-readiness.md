# Live-run readiness — plan

> **For agentic workers:** use superpowers:subagent-driven-development. The evidence is the live-check triage: `hc-triage.md` in the SDD workspace. Item ids are G1–G17 and the row ids it lists.

**Goal:** make the author's live session (`spec/HUMAN-CHECKS.md`, test account only) short, safe and correct before it starts:
- fix the run documents;
- guard every live command against touching the author's real account or machine;
- let the real `pcb` binary be rehearsed end to end against a local fake.

**Spec:** `spec/SPEC.md`, `spec/HUMAN-CHECKS.md`, `spec/experiments/DATASET.md`, decisions 0007–0013.

## Global Constraints
- **No live Proton call** by any agent, ever. The fake runs on 127.0.0.1 only.
- **Nothing reads or writes the author's real dirs:**
  - `~/.config/proton-cal`
  - `~/.config/pcb`
  - `~/.local/state/pcb`
  - the author's keyrings
- **Every live command in the docs runs through a guard** that refuses default dirs.
- **Tests bite** (sabotage recorded).
- **No user-facing behaviour change in `pcb`.** Developer tooling lives under `cmd/pcb-lab`, `cmd/pcb-fake` or `scripts/`, and is never built into the image or installed by `make install`.

## Tasks

### R1: Run documents and a guarded runner (G2–G6, G8–G12, G14, G17)

**Files:**
- `spec/HUMAN-CHECKS.md`
- `spec/experiments/DATASET.md`
- a new `scripts/live/` holding `preflight.sh`, `guard.sh` and `README.md`
- `Makefile` target `live-preflight`

**What to do:**
1. **`scripts/live/guard.sh`**, sourced by every live snippet in the docs:
   - It refuses to continue unless each of `PCB_CONFIG_DIR`, `PCB_STATE_DIR`, `PCB_LAB_DIR` (or the lab's real variable) and `PROTON_CAL_CONFIG_DIR` (when proton-cal is used) is set, absolute, and under a dedicated run root such as `~/pcb-live-run/<date>/`.
   - It refuses `~/.config/*`, `~/.local/state/pcb` and any dir holding the author's real files.
   - It prints the resolved dirs.
   - Every doc snippet that sets an env var and then logs in becomes ONE command (`env … cmd`, or a guarded subshell), never `export` followed by a separate call (G10).
2. **`scripts/live/preflight.sh`**, run before the session:
   - it checks the binaries (`pcb`, `pcb-lab` and the `proton-cal` build) and their versions;
   - it checks the guard and the free ports;
   - it confirms the host is not morpheus;
   - it warns when the host is blackwater for the steps that reboot or cut the network;
   - it prints the login budget;
   - it keeps a login ledger (time, step, purpose), and refuses a login within 60 s of the last one or beyond the budget.
3. **Fix the documents:**
   - **G2:** the step-2 lab logins each get their own state dir; later steps name which session they resume.
   - **G3:** 7.3.2 and 7.3.3 probe `/dav/calendars/me/`, which answers 503, not the root.
   - **G4:** M1.8 no longer wipes dirs that later steps reuse; it uses its own.
   - **G5, G6:** a true ordered sequence with "author needed here" marks and the real login count (12–13), so the author knows to stay reachable.
   - **G8:** build and install steps (worktrees are gone): `make build`, `go build ./cmd/pcb-lab`, into the run root, never `~/.local/bin`.
   - **G9:** the sudo steps are marked "author types this" (ruling R18), or rewritten to need no sudo where possible.
   - **G12:** `docker exec -it` stays only where a TTY is real; scripted steps drop `-t`.
   - **G11:** step 7 runs on a throwaway Debian VM or container host, never blackwater, for the reboot, network cut and `--delete-data` steps. The compose file in HUMAN-CHECKS publishes 127.0.0.1 only. 6.3E names the second machine, or says "any LAN machine of the author's".
   - **G14:** as the triage says.
   - **G17:** step 2a asks the author to solve a fresh CAPTCHA right before it, not to reuse a stored token.
4. **Mark 7.8 complete:** the unauthenticated pull answers `unauthorized`, and the CI runs are green (triage, "done now").

**Test:**
- `scripts/live/guard_test.sh`, a bats-free bash test run by `make live-preflight-test` and in CI: it shows the guard refuses each forbidden dir, a relative path and an unset var, and accepts a run-root dir;
- shellcheck clean.

**Commit:** `🧪 Guard and order the live session`

### R2: A local fake for rehearsals (G1)

**Files:**
- a new `cmd/pcb-fake` (developer only)
- `internal/proton/protontest`, only as far as needed to run outside `testing`
- a new `scripts/live/rehearse.sh`
- `Makefile` target `rehearse`

**What to do:**
1. **`pcb-fake serve --listen 127.0.0.1:0 --seed <file>`** serves the protontest fake over loopback HTTP.
   - It refuses any non-loopback address.
   - It prints the base URL and the test account's username.
   - It takes the password and the TOTP secret from a file, never argv.
   - It seeds the test data set (DATASET.md's shape: calendars, recurring events, exceptions, attendees, alarms, shared calendar) so that pcb has realistic content.
   - Refactor `protontest.New` to accept a minimal cleanup interface instead of `testing.TB`, if needed. The tests keep passing unchanged.
2. **`scripts/live/rehearse.sh`** builds pcb and pcb-fake into a temp run root, starts the fake, and points a real `pcb` at it through `[proton] base_url` (config in the temp dir). It runs the author's flow offline:
   - login with TOTP held through the daemon (`resume --totp`);
   - `serve --wait-for-login`;
   - `client show --reveal`;
   - a CalDAV PROPFIND, sync-collection and multiget with curl;
   - a PUT create, update, delete with read-back;
   - `rotate` → 401 on the old password;
   - a session revoked by the fake → `needs-login`, 503, re-login with no restart;
   - logout.

   Each step asserts its expected output (exit code, status line, HTTP status) and the script exits non-zero on the first mismatch.
3. **Run it in CI** as a job `rehearsal` (no network beyond loopback).
4. **HUMAN-CHECKS gets a line:** run `make rehearse` before the live session; a red rehearsal stops the live run.

**Test:** the rehearsal itself is the test. Add one sabotage to the report, for example a pcb built with the read-back disabled, which must fail the rehearsal.

**Commit:** `🧪 Rehearse the live session against a local fake`

### R3: Prepare the probes the live session lacks (triage top 3 and 5)

This comes after R1 and R2. Build, against the fake only:
- **`pcb-lab probe sessions`:** list, and revoke other sessions of the test account (the endpoint and auth scope as go-proton-api, MIT, uses them; never from WebClients).
- **`pcb-lab probe write-matrix`:** the L.4 #3, #5, #6, #8, #10 cases plus the unbumped time edit and an update with `Notifications` absent. It writes through the Proton API directly, reads back, and prints one line per case.

Both are tested against the fake, and both carry the same guard as R1. The live run of each is a HUMAN-CHECKS step.

**Commit:** `🔬 Lab probes for sessions and the write matrix`
