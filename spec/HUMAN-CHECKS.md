# Live runs and human checks

One ordered sequence, run **in the order of the table below** (the sections of this file keep their old numbers, which the experiment files and SPEC cite; the order is not the order of the sections). The agent runs everything, **but the author must stay reachable for the whole run**: any login can be answered with a CAPTCHA (about 13 logins in all, the table counts them), and the marked steps need the author in person. The per-experiment detail (what each probe records, how to fill the results) is in the "Pending live run" section of each experiment file; this file is the order and the rules. Where an experiment file still names `~/.local/state/pcb-lab*`, `~/.local/bin/pcb-lab` or an `export`, this file wins: the dirs are the run root's (next section).

Nothing below has been run against Proton yet, except the GHCR checks of 7.8 (marked there): no live result exists.

## The order, the author's marks and the login count

| # | Step | Author needed? | Logins |
|---|---|---|---|
| 0 | Before the session: build into the run root, `make live-preflight`, run `make rehearse` (added by R2: a red rehearsal stops the live run) | no | 0 |
| 1 | **1** a fresh CAPTCHA, then **2a** | **AUTHOR NEEDED: solve the CAPTCHA the moment the agent shows it** | 2 (the challenge, then 2a) |
| 2 | **2b, 2c, 2d** (60 s apart) | reachable (any login may meet a CAPTCHA) | 3 |
| 3 | **3** the E01 long probe | reachable | 1 |
| 4 | **4** the seed data set | reachable | 1 |
| 5 | **M1** live session | **AUTHOR NEEDED: M1.2/M1.3 CAPTCHA if asked, M1.4 adds the holidays and subscribed calendars, M1.7 revokes a session** | 3 (M1.2, M1.3 only after a CAPTCHA, M1.7) |
| 6 | **M2** live session | **AUTHOR NEEDED: M2.3 edits in the web app (ten of them), M2.6 one title edit** | 0 (1 spare: a relogin if Proton ended the session) |
| 7 | **6** Thunderbird and the M4 writes | **AUTHOR NEEDED: the 6.3 sitting (A to F), Thunderbird and the web app** | 0 |
| 8 | **7** the M5 daemon, service and container, **on a throwaway VM**, with its own run root | **AUTHOR NEEDED: 7.1.7, 7.2.5, 7.3.1 (revoke), 7.4.1 and 7.4.3, 7.5, 7.6.3, 7.7; the author types the `sudo` lines** | 3 |
| 9 | **Close-out**: M1.8 (logout, remove `$R/pcb`), then step **5** (write-up) | no | 0 |

Total: 2 + 3 + 1 + 1 + 3 + 3 = **13**, 14 with the M2 spare; every CAPTCHA re-solve adds a login. The main run root's ledger allows 11 (everything except step 7); step 7's VM keeps its own ledger with a budget of 3, and its first login is at least 60 s after the last entry of the main ledger (the agent compares by hand). The author's marked steps, in order: 1, M1.2/M1.3, M1.4, M1.7, M2.3, M2.6, 6.3, 7.1.7, 7.2.5, 7.3.1, 7.4.1/7.4.3, 7.5, 7.6.3, 7.7.

## The run root, the guard and the build (read first)

Every live command runs inside one run root, `~/pcb-live-run/<run>/` (`<run>` is the date, e.g. `2026-11-14`; `$R` in prose is shorthand for it). Layout: `bin/` (`pcb`, `pcb-lab`, `proton-cal`, built here and never installed to `~/.local/bin`), `pcb/config` and `pcb/state` (the one pcb session that M1, M2 and step 6 share), `lab/2a`, `lab/2b`, `lab/2c`, `lab/2d`, `lab/e01` (one `pcb-lab` session each), `seed/` (proton-cal's config dir), `logins.tsv` (the login ledger), `m4-live/` and `m5-live/` (scratch of steps 6 and 7).

**Why one command.** Agent shell calls do not share variables. A snippet that `export`s `PCB_STATE_DIR` and logs in with a later call falls back to `~/.config/pcb` and `~/.local/state/pcb` (the second already exists), and `proton-cal login` to the author's own `~/.config/proton-cal`, which would log the test account into the author's real tool (triage G10). So:

- every command that can reach Proton, and every `pcb` command, is ONE call of `scripts/live/run.sh --run <run> [--lab <dir>] [--pcb <dir>] [--proton-cal] [--login STEP 'PURPOSE'] -- <command>`: it sets `PCB_CONFIG_DIR`, `PCB_STATE_DIR`, `PCB_LAB_DIR` (what a lab command receives as `--state-dir`; `pcb-lab` has no variable of its own) and `PROTON_CAL_CONFIG_DIR` under the run root, sources `scripts/live/guard.sh` (which refuses an unset, relative or out-of-run-root dir, every real dir of the author after symlinks are resolved, and any `XDG_*` outside the run root), records the login in the ledger (refused within 60 s of the last or beyond the budget, exit 20), and puts `$R/bin` first on `PATH`. In its arguments, `@RUN@` is `$R` and `@LAB@` is `$R/lab/<dir>`. Details: `scripts/live/README.md`;
- the snippets below write `run.sh` as `live` for short: **`live` = `scripts/live/run.sh --run <run>`**, and the agent always spells it in full;
- never `export` a live variable, never run `pcb`, `pcb-lab` or `proton-cal` bare;
- credentials still only through `pass-cli run --env-file ... --` after the `--` (the guard reads none).

**Build into the run root (the worktrees `proton-calendar-bridge-m0..m4` are gone; main has everything).** From a checkout of main of this repository, `R=$HOME/pcb-live-run/<run>`:
```sh
mkdir -p "$R/bin" && make build OUT="$R/bin/pcb" && go build -trimpath -o "$R/bin/pcb-lab" ./cmd/pcb-lab
# proton-cal: the fork at origin/main, DATASET.md "Pending live run" step 1, with -o "$R/bin/proton-cal"
make live-preflight RUN="$R"        # tools, versions, guard, ports 1080/8443, host, login budget; no API call
make rehearse                       # added by R2: the offline rehearsal; red means stop
```
`make live-preflight RUN="$R" STEP7=1` on the step 7 host: it refuses blackwater. It never runs `pcb login` or any Proton call.

## Rules for the whole run

- Test account only (vault `pcb-dev`, item `proton-calendar-bridge-test`), credentials through `pass-cli run --env-file scripts/lab.env.example --` (never typed, never in argv). `pass-cli` must be logged in on blackwater (pre-check in step 2).
- Logins: the budget is the table above (step 2 alone has four, so "at most one per step" does not hold; each step names its own count). **At least 60 s between any two logins**: the lab's own guard refuses sooner with exit 20 but is per state dir, so the ledger (`live --login STEP PURPOSE`, `$R/logins.tsv`) is what enforces it across dirs, and it also refuses beyond the budget. Honour `Retry-After`.
- Keep the login log: the ledger is it (step, UTC time, purpose); add the outcome by hand after each login.
- Expected outputs after the initial-version audit fixes (`spec/audits/2026-11-12-initial-version.md`), where older lines of this file may differ: a key or state fault is exit 10 in every command (a missing or unreadable key, a `secrets.sealed` or `state.json` of a newer pcb: `upgrade pcb`); `pcb doctor` exits 0 unless a check fails (10 when you must act, 20 only when the daemon alone is unreachable) and shows `needs-login` as a warning `!` while the daemon waits for a login; `pcb sync` while `pcb serve` runs exits 0 with `pcb serve is running and already syncs: see pcb status`, and logged out it is exit 10 `not logged in to Proton`; every refused CalDAV write logs one `refused a CalDAV write` warning (a refusal step expects it; the greps for `refusing a write` and `not confirmed` below are the stop conditions and still mean what they say); a login call that stalls ends at 30 s with exit 20; warnings of the code behind a command print on stderr as `Warning: ...`.
- E02 counts logins from the ledger and E03's pending section asks for it.
- Stop conditions (stop the whole sequence, record what happened, report; do not retry in a loop):
  - any login or probe ends in a CAPTCHA (exit 10): record it as a new row in `E02-captcha-token.md` (protocol there), ask the author for a fresh solve (the only other author step); the remaining steps wait;
  - any 429 or `Retry-After`: wait it out, never faster; a second one in the same step: stop;
  - `pcb-lab probe refresh` non-zero (refresh refused, session gone): stop the sequence; resume from the next step only after a fresh guarded login;
  - `Other` without calendar access on the test account while the fork's evidence says it works: investigate the account state before anything else;
  - no candidate gets calendar access: stop M0 and report (the E03 gate);
  - `seed apply` exit 40 (rate limit): wait a few minutes, rerun unchanged; a failed command naming an id (`s346.e`, ...) is a dataset fix, not a retry.
- A candidate without calendar access is an expected measurement (probe exit 20): note it and go on.

## 1. Author: solve a fresh CAPTCHA, right before 2a (author needed)

The first `pcb-lab login` on blackwater (2026-11-10 23:47 UTC) was answered with a Proton CAPTCHA, as a new client usually is. That token is about 40 hours old on 2026-11-12 and Proton will almost certainly refuse it (E02 measures exactly that limit), so it is **not reused**: a login on it would burn a login and ask for a second solve anyway. The author is asked for one fresh solve at the moment it is needed.

1. Agent (login 1 of the session; `<run>` and the build as in "The run root"): a login without token, which ends in a CAPTCHA (exit 10, `hv.json` in the lab dir):
   `live --lab 2a --login 2a 'challenge for the Other login' -- pass-cli run --env-file scripts/lab.env.example -- pcb-lab login --state-dir @LAB@`
   Not in CAPTCHA but exit 0: no CAPTCHA this time; record it for E02, and skip to 2b (2a is then logged in).
2. Agent shows the verify URL (the token in it is a CAPTCHA challenge, not an account secret), reading the file of that run: `python3 -c 'import json,os,sys;print(json.load(open(os.path.expanduser(sys.argv[1])))["url"])' ~/pcb-live-run/<run>/lab/2a/hv.json`, and says so to the author.
3. **Author, at once**: open the URL in any browser, solve the CAPTCHA, close the tab, tell the agent "captcha solved at HH:MM". The agent needs it within about a minute of showing the URL (the 60 s between logins is waiting anyway); the age of the token at its use is the E02 data point.
4. The agent continues alone with step 2a (login 2). If Proton refuses this token too (a new HV at 2a), the agent starts one more fresh login, the author solves the new URL right away, and the agent resumes within a minute (protocol in `E02-captcha-token.md`); that is login 3 and a new E02 row.

## 2. Agent: pre-check, then the E03/E02/E01 login sequence

Pre-check first: `PROTON_PASS_AGENT_REASON="<why>" pass-cli info` must succeed. If not, log pass-cli in again with the PAT file `~/.config/pcb-dev/pass-pat`, passed through the environment variable `PROTON_PASS_PERSONAL_ACCESS_TOKEN` (never argv), then repeat the check. The binaries are already built into `$R/bin` (see "The run root": `go build -trimpath -o "$R/bin/pcb-lab" ./cmd/pcb-lab`, no worktree, nothing in `~/.local/bin`). Commands and result tables: `E03-client-identity.md` ("Pending live run"), where E03's steps 1 to 4 are 2a to 2d below and E03's step 5 (E01's dedicated login) is step 3 here. The login log (the ledger) is kept in that E03 section, as E02 and E03 expect.

**Each login has its own state dir** (triage G2: logged in to one default dir, 2b to 2d would replace 2a's session and the later probes would resume the negative control's). Each login is one call, e.g. 2b:
`live --lab 2b --login 2b 'linux-mail identity' -- env PCB_LAB_APPVERSION=linux-mail@1.0.0 pass-cli run --env-file scripts/lab.env.example -- pcb-lab login --state-dir @LAB@`
and each probe after it names the same dir and the same `PCB_LAB_APPVERSION` (none for 2a, which is `Other`), always one call: `live --lab 2b -- env PCB_LAB_APPVERSION=linux-mail@1.0.0 pcb-lab probe calendar-access --state-dir @LAB@`.

| # | Login (state dir, identity) | Then, on that session | Experiment |
|---|---|---|---|
| 2a | `Other`, `lab/2a`, no `PCB_LAB_APPVERSION`, with the token the author just solved (step 1: `--captcha-token <token from lab/2a/hv.json>`) | `whoami --json` (scope list); `probe calendar-access`; **`probe refresh` once** (never again on this session) | E02 (token accepted in a new process, age at retry), E03 (`Other`), E01 (immediate results: `ExpiresIn`, rotation, old-token refusal) |
| 2b | `linux-mail@1.0.0`, `lab/2b`, +60 s | `probe calendar-access` | E03 |
| 2c | `web-calendar@5.0.131.2`, `lab/2c`, +60 s | `probe calendar-access` | E03 |
| 2d | `external-drive-caloxide@0.2.1-stable`, `lab/2d`, +60 s (negative control) | `probe calendar-access` | E03 |

**The calendar-access lab session.** Every later `pcb-lab probe` (M1.5, M2.4, 6.1.4) resumes the dir of the candidate that E03 showed to have calendar access, `lab/<w>`: `lab/2a` with no `PCB_LAB_APPVERSION` if `Other` works, else that candidate's dir and its `PCB_LAB_APPVERSION`. Write `<w>` and its identity in the E03 results; the later steps below say "the calendar-access lab session". The negative control's dir (`lab/2d`) is never used for them.

Then fill the E03 results table and decide: set decision 0011 final (or revise it) and turn SPEC §5.1 to `[verified: E03]`; fill E02 row 1 and the E01 "Results" immediate part. Raw redacted recordings go to `spec/experiments/raw/E03/` and `raw/E01/` (redaction by `internal/lab/redact` only).

## 3. Agent: E01 long probe (its own dedicated session)

At least 60 s after 2d (the ledger enforces it). Nothing else ever uses this state dir, `lab/e01`. Commands in `E01-session-lifetime.md`, with the dir and binary of the run root instead of its `~/.local/state/pcb-lab-e01` and `~/.local/bin/pcb-lab`:
- `live --lab e01 --login 3 'E01 long probe' -- pass-cli run --env-file scripts/lab.env.example -- pcb-lab login --state-dir @LAB@`, then the first sample `live --lab e01 -- pcb-lab probe alive --state-dir @LAB@`;
- the user timer `pcb-lab-e01.timer` (every 6 h, `Persistent=true`) with the unit files generated for the run root (the shipped `.service` hard-codes the two old paths): `mkdir -p "$R/e01" && sed -e "s|^ExecStart=.*|ExecStart=$R/bin/pcb-lab probe alive --state-dir $R/lab/e01|" scripts/e01-timer/pcb-lab-e01.service > "$R/e01/pcb-lab-e01.service" && cp scripts/e01-timer/pcb-lab-e01.timer "$R/e01/"`, then `systemctl --user link "$R/e01/pcb-lab-e01.service" "$R/e01/pcb-lab-e01.timer" && systemctl --user enable --now pcb-lab-e01.timer` (it only links two units named `pcb-lab-e01`; it touches no pcb dir).

**The run root stays for the 14 days** (the timer reads `lab/e01` and `$R/bin/pcb-lab`): the close-out removes `$R/pcb` and the other lab dirs, not `lab/e01` or `bin/`. It samples for 14 days; afterwards the agent copies `lab/e01/e01.jsonl` to `raw/E01/alive.jsonl`, fills the E01 results (age and code when the session ended, or "survived 14 days of 6-hourly use": the probe refreshes only when the access token is rejected, not every 6 h), uninstalls the timer (`systemctl --user disable --now pcb-lab-e01.timer`, then `systemctl --user unlink` of the two units) and may remove the run root. E01's questions 1 to 3 are answered by step 2a; question 4 only after the 14 days.

## 4. Agent: seed the synthetic data set

At least 60 s after step 3's login. Commands and the wipe/reseed notes: `DATASET.md` ("Pending live run"): build the fork's `origin/main` as `proton-cal` into `$R/bin`, `proton-cal login --no-prompt` through `live --proton-cal` (config dir `$R/seed`, never the author's own `~/.config/proton-cal`) with `scripts/seed.env.example` (this login counts as a lab login, `--login 4 'seed'`), `pcb-lab seed plan`, then `pcb-lab seed apply --config-dir @RUN@/seed` (about 370 writes, one per 2 s, about 12 minutes; stops on the first rate limit with exit 40, rerun resumes). Record the final counts in `DATASET.md`.

## 5. After the run (agent; the last step, after the M1.8 close-out)

Update `E01`, `E02`, `E03`, `DATASET.md`, SPEC §5.1/§5.2 markers, `spec/OPEN.md`, commit on a branch. The E01 14-day window keeps running after this; its close-out is a separate, later commit.

## 6. Thunderbird and the M4 live session (agent, then one author sitting; after the M1 and M2 live sessions)

M3 (CalDAV read) and M4 (CalDAV writes) are built and tested against the fake Proton only. This step proves both against the real account in one session (ruling M3-R1: nobody subscribes Thunderbird before writes work). It runs **after the M1 and M2 live sessions**: `pcb` must already be logged in to the test account with a filled cache.

Order, chosen so the author acts once:
1. **6.1** the agent prepares (no author);
2. **6.2** the agent runs every write check it can make over the wire with `curl` (no author);
3. **6.3** one author sitting, Thunderbird and the web app side by side (the agent watches the logs and runs the checks after each author action);
4. **6.4** the agent's checks after the sitting (no author);
5. **6.5** what to record; **6.6** clean up.

**Rules for this step** (on top of "Rules for the whole run"):
- **Test account only** (vault `pcb-dev`, item `proton-calendar-bridge-test`), never the author's own account: before anything, `live -- pcb status --json | jq -r .data.username` must print the test account's username (compare by eye; do not record it). Nothing here writes to any other account.
- **No new login.** `pcb` reuses the session of the M1/M2 live sessions, in their isolated dirs (`$R/pcb/config`, `$R/pcb/state`; every `live -- pcb ...` below uses them by default). If that session is gone, one login per M1.2 is allowed (at least 60 s after the last login, CAPTCHA rules of M1.2), and counts in the ledger (`--login`).
- Every event the session creates has a title starting `pcb-m4` and a UID starting `pcb-m4-`, dated January–February 2027, so it can be found and removed.
- **Never provoke a 429** (no loops, no bursts). Each `curl` below is one request; run them one at a time.
- Stop conditions: those of the whole run, plus: the owned default calendar is not writable at 6.1.4 (do the diagnosis there, then stop the write part); any write answers 5xx twice in a row; any `pcb serve` log line `refusing a write: a row's key packet changed` (record it, stop: R17/R30 must be looked at before more writes).
- Every bash call that uses `$R`, `$DB` or `$LIVE` starts with the `R=...; DB=...; LIVE=...` line of 6.1.1 (agent calls share no variables). `curl` and `sqlite3 -readonly` touch only localhost and `$R/pcb/state`.
- Nothing decrypted goes into the repository except the test events' own lines written below; key packets, signatures and ciphertext are compared, never copied into a write-up.

**6.1 Prepare (agent).**
1. Point at the M1/M2 dirs. `pcb` is the run-root build (`live` puts `$R/bin` first), the dirs are `$R/pcb/*` through `live`; the helper variables below are plain shell, in **bash**, and hold only paths of the run root (a variable lost between calls cannot reach any other dir: `live` sets the pcb dirs itself):
   ```sh
   R=$HOME/pcb-live-run/<run>; DB=$R/pcb/state/pcb.db; LIVE=$R/m4-live   # PCB_STATE_DIR is used as is: the cache is pcb.db in it
   (umask 077; mkdir -p "$LIVE")
   live -- pcb status --json | jq '.data | {phase, username, unattended}'   # phase "ready"; username: the test account
   live -- pcb sync --json | jq '.data.calendars[] | {name, mode, error}'   # exit 0, no error
   ```
2. Two CalDAV clients: one for Thunderbird, one for the agent's `curl` (its password goes to a 0600 netrc file, never into argv):
   ```sh
   live -- pcb client add Thunderbird        # prints the password once: hand it to the author out of band
   (umask 077; live -- pcb client add m4-curl --json | jq -r '"machine localhost login me password " + .data.password' > "$LIVE/netrc")
   ```
3. Start the bridge in a second terminal, in bash, logs kept (the `R`, `DB` and `LIVE` line of 1. is repeated there; `live` itself sets the pcb dirs):
   ```sh
   R=$HOME/pcb-live-run/<run>; LIVE=$R/m4-live
   live -- pcb serve 2> >(tee "$LIVE/serve.log" >&2)
   ```
   Expect on stdout `Thunderbird URL: http://localhost:1080/`; it refuses to start logged out (exit 10) or while `pcb sync` runs (exit 20). It listens on the loopback only, as configured.
4. The write gate (E14, R17/E10). `live -- pcb calendar list --json > "$LIVE/calendars.json"; jq '.data.calendars[] | {name, type, default, read_only, keys}' "$LIVE/calendars.json"`. Expect the default calendar `type` `normal`, `read_only` **false**, `keys` `ok`; holidays and subscribed calendars `read_only` true. If the default calendar is `read_only` true, diagnose before stopping (only if the calendar-access lab session of step 2 is still alive; never log in for it):
   ```sh
   live --lab <w> -- env PCB_LAB_APPVERSION=<its, none for 2a> pcb-lab probe calendar-access --state-dir @LAB@ --record-dir @RUN@/m4-live/lab   # the calendar-access lab session of step 2
   jq '.body.Calendars[] | {Type, Permissions: [.Members[].Permissions]}' "$LIVE"/lab/*calendar-list.json
   jq '[.body.Passphrase.MemberPassphrases[].Signature]' "$LIVE"/lab/*calendar-bootstrap.json   # first calendar only; the probe reads the v1 bootstrap, assumed to carry the same field as the v2 one pcb reads
   ```
   Read it as: a `Permissions` value without the 8 bit → E14's bit is wrong; 8 set and a signature `<redacted:len=N>` with N > 0 → the signature does not verify with our address key (R17's assumption is wrong, E10); no signature (empty) → the gate stays undecided and every write would be 503. Record, then stop the write part (6.2, 6.3 writes); the read part of 6.3 can still run.
5. Variables and helpers for 6.2 (bash, in the same call as the commands that use them, since variables do not survive between agent calls; the `R`, `DB`, `LIVE` line of 1. goes first; `BASE` is the default calendar's CalDAV URL, its path segment being the first 16 hex digits of the SHA-256 of the calendar ID):
   ```sh
   CAL_ID=$(jq -r '.data.calendars[] | select(.default) | .id' "$LIVE/calendars.json")
   BASE=http://localhost:1080/dav/calendars/me/$(printf %s "$CAL_ID" | sha256sum | cut -c1-16)
   C=(curl -s --netrc-file "$LIVE/netrc")
   # ev FILE UID SUMMARY DTSTART-LINE DTEND-LINE [more lines]: one VEVENT body, CRLF
   ev() { local f=$1 uid=$2 sum=$3 s=$4 e=$5; shift 5
     { printf 'BEGIN:VCALENDAR\nVERSION:2.0\nPRODID:-//pcb m4 live//EN\nBEGIN:VEVENT\nUID:%s\nDTSTAMP:20261201T000000Z\nSUMMARY:%s\n%s\n%s\nSEQUENCE:0\n' "$uid" "$sum" "$s" "$e"
       for l in "$@"; do printf '%s\n' "$l"; done; printf 'END:VEVENT\nEND:VCALENDAR\n'; } | sed 's/$/\r/' > "$LIVE/$f"; }
   ics() { sed 's/$/\r/' > "$LIVE/$1"; }   # ics FILE <<'EOF' … EOF: a free-form body, CRLF
   put() { "${C[@]}" -o "$LIVE/out.xml" -w '%{http_code}\n' -X PUT -H 'Content-Type: text/calendar; charset=utf-8' --data-binary @"$LIVE/$2" "${@:3}" "$BASE/$1"; cat "$LIVE/out.xml"; echo; }
   del() { "${C[@]}" -o /dev/null -w '%{http_code}\n' -X DELETE "${@:2}" "$BASE/$1"; }
   get() { "${C[@]}" "$BASE/$1" | tr -d '\r'; }
   etag() { "${C[@]}" -o /dev/null -D - "$BASE/$1" | tr -d '\r' | sed -n 's/^[Ee][Tt][Aa][Gg]: //p'; }
   rows() { sqlite3 -readonly "$DB" "select substr(id,1,10), etag, seq, recurrence_id from events where calendar_id='$CAL_ID' and uid='$1' order by recurrence_id"; }
   raw() { sqlite3 -readonly "$DB" "select cast(raw as text) from events where calendar_id='$CAL_ID' and uid='$1' and recurrence_id is null"; }
   newest() { sqlite3 -readonly "$DB" "select uid, rrule from events where calendar_id='$CAL_ID' order by last_edit desc limit ${1:-3}"; }   # the UIDs of the latest edits
   AL=(BEGIN:VALARM ACTION:DISPLAY TRIGGER:-PT15M DESCRIPTION:r END:VALARM)
   ```
   Check: `"${C[@]}" -o /dev/null -w '%{http_code}\n' -X PROPFIND -H 'Depth: 0' "$BASE/"` prints `207`; `"${C[@]}" -X PROPFIND -H 'Depth: 0' -H 'Content-Type: text/xml' --data '<D:propfind xmlns:D="DAV:"><D:prop><D:current-user-privilege-set/></D:prop></D:propfind>' "$BASE/"` contains `<D:privilege><D:write/></D:privilege>`.

**6.2 Writes over the wire (agent alone).** Each line: the command, then what to expect. After each 2xx write, `grep -c 'not confirmed\|refusing a write' "$LIVE/serve.log"` must stay 0 unless the step says otherwise; a refused write (403, 409, 412) also logs a `refused a CalDAV write` line (the same code and href at most once per 10 minutes) (method, status, code, calendar, href, reason), which a refusal step expects. Results go to the experiment named in brackets.

*W1. Create, read back, signatures, key packets* [L.4 #1, E10, R32, `?UID=`]:
```sh
ev basic.ics pcb-m4-basic 'pcb-m4 basic' 'DTSTART;TZID=Europe/Paris:20270104T100000' 'DTEND;TZID=Europe/Paris:20270104T110000' 'DESCRIPTION:first line' 'LOCATION:Room 1' "${AL[@]}"
put pcb-m4-basic.ics basic.ics -H 'If-None-Match: *'   # 201, empty body
get pcb-m4-basic.ics          # SUMMARY:pcb-m4 basic, DESCRIPTION, LOCATION, SEQUENCE:0, one VALARM with TRIGGER:-PT15M, no VTIMEZONE
rows pcb-m4-basic             # one row, recurrence_id empty
raw pcb-m4-basic | jq -r '.SharedEvents[] | select(.Type==2) | .Data'      # pcb's fragment: BEGIN:VCALENDAR, BEGIN:VEVENT, then UID, DTSTAMP, DTSTART, DTEND, SEQUENCE and nothing else (no SUMMARY, VERSION or PRODID), END:VEVENT, END:VCALENDAR
raw pcb-m4-basic | jq -r '.SharedEvents[] | select(.Type==2) | .Signature' | gpg --list-packets 2>/dev/null | grep -o 'sigclass 0x0[01]'   # sigclass 0x00 (binary)
raw pcb-m4-basic | jq -r '.SharedKeyPacket' | base64 -d | gpg --list-packets 2>/dev/null | grep -c ':pubkey enc packet:'                # 1 (primary calendar key only)
raw pcb-m4-basic | jq -r '[(.SharedEvents // [])[], (.CalendarEvents // [])[]] | map(select(.Type==3) | (.Data | test("^[A-Za-z0-9+/]+=*$"))) | all'  # true: one line of standard base64, as pcb sent it
```
Then `grep 'rows of another UID' "$LIVE/serve.log"`: no line means `?UID=` filtered exactly. Rows of other UIDs in the answer are ignored (M4-R45), so the write succeeds either way; a line (`Proton listed rows of another UID for a write's UID: they are ignored`, with their row IDs) means Proton does not filter exactly: record it. A confirmed write also proves the signed parts came back byte for byte (their signatures verify over exact bytes); an encrypted `Data` other than one line of standard base64 means Proton re-encoded it (harmless, R32: the confirmation compares plaintext).

*W2. The ListByUID row equals the feed row* [Task 6 live check, L.4 #11]: note `rows pcb-m4-basic` (etag, seq), wait 70 s (two `pcb serve` passes, no edit), `rows pcb-m4-basic` again: **same etag and seq**. A new seq with no edit means the row Proton lists by UID differs from the one it serves by ID: every write would then churn a second change (record which fields differ: `raw` before and after, `jq -S . | diff`). Optional: run `live --lab <w> -- env PCB_LAB_APPVERSION=<its, none for 2a> pcb-lab probe feed --state-dir @LAB@ --calendar "$CAL_ID" --wait 2m` (the calendar-access lab session of step 2) during W1 to time the feed entry (L.4 #11, E04).

*W3. Exact UID match* [`?UID=`]: `ev basic2.ics pcb-m4-basic-2 'pcb-m4 basic 2' 'DTSTART;TZID=Europe/Paris:20270105T100000' 'DTEND;TZID=Europe/Paris:20270105T110000'; put pcb-m4-basic-2.ics basic2.ics -H 'If-None-Match: *'` → 201; then W4 edits `pcb-m4-basic`: no `rows of another UID` warning in `serve.log` for that write shows Proton does not match `pcb-m4-basic` as a prefix of `pcb-m4-basic-2` (a prefix match logs the warning, as in W1; the write succeeds either way).

*W4. Text-only edit without a SEQUENCE bump* [L.4 #2, R4, decisive]:
```sh
sed -i 's/^SUMMARY:pcb-m4 basic\r$/SUMMARY:pcb-m4 basic renamed\r/' "$LIVE/basic.ics"
put pcb-m4-basic.ics basic.ics -H "If-Match: $(etag pcb-m4-basic.ics)"   # 204
get pcb-m4-basic.ics | grep -E '^(SUMMARY|SEQUENCE)'                      # SUMMARY:pcb-m4 basic renamed, SEQUENCE:0
```
If instead 503 with `a write is not confirmed by the rows read back` in the log and the old title in `get`: Proton dropped the unbumped edit (carbonate's and caloxide's reading of research A.4). Record it; R4 must change (bump on every edit) before Thunderbird edits titles (code change, test first); skip W5–W6 and every text edit of 6.3.

*W5. Time edit, the client does not bump* [R4]: `sed -i 's/T100000\r$/T103000\r/; s/T110000\r$/T113000\r/' "$LIVE/basic.ics"; put pcb-m4-basic.ics basic.ics -H "If-Match: $(etag pcb-m4-basic.ics)"` → 204; `get pcb-m4-basic.ics | grep -E '^(DTSTART|SEQUENCE)'` → `DTSTART;TZID=Europe/Paris:20270104T103000`, `SEQUENCE:1` (pcb bumped it). pcb never sends an unbumped time edit, so whether Proton drops one is not reachable with these tools; if W4 showed text edits kept, record "unbumped time edit: not tested (pcb bumps)".

*W6. Alarm-only edits* [research D, tri-state]: `sed -i 's/TRIGGER:-PT15M/TRIGGER:-PT30M/' "$LIVE/basic.ics"`, PUT with If-Match → 204, `get` shows `TRIGGER:-PT30M`, `raw pcb-m4-basic | jq -c .Notifications` → `[{"Type":1,"Trigger":"-PT30M"}]`, SEQUENCE still 1. Then remove the five VALARM lines (`sed -i '/VALARM\|ACTION:\|TRIGGER:\|DESCRIPTION:r/d'`), PUT → 204, `get` has no VALARM, `.Notifications` is `[]`.

*W7. Unknown and extra properties* [L.4 #7, E13]:
```sh
ev props.ics pcb-m4-props 'pcb-m4 props' 'DTSTART;TZID=Europe/Paris:20270105T140000' 'DTEND;TZID=Europe/Paris:20270105T150000' \
  'X-MOZ-GENERATION:3' 'X-MOZ-LASTACK:20261201T000000Z' 'X-PCB-M4-TEST;X-P=1:kept' 'CLASS:PRIVATE' 'CATEGORIES:pcb,m4' \
  'URL:https://example.org/pcb-m4' 'ATTACH:https://example.org/pcb-m4.txt' 'GEO:48.85;2.35' 'PRIORITY:5' 'RESOURCES:Projector' \
  'CONTACT:pcb test' 'RELATED-TO:pcb-m4-basic' 'COMMENT:calendar comment' 'TRANSP:TRANSPARENT' 'STATUS:TENTATIVE' 'COLOR:red'
put pcb-m4-props.ics props.ics -H 'If-None-Match: *'   # 201
get pcb-m4-props.ics   # every line above back byte for byte (in part order, not the order sent), except: X-PCB-M4-TEST (pcb strips X-PCB-*), STATUS:TENTATIVE (dropped), COLOR (ignored)
raw pcb-m4-props | jq -r '.CalendarEvents[] | "\(.Type) \(.Data|.[0:30])"'   # a Type 2 part (UID, DTSTAMP, TRANSP) and a Type 3 part (COMMENT)
raw pcb-m4-props | jq -r '.SharedEvents[] | select(.Type==2) | .Data'         # still only UID, DTSTAMP, DTSTART, DTEND, SEQUENCE
```
`X-PCB-M4-TEST` is the bridge's own stripping rule, not Proton's; the E13 question is about the others. 6.3 C1 edits this event in the web app; 6.4.1 reads what survives.

*W8. Recurrence forms* [L.4 #6, E12]. Each a create with `-H 'If-None-Match: *'`, then `get NAME | grep RRULE`, `rows UID`:

| Name / UID | DTSTART–DTEND (Europe/Paris) | RRULE sent | Expect |
|---|---|---|---|
| `pcb-m4-weekdays` | 20270104T090000–T093000 (Mon) | `FREQ=DAILY;BYDAY=MO,TU,WE,TH,FR;COUNT=10` | 201, served `FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR;COUNT=10` (R12) |
| `pcb-m4-2tu` | 20270112T140000–T150000 (2nd Tue) | `FREQ=MONTHLY;BYDAY=2TU;COUNT=6` | 201, served `FREQ=MONTHLY;BYDAY=TU;BYSETPOS=2;COUNT=6` (R5) |
| `pcb-m4-lastfr` | 20270129T160000–T170000 (last Fri) | `FREQ=MONTHLY;BYDAY=-1FR;COUNT=3` | 201, served `FREQ=MONTHLY;BYDAY=FR;BYSETPOS=-1;COUNT=3` |
| `pcb-m4-notfirst` | 20270106T120000–T130000 (Wed) | `FREQ=WEEKLY;BYDAY=MO;COUNT=3` | pcb does not check that DTSTART is an occurrence: record Proton's answer (201, or 403 `pcb:proton-refused` with a code) and, in 6.3, what the web app and Thunderbird show |
| `pcb-m4-count49` | 20270201T080000–T081500 | `FREQ=WEEKLY;COUNT=49` | 201 |
| `pcb-m4-until` | all-day `DTSTART;VALUE=DATE:20270202`, `DTEND;VALUE=DATE:20270203` | `FREQ=YEARLY;UNTIL=20371231` | 201 |
| `pcb-m4-duration` [L.4 #8] | 20270108T090000, and `DURATION:PT45M` passed in place of the DTEND line | none | 201, served `DTEND;TZID=Europe/Paris:20270108T094500` and no DURATION |

e.g. `ev weekdays.ics pcb-m4-weekdays 'pcb-m4 weekdays' 'DTSTART;TZID=Europe/Paris:20270104T090000' 'DTEND;TZID=Europe/Paris:20270104T093000' 'RRULE:FREQ=DAILY;BYDAY=MO,TU,WE,TH,FR;COUNT=10'; put pcb-m4-weekdays.ics weekdays.ics -H 'If-None-Match: *'`.

Refused by pcb, no write reaches Proton. Make each with `ev refused.ics pcb-m4-refused 'pcb-m4 refused' 'DTSTART;TZID=Europe/Paris:20270104T100000' 'DTEND;TZID=Europe/Paris:20270104T110000' '<the line>'` (the floating and unknown-TZID cases replace the DTSTART line instead and add no line) and `put pcb-m4-refused.ics refused.ics -H 'If-None-Match: *'`; each answers 403 with its body, and `rows pcb-m4-refused` stays empty. `RRULE:FREQ=WEEKLY;COUNT=50` → `<D:error xmlns:D="DAV:"><pcb:unsupported-recurrence xmlns:pcb="urn:pcb"/><pcb:reason xmlns:pcb="urn:pcb">RRULE COUNT above 49</pcb:reason></D:error>`; `RRULE:FREQ=HOURLY` → `unsupported-recurrence`; `RRULE:FREQ=MONTHLY;BYDAY=TU;BYSETPOS=5` → `unsupported-recurrence`; an `RDATE;TZID=Europe/Paris:20270110T100000` line → `pcb:rdate`; DTSTART `DTSTART:20270104T100000` (floating) → `pcb:floating-time`; `DTSTART;TZID=Mars/Olympus:20270104T100000` → `pcb:unknown-tzid`; an `ATTENDEE:mailto:pcb-m4-attendee@example.org` line → `pcb:attendee-change` (refused after the refresh, a GET; the body-level ones above are refused before any request). Whether **Proton** enforces these limits itself (COUNT 50, UNTIL 2038, BYSETPOS 5, HOURLY, INTERVAL bounds) is not reachable through pcb, which refuses first: record "not tested (the bridge gatekeeps)".

*W9. A series with exceptions, mixed batches* [L.4 #4, E12, research E.2]. Write `series.ics` with the master only:
```sh
ics series.ics <<'EOF'
BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//pcb m4 live//EN
BEGIN:VEVENT
UID:pcb-m4-series
DTSTAMP:20261201T000000Z
SUMMARY:pcb-m4 series
DTSTART;TZID=Europe/Paris:20270118T080000
DTEND;TZID=Europe/Paris:20270118T083000
RRULE:FREQ=DAILY;COUNT=5
SEQUENCE:0
END:VEVENT
END:VCALENDAR
EOF
put pcb-m4-series.ics series.ics -H 'If-None-Match: *'   # 201; rows: 1
```
Then, each with `-H "If-Match: $(etag pcb-m4-series.ics)"`, edit `series.ics` (an exception VEVENT goes before `END:VCALENDAR`):
1. Add an exception for the 3rd occurrence (`RECURRENCE-ID;TZID=Europe/Paris:20270120T080000`, DTSTART/DTEND 09:00–09:30, `SUMMARY:pcb-m4 series moved`, same UID, `SEQUENCE:0`) → 204, rows: 2 (one with a recurrence_id), `get` shows both VEVENTs and the exception's `SEQUENCE` at least the master's. (One create.)
2. Add `EXDATE;TZID=Europe/Paris:20270121T080000` to the master → 204, rows: 2; `get` shows the EXDATE and the master at `SEQUENCE:1` (bumped: shared signed change), the exception at `SEQUENCE:1` too (raised to the master's); `raw pcb-m4-series | jq -r '.CalendarEvents[] | select(.Type==2) | .Data'` holds the EXDATE (both signed parts). (Two updates in one call.)
3. Remove the exception VEVENT and add `EXDATE;TZID=Europe/Paris:20270120T080000` to the master → 204, rows: 1, master `SEQUENCE:2`. (Update + delete in **one** call.)
4. Add an exception for the 5th occurrence (`…20270122T080000`, 09:00–09:30) → 204, rows 2, its `SEQUENCE:2`. Then, in one PUT: rename the master `SUMMARY:pcb-m4 series renamed`, remove the 5th-occurrence exception, add one for the 2nd (`RECURRENCE-ID;TZID=Europe/Paris:20270119T080000`, 09:00–09:30) → 204 (update + create + delete in one call), rows 2, master still `SEQUENCE:2` (a text edit). If Proton refuses a mixed batch, or one entry of it: 403 `pcb:proton-refused` (code in `serve.log`'s `Proton refused a write … code=`), also when other entries of the call landed: Proton then keeps those (pcb undoes nothing) and `rows`, `get` show them; record which entries it applied. A 412 here means the read-back saw a change that none of the applied entries explains (Proton applied part of a call it refused as a whole): record it too. Never move a master's time in this check: Thunderbird rewrites RECURRENCE-IDs and EXDATEs when it does, which hand-written bodies would not.

*W10. Escaped UID* [UID escaping, SPEC §5.3]: `ev esc.ics 'pcb-m4-a\,b' 'pcb-m4 escaped' 'DTSTART;TZID=Europe/Paris:20270107T100000' 'DTEND;TZID=Europe/Paris:20270107T110000'; put pcb-m4-escaped.ics esc.ics -H 'If-None-Match: *'`. Then `sqlite3 -readonly "$DB" "select uid from events where uid like 'pcb-m4-a%'"`:
- 201 and the column is `pcb-m4-a\,b`: Proton keeps the escaped text; nothing to change.
- 503 (`not confirmed`), and after the next pass the column is `pcb-m4-a,b`: Proton unescapes. The event exists in Proton but pcb cannot confirm or edit it (its write check binds to the UID as written). Record; the full fix (compare unescaped UIDs in `ical.ProvesKey`, the planner and `ListByUID`) is a code change. Since the audit fix write-I2, an edit PUT of that event at its own name answers 503 `the event's UID is stored by Proton in another form than its text` (not 409, not `not confirmed`), and the same UID PUT at another name answers 409 naming the object. The event is served at a `~`-encoded name: delete it in the web app at 6.6.

*W11. Names, conflicts, preconditions (no Proton write)* [E08 MUST 9–11]:
```sh
ev alias.ics pcb-m4-alias@example.org 'pcb-m4 alias' 'DTSTART;TZID=Europe/Paris:20270108T100000' 'DTEND;TZID=Europe/Paris:20270108T110000'
put pcb-m4-alias_example.org.ics alias.ics -H 'If-None-Match: *'   # 201 (Thunderbird's sanitised name, kept as an alias)
"${C[@]}" -o /dev/null -w '%{http_code}\n' "$BASE/pcb-m4-alias@example.org.ics"   # 404: an aliased UID is served at its alias only
put pcb-m4-alias_example.org.ics alias.ics -H 'If-None-Match: *'   # 412
put other-name.ics alias.ics                                       # 409, <C:no-uid-conflict><D:href>…/pcb-m4-alias_example.org.ics</D:href>
put pcb-m4-props.ics props.ics -H 'If-Match: "v1-stale"'          # 412
del pcb-m4-nothing.ics                                             # 404
```

*W12. Delete* [L.4 #1]: `del pcb-m4-basic-2.ics -H "If-Match: $(etag pcb-m4-basic-2.ics)"` → 204; `rows pcb-m4-basic-2` empty; `get pcb-m4-basic-2.ics` empty (404).

*Not reachable here* (pcb never sends these; record "not tested", see `OPEN.md`, "Still open, later"): L.4 #10 (what Proton does to a personal part an update leaves out: since the audit fix write-I3 pcb refuses to update a row that holds one, so this needs a `pcb-lab` probe that sends such an update on the test account, as for L.4 #3; D.4 below only reaches it for a row with `P` > 0), L.4 #3 (new key packets on an update), L.4 #5 (a second master with an existing UID), and the server-side halves of L.4 #6 (does Proton itself enforce the RRULE limits) and #8 (STATUS:TENTATIVE's 2000, floating times, non-IANA TZIDs, DURATION as sent).

*W13. 429* [E06]: do not provoke one. If any 429 shows anywhere in the session, record the request, Proton's `Retry-After`, and what pcb answered (a write: 503 with Proton's `Retry-After`, never retried; a sync pass: its backoff).

**6.3 The author sitting (Thunderbird and the web app; the agent watches `serve.log` and runs the check after each item).** Use a fresh Thunderbird profile, so no earlier calendar or certificate exception interferes. The agent notes the UTC time of each item. If Thunderbird offers to send invitations or updates, always choose not to send.

*A. Subscribe (local, plain HTTP).* Calendar > New Calendar > On the Network. Location `http://localhost:1080/`, username anything, the Thunderbird password from 6.1, "Find Calendars". Expect every calendar of the test account, each with its name and colour; subscribe to all. Before the dialog the agent checks, with the `m4-curl` netrc: `"${C[@]}" -si http://localhost:1080/.well-known/caldav | head -3` (308, `Location: /dav/principals/me/`), `"${C[@]}" -si -X PROPFIND -H 'Depth: 0' http://localhost:1080/ | head -1` (`HTTP/1.1 207`), and without credentials `curl -si -X PROPFIND http://localhost:1080/ | head -1` (`HTTP/1.1 401`, `WWW-Authenticate: Basic realm="pcb"`). Then repeat the dialog once with `http://localhost:1080/.well-known/caldav`: same calendars found. Thunderbird must show the default calendar writable and holidays/subscribed ones read only (calendar properties, "Read Only").

*B. Look (no edit).* In Thunderbird and in the web app, side by side, check each event of 6.2 and say what differs:
1. `pcb-m4 basic renamed` on Mon 4 Jan 2027 10:30–11:30, with its description and location, no reminder; `pcb-m4 props` on Tue 5 Jan 14:00–15:00, shown as free (TRANSP); in the web app, both open without an error.
2. `pcb-m4 weekdays`: 10 occurrences, 4–15 Jan, weekdays only; `pcb-m4 2tu`: second Tuesdays from 12 Jan, 6 times; `pcb-m4 lastfr`: last Fridays from 29 Jan, 3 times; `pcb-m4 count49`, `pcb-m4 until`: shown (open one occurrence far out).
3. `pcb-m4 notfirst`: which days each app shows (Wed 6 Jan and/or Mondays 11, 18, 25 Jan).
4. `pcb-m4 series renamed`: 18 Jan 08:00, 19 Jan 09:00 (the exception), 20 and 21 Jan missing, 22 Jan 08:00.
5. `pcb-m4 alias`, and (if W10 gave 201) `pcb-m4 escaped`.
Any event the web app cannot open or shows wrongly is an L.4 #1 failure: record it with the time.

*C. Web-app edits (on the test account).*
1. Rename `pcb-m4 props` to `pcb-m4 props web` (E13: what Proton's own client keeps; 6.4 reads it).
2. Create `pcb-m4 web defaults` (Fri 8 Jan 2027 14:00–15:00), leaving the calendar's default notifications as they are.
3. Create `pcb-m4 meeting`, daily, 3 times from Mon 11 Jan 2027 10:00–10:30, with one participant `pcb-m4-attendee@example.org` (a reserved domain: nothing is delivered).
4. Delete `pcb-m4 lastfr`.
The agent then runs, after one `pcb serve` pass (30 s): Thunderbird right-click > Synchronize Calendars; expect the renamed event, the two new ones and `lastfr` gone. Agent checks: `raw <uid> | jq -c '{Notifications, P: (.PersonalEvents // [] | length)}'` for `pcb-m4 web defaults` (its UID: `newest 4` lists the latest edits; it is the one without RRULE that is not `pcb-m4-props`; `get <uid>.ics | grep SUMMARY` confirms): expect `Notifications` `null` (defaults); note `P`.

*D. Thunderbird writes.* After each item the agent checks `serve.log` (no `not confirmed`, no `refusing`), and the author checks the web app (refresh the page).
1. New event `pcb-m4 tb new`, Tue 5 Jan 2027 16:00–17:00, reminder 15 minutes before. Expect it in the web app with a 15-minute notification. Agent: its UID (`newest 1`), `rows <uid>`, and `sqlite3 -readonly "$DB" "select name, uid from aliases where calendar_id='$CAL_ID'"` shows no alias for it (Thunderbird's name `<uuid>.ics` is the canonical one).
2. Rename it `pcb-m4 tb renamed` → web app shows it (an unbumped text edit, as W4).
3. Drag it to 17:00–18:00 → web app shows the new time; agent: `get <uid>.ics | grep SEQUENCE` → bumped.
4. Rename `pcb-m4 web defaults` to `pcb-m4 web defaults tb` → agent: its `Notifications` still `null` (`TestUnchangedDefaultAlarmsKeepNull` live), and `P` compared with its value at C (L.4 #10: what a sync update that sends no personal part does to one). Since the audit fix write-I3, pcb refuses the update of a row that holds a personal part (403 `pcb:personal-part`, `serve.log`: `refused a CalDAV write`): with `P` > 0 at C this rename is refused, which is expected, and L.4 #10 needs a lab probe instead; once L.4 #10 shows Proton keeps an omitted personal part (or pcb re-sends it), the refusal is lifted (`RefusePersonalPart` in `internal/service/writeplan.go`).
5. New event `pcb-m4 tb weekdays`, Mon 1 Feb 2027 09:00–09:30, Repeat: every weekday, 10 times → web app shows weekdays only; agent: `get` shows `FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR;COUNT=10` (whatever form Thunderbird sent, R12).
6. New event `pcb-m4 tb monthly`, Tue 9 Feb 2027 11:00–12:00, Repeat: custom, monthly, the second Tuesday, 3 times → web app shows second Tuesdays; agent: `BYDAY=TU;BYSETPOS=2` (R5).
7. In `pcb-m4 tb weekdays`, move the Wed 3 Feb occurrence to 10:00 ("this occurrence only") → web app shows it moved; agent: `rows` 2.
8. Delete the Thu 4 Feb occurrence ("this occurrence only") → web app shows it gone; agent: `get` shows an EXDATE.
9. In `pcb-m4 meeting`, move the Tue 12 Jan occurrence to 11:00 ("this occurrence only") [Task 7a M6: X-PM-TOKEN and attendee statuses reused on a new exception row] → expect success and the web app showing the moved occurrence with its participant; a 403 in Thunderbird (agent: `Proton refused a write … code=` in the log) means Proton refuses tokens reused on another row: record the code. If Thunderbird will not edit the event (with no mail identity it may take it for someone else's invitation), the agent sends the same change: `get <uid>.ics > "$LIVE/meeting.ics"`, append before `END:VCALENDAR` a VEVENT with the master's UID, `RECURRENCE-ID;TZID=Europe/Paris:20270112T100000`, DTSTART/DTEND 11:00–11:30 on 12 Jan, the master's ORGANIZER and ATTENDEE lines, `SUMMARY:pcb-m4 meeting moved`; `sed -i 's/$/\r/' "$LIVE/meeting.ics"`; `put <uid>.ics meeting.ics -H "If-Match: $(etag <uid>.ics)"` → 204.
10. "This and all future occurrences" on `pcb-m4 tb weekdays` from Mon 8 Feb (change the time) [L.4 #12] → record what Thunderbird sent: agent runs `newest 2` (a new UID next to the old one, whose `get` now ends with an UNTIL: Thunderbird truncated the series and made a new one) and reads `serve.log` (a caldav 403 for `recurrence-range` would mean Thunderbird sent RANGE, refused). Record what each app shows.
11. Alarm [E08 §6.2, research F]: create `pcb-m4 alarm` starting 4 minutes from now (today: the one event outside January–February 2027), reminder 1 minute before; when the alarm shows, dismiss it; refresh (Synchronize Calendars) twice, 30 s apart: it must not show again. Agent: `get` of it shows a VEVENT-level `X-MOZ-LASTACK` and the VALARM regenerated without one.
12. Conflict [L.4 #12, E08 §4.2]: rename `pcb-m4 tb renamed` to `pcb-m4 web side` in the web app; **without** synchronising Thunderbird, rename it to `pcb-m4 tb side` in Thunderbird → Thunderbird's conflict dialog (pcb answered 412). Choose to overwrite → web app shows `pcb-m4 tb side`. Record the dialog's wording.
13. UID conflict [L.4 #12, 409]: the agent writes `$LIVE/conflict.ics` with the UID of `pcb-m4 web defaults tb` (`ev conflict.ics '<its uid>' 'pcb-m4 import' 'DTSTART;TZID=Europe/Paris:20270108T140000' 'DTEND;TZID=Europe/Paris:20270108T150000'`) and hands the file to the author, who imports it into the default calendar (Calendar > Import) → Thunderbird PUTs it at `<the UID with [^A-Za-z0-9_.-] made _>.ics`. A web-app UID holds an `@` [inferred], so that name differs from pcb's href and pcb answers 409 naming `<uid>.ics`; with no `@`, the names coincide and pcb answers 412 (Thunderbird reads both as a UID conflict, E08 §4.2). Record which, what Thunderbird shows, and what it holds after a Synchronize (it should keep the server copy).
14. A refused write [README, E08 §3.8]: add a participant `pcb-m4-attendee@example.org` to `pcb-m4 tb side` and save → pcb answers 403 `pcb:attendee-change`. Record what Thunderbird shows (a calendar with offline support keeps the change and retries it silently; the README recommends offline support because a 403 disables an uncached calendar, E08 §4.2). Then remove the participant in Thunderbird and save: the event must be saved again (204), and the web app must show no participant.
15. Delete `pcb-m4 tb side`, `pcb-m4 web defaults tb`, `pcb-m4 meeting` (the whole series; send no cancellation), `pcb-m4 alarm` in Thunderbird → gone in the web app.
16. Stop `pcb serve` (agent, Ctrl-C), quit and restart Thunderbird: cached events still shown; start `pcb serve` (agent), Synchronize: no error, no duplicated calendar.

*E. Over the network, TLS (agent, then author).* Stop `pcb serve` (it listens on every interface until the end of this item: do it on a trusted network), set in `$R/pcb/config/config.toml`, section `[caldav]`: `listen = "0.0.0.0:8443"` and `hostnames = ["<the host's LAN name>"]`, run `live -- pcb serve` again. Expect `Thunderbird URL: https://<hostname>:8443/` and the fingerprint line; `pcb doctor` shows the same fingerprint. On a second machine (**any LAN machine of the author's, named by the author before the item starts; never morpheus**), repeat A with that URL. Thunderbird shows the certificate exception dialog: **compare the SHA-256 fingerprint in the dialog with the one `pcb serve` printed, character by character; confirm only if they match.** Record: whether the dialog appears in the creation flow or the certificate had to be added first under Settings > Certificates, and whether an IP-literal URL works (E08 §2.3). Then restore `listen` (remove the two lines) and restart `pcb serve`.

*F. Revoked client (agent, then author).* `live -- pcb client remove Thunderbird`; the author synchronises: Thunderbird reports an authentication failure and asks for a password (401). `live -- pcb client add Thunderbird` gives a new password; the old one stays refused. `live -- pcb client list` never prints a password.

**6.4 After the sitting (agent).**
1. E13: `get pcb-m4-props.ics` after the web-app rename: list which of W7's lines are still there (one line each: kept, changed, gone).
2. Key packets never rewritten [R30/R35]: no row of the cache may hold other key packets than its pin (prints nothing when none does):
   ```sh
   sqlite3 -readonly "$DB" <<'SQL'
   SELECT e.calendar_id, e.id,
     coalesce(json_extract(CAST(e.raw AS TEXT), '$.SharedKeyPacket'), '')   = p.shared   AS shared_same,
     coalesce(json_extract(CAST(e.raw AS TEXT), '$.CalendarKeyPacket'), '') = p.calendar AS calendar_same,
     coalesce(json_extract(CAST(e.raw AS TEXT), '$.AddressKeyPacket'), '')  = p.address  AS address_same
   FROM events e JOIN pins p ON p.calendar_id = e.calendar_id AND p.id = e.id
   WHERE NOT (shared_same AND calendar_same AND address_same);
   SQL
   ```
   A listed row: decode both spellings (`base64 -d`, whitespace and padding aside) before concluding; pcb compares decoded bytes. Different bytes after a web-app edit (C1) mean Proton's client rewrote key packets: that event is read only for pcb from then on (403 `pcb:untrusted-key`), as R30 accepts; record which edit did it.
3. Stability [L.4 #11]: `sqlite3 -readonly "$DB" "select max(seq) from events where calendar_id='$CAL_ID'"`, wait 70 s with no edit anywhere, again: unchanged.
4. Integrity [E10]: `grep 'render counters' "$LIVE/serve.log" | tail -3`: no `unverified` above 0 (our own rows, including the web app's, verify with our address key); placeholders and their count recorded.
5. Logs: `grep -E 'level=(WARN|ERROR)' "$LIVE/serve.log"` — every line explained by a step above (pcb's own 403s and 412s are not logged; what is: Proton refusals, unconfirmed writes, concurrent changes, refused key packets, and each 503).

**6.5 What to record (agent).** One line each, `[verified: live]` or what differed, with the step's UTC time:
- in `E08-thunderbird-caldav.md`, "Live results": detection through `/.well-known/caldav`, `/` and the principal, preemptive Basic or not (A); sync-collection end to end, including a deletion (C4) and, optionally, a `valid-sync-token` reset (stop `pcb serve`, remove `$R/pcb/state/pcb.db`, `pcb.db-wal`, `pcb.db-shm`, `live -- pcb sync --full`, restart: a new epoch); multiget of a UID with `@` (W11); `readOnly` of each calendar (A); the TLS exception flow and the 825-day certificate (E); real data: non-IANA TZIDs, orphan exceptions, duplicate masters, placeholders (render counters); alarms from `Notifications` shown once and not re-firing (D11); first-sync time and memory (E09 live half); `getcontenttype` with `;component=vevent`; a calendar rename or recolour not reaching an existing subscription; every write of D with its status; the conflict dialog (D12), the 409 import (D13), the refused write (D14), "this and future" (D10); another member's shared calendar: skipped (needs a second account);
- in `E10-signatures.md`: 6.1.4, W1 (sigclass, one PKESK), 6.4.4;
- in `E12-recurrence-forms.md`: W8, W9, B2–B4, D5–D10;
- in `E13-unknown-properties.md`: W7, 6.4.1;
- in `E14-member-permissions.md`: 6.1.4;
- in `E07-row-changes.md` and research L.4: W2, W4, W5, W10, C4/D4 (`PersonalEvents`), D9, 6.4.2, 6.4.3;
- as "not tested (not reachable with M4's tools, `OPEN.md`)": L.4 #3, #5, and the server-side halves of #6 and #8;
- then turn the matching markers of SPEC §5.3, §5.4, §8.1, §9 and §17, `mapping.md` and `OPEN.md`; any Proton behaviour that differs from what the code assumes is fixed test first, in its own commit, before the write-up commit `🧪 M4 live session against the test account`.

**6.6 Clean up (agent).** Delete what is left of 6.2 with `del NAME -H "If-Match: $(etag NAME)"` (`pcb-m4-basic.ics`, `pcb-m4-props.ics`, `pcb-m4-weekdays.ics`, `pcb-m4-2tu.ics`, `pcb-m4-notfirst.ics`, `pcb-m4-count49.ics`, `pcb-m4-until.ics`, `pcb-m4-duration.ics`, `pcb-m4-series.ics`, `pcb-m4-alias_example.org.ics`, `pcb-m4-escaped.ics`), each 204; the author deletes in the web app anything titled `pcb-m4` still there (search "pcb-m4"). `live -- pcb client remove m4-curl`; `rm -rf "$R/m4-live"`. **Keep `$R/pcb`** (the M1/M2/6 session): step 7 runs on its own VM, and its logout and removal are the close-out at the end of the whole sequence (M1.8).

## 7. M5 live session (the daemon, the service, the container; agent, with author actions marked; on a throwaway VM)

M5 (SPEC §8.1, §13, §15) is built and smoke-tested against no Proton at all: `make container-smoke` and `make systemd-smoke` never log in. This step runs the daemon, `pcb install`, the image and the viewable CalDAV credentials against the test account. It runs **after the M1 and M2 live sessions and step 6** (a working login, a filled cache, Thunderbird known to work against `pcb serve`); if step 6 found a defect, fix it first.

**Rules for this step** (on top of "Rules for the whole run"):
- **Test account only** (vault `pcb-dev`, item `proton-calendar-bridge-test`), never morpheus, never the author's own account. On a new host, `pcb status --json | jq -r .data.username` after each login must print the test account's username (compare by eye; do not record it).
- **Where: a throwaway Debian 13 VM (or a container host that never ran pcb), never blackwater, never morpheus.** 7.1 needs a host that has never run pcb (blackwater holds the author's `~/.local/state/pcb`, which 7.6's `--delete-data` would delete), 7.1.8 reboots the machine, 7.5 rejects all outgoing HTTPS for four minutes, and 7.2 runs Docker. The container (7.2) runs under Docker on the same VM. The VM has its own run root `~/pcb-live-run/<run>/` (builds in its `bin/`, scratch in `m5-live/`), and **before 7.1** `make live-preflight RUN="$HOME/pcb-live-run/<run>" STEP7=1` must pass on it: it refuses blackwater and morpheus and any host where `~/.config/pcb` or `~/.local/state/pcb` already exist. `pcb` here runs with the VM's default dirs (that is what `pcb install` tests), so the dir guard does not apply: every login is one call of `live --vm --login 7.N 'PURPOSE' -- <command>`, which refuses blackwater and morpheus by host name and keeps the VM's own ledger (budget 3).
- **`sudo` is typed by the author** (ruling R18: the agent cannot type a password). The lines that need it are marked *author types this*: 7.1.3, 7.1.8, 7.2.1 (rewritten to need none), 7.5.
- Logins: **at most 3 in this step** (7.1, 7.2 and the re-login of 7.3), each at least 60 s after the last one (and after the last entry of the main run's ledger) and counted in the VM's ledger. A CAPTCHA (exit 10 naming a verify URL) is the stop condition of the whole run: record it, ask the author for a fresh solve.
- Credentials through `pass-cli run --env-file scripts/pcb.env.example --` only, never typed, never in argv. For the 2FA-held login of 7.1, run it with `env -u PCB_TOTP` inside `pass-cli run`, so the code is not sent at once, and send a current code on `resume --totp` afterwards (the agent derives it from the `otpauth://` URI in Pass and puts it in `PCB_TOTP` for that one command).
- Every state-changing command below is run on a throwaway host or volume: `rm -rf` only the directories the step created.
- Record each result as `[verified: live]` or what differed, with the UTC time, in `spec/experiments/M5-live.md` (to be created by the run).

**7.1 A fresh Debian host, from source (agent).**
1. As a normal user with sudo, on the VM: clone the repository, then `make build && make install` (`bin/pcb` goes to the VM user's `~/.local/bin/pcb`; put it on `PATH`; this is the VM, never blackwater). `$R/bin/pcb` of the VM's run root may be the same binary; `live` finds it there.
2. `pcb install --plan; echo $?` then `pcb install; echo $?`. Expect exit 0 both times; the plan creates `~/.config/pcb`, `~/.local/state/pcb`, `config.toml`, `~/.config/systemd/user/pcb.service` and runs `systemctl --user enable --now pcb.service`; the apply ends with `Next:` and `loginctl enable-linger <user>`, `pcb login`, `pcb client show` (the first line is absent when lingering is already on).
3. *Author types this on the VM:* `sudo loginctl enable-linger "$USER"`. Then `pcb service status --json | jq .data` shows `active_state` `active`, `unit_file_state` `enabled`.
4. `pcb status; echo $?` (exit 0): `daemon: waiting for a login since ...` and `alert: error needs-login: ... (fix: run pcb login)`. `pcb status --daemon --json | jq -r .data.health.serve` prints `waiting-for-login`. `pcb doctor; echo $?` exits 0 (after the audit fixes; it used to exit 20): `service` ok, `daemon` ok, and `alert needs-login` marked `!` (a warning, the daemon is waiting for a login).
5. A login whose 2FA code is held by the daemon (login #1): `live --vm --login 7.1 'daemon holds the 2FA code' -- pass-cli run --env-file scripts/pcb.env.example -- env -u PCB_TOTP pcb login --non-interactive; echo $?`. Expect exit 10, `the account needs a 2FA code`, hint `PCB_TOTP=<code> pcb login resume --totp before HH:MM:SS (the daemon holds the login)` (local time, 5 minutes ahead). `pcb status` shows `login_expires:`. Then, within the 5 minutes, `PCB_TOTP=<code> pcb login resume --totp; echo $?`: exit 0, `CalDAV credentials: pcb client show` (the `default` client was created). If the account has no 2FA the first command completes the login: note it and skip the resume.
6. `pcb status` shows `daemon: serving since ...`, one `calendar:` line per calendar with `write gate open` or `closed (reason)`, no alert; `pcb client show` prints `URL:`, `Username: default`, `Password:` and, over TLS, a fingerprint (plain HTTP on the loopback default: no fingerprint line). The daemon was never restarted: `pcb status --json | jq .data.daemon.pid` equals the pid of step 4. `pcb sync; echo $?` exits 0 and prints `pcb serve is running and already syncs: see pcb status` (the daemon holds `sync.lock`). Then, for the later steps (plain variables of the VM's run root, set at the head of each call that uses them): `URL=$(pcb client show --json | jq -r .data.url); LIVE=$HOME/pcb-live-run/<run>/m5-live; (umask 077; mkdir -p "$LIVE"; pcb client add m5-curl --json | jq -r '"machine <host of $URL> login me password " + .data.password' > "$LIVE/netrc")` (the password goes to a 0600 file, never into argv; write the host in by hand).
7. Thunderbird (author action, a fresh profile as in 6.3): New Calendar, On the Network, CalDAV, the URL, username `default`, the password of `pcb client show`. The calendars appear; one event edit goes through: create `pcb-m5 vm` (any day in January to February 2027), then rename it `pcb-m5 vm renamed`. Everything step 7 writes is titled `pcb-m5 ...`; 7.9 deletes it (G14). `pcb service logs | tail` shows no `refusing a write` or `not confirmed`.
8. Reboot (*author types this on the VM*: `sudo reboot`, or resets it from the hypervisor; never on blackwater). After the host is back (within 2 minutes, no login by anyone): `pcb service status` is `active`, `pcb status --daemon` exits 0 with `daemon: serving`, Thunderbird refreshes without a prompt. The session survived the restart (sealed in `secrets.sealed`, key file in the config directory).
9. Record `systemd-analyze --user security pcb.service | tail -1` and `pcb doctor` (the secrets backend line says key file).

**7.2 The container (agent; login #2).**
1. `make image` (`pcb:local`, rebuilt now: the local image goes stale). Create the compose project in `$HOME/pcb-live-run/<run>/m5-live/compose` (the scratch directory 7.2.7 removes): the file of README "Run in a container" **with its port line changed to `"127.0.0.1:1080:1080"`** (the README's `1080:1080` publishes on every interface; this session publishes on the loopback only), a `config.toml` with `[secrets] key_file = "/run/secrets/pcb_key"` and `[caldav] hostnames = ["localhost"]`, and `head -c 32 /dev/urandom > pcb_key`, then always, **with no sudo**, through a throwaway container as root: `docker run --rm -v "$PWD:/w" busybox sh -c 'chown 65532:65532 /w/pcb_key && chmod 600 /w/pcb_key'` (the user is in the `docker` group; compose outside swarm mounts the secret with the host file's owner and mode and ignores `uid`/`mode`; pcb refuses a key others can read, 0444 included). Once, before that chown, with only `chmod 600 pcb_key` (still yours): `docker compose run --rm --no-deps --entrypoint pcb pcb doctor` shows `✗  key file: /run/secrets/pcb_key is not readable by this user (uid 65532): permission denied` and exits 10 (the user must act; after the audit fixes); record it, then chown.
2. `docker compose up -d`. Within 60 s `docker inspect -f '{{.State.Health.Status}}' pcb` prints `healthy` while logged out (the daemon answers, `pcb status --json --daemon` exits 0). `docker exec pcb pcb status` shows `waiting for a login`.
3. `live --vm --login 7.2 'container login' -- pass-cli run --env-file scripts/pcb.env.example -- docker exec -e PCB_USERNAME -e PCB_PASSWORD -e PCB_TOTP pcb pcb login --non-interactive` (no `-it`: the agent has no TTY, and `docker exec -t` fails without one; `-it` only where the author types in a real terminal). Expect exit 0 and the `CalDAV credentials: pcb client show` line. `docker exec pcb pcb status` shows `daemon: serving` and `key_file: /run/secrets/pcb_key`. The image has no shell, so `docker exec pcb pcb doctor` is the check: its secrets line says `secrets sealed with a key file (headless fallback)` and, because the key is on another mount, no "same volume" warning.
4. `docker exec pcb pcb client show` prints the URL, `Username: default`, the password and the TLS fingerprint. Compare the fingerprint with `openssl s_client -connect localhost:1080 </dev/null 2>/dev/null | openssl x509 -noout -fingerprint -sha256`: equal.
5. Thunderbird (author action): the URL, the certificate exception dialog shows that fingerprint, confirm it only if equal; credentials as above; calendars sync, an event edit is written: create `pcb-m5 container`, then rename it (cleaned up at 7.9).
6. `docker compose restart pcb`: healthy again within 60 s, no login asked, Thunderbird keeps syncing. `docker compose down` then `up -d`: the same (volume kept). A bind-mount variant (`./data` chowned to 65532, 0700) is tried once: `pcb` starts; with root-owned `./data` it exits 20 with the unsafe-directory message (the README warns about it).
7. Cleanup: `docker exec pcb pcb logout` first (revokes the session through the daemon; expect `revoked`), then `docker compose down -v`, `docker rmi pcb:local` and `rm -rf "$HOME/pcb-live-run/<run>/m5-live/compose"` (that directory only).

**7.3 A session ended by Proton (agent and author; login #3).** On the 7.1 host, `pcb` serving.
1. Author: in account.proton.me, Security, Sessions, revoke the `pcb` session (the newest one from 7.1), and say "revoked at HH:MM". The agent notes the time.
2. Agent: one authenticated request wakes an idle loop (a request to the **calendar home**, not the root: the root answers 207 logged out too, only `/dav/calendars/me/` answers 503; `CAL=${URL}dav/calendars/me/`, `$URL` ending in `/`): `curl -sk --netrc-file "$LIVE/netrc" -o /dev/null -w '%{http_code}\n' -X PROPFIND -H 'Depth: 0' "$CAL"` (a `pcb client add m7-curl` netrc as in 6.1). Within about a minute `pcb status` shows `daemon: waiting for a login` and `alert: error needs-login: ...` (the next sync pass met the refused refresh; with `unattended-relogin` off, nothing else is tried); the same `curl` on `$CAL` answers `503` with `Retry-After: 30`; without credentials `401`.
3. Agent: `live --vm --login 7.3 're-login after the revoke' -- pass-cli run --env-file scripts/pcb.env.example -- pcb login --non-interactive` (login #3, at least 60 s after #2). Expect exit 0 and no restart: `pcb status --json | jq .data.daemon.pid` equals the pid before, `daemon: serving`, the same `curl` on `$CAL` answers `207` (it answered `503` before: the root would say 207 either way and prove nothing), the `default` client still exists and its password is unchanged (`pcb client show` equals what it printed before). If Proton answers a CAPTCHA, stop (rules).
4. Record what Proton answered to the refused refresh (the status and code in the log, `pcb service logs`): it confirms the item "Refused refresh" of "Signals to confirm" (M1).

**7.4 Idle (agent and author).** On the 7.1 host, `pcb` serving, Thunderbird connected.
1. Author: close Thunderbird and do nothing against `pcb` (no `curl`) for over 35 minutes. Not `pcb status`: it uses the control socket, which is not activity.
2. Agent, after 36 minutes: `pcb status --json | jq '.data.health | {idle, interval_seconds, last_pass}'`: `idle` true and `interval_seconds` 300 (a pass every 5 minutes: two `last_pass.at` values read 5 minutes apart differ by about 300 s).
3. Author: reopen Thunderbird (or the agent sends one authenticated `curl`). Within seconds `last_pass.at` moves (a pass started at once) and `interval_seconds` is 30 again, `idle` false.
4. Record: whether Thunderbird, left open for an hour with default settings, ever lets the daemon go idle (`idle` stays false while it is open: the 30-minute refresh, E08 §3.7, is under the 35 minutes).

**7.5 `pcb status` and `pcb doctor` as the author reads them (author).** With the daemon serving, run `pcb status` and `pcb doctor` on the 7.1 host and the container. The author reads each line: is it understandable, does it name the fix, is anything missing for deciding whether the bridge is healthy? Also provoke one alert and read it, on the VM only (never blackwater: it would cut all its HTTPS), *the author types the two `sudo` lines*: keep a client active first (Thunderbird open, or the agent sends one authenticated `curl` a minute), else the loop idles at a 5-minute interval and the stall takes 25 minutes, not 4; then `sudo iptables -I OUTPUT -p tcp --dport 443 -j REJECT` for 4 minutes, then `sudo iptables -D OUTPUT -p tcp --dport 443 -j REJECT` (a failing pass, then a `sync-stalled` warning once no sync succeeded for 5 intervals; it clears at the next good pass). The `calendar-keys-failed` alert only if the test account has a calendar whose keys cannot unlock (skip when absent). Record the author's comments in `M5-live.md`; wording changes are code changes made afterwards, test first.

**7.6 `uninstall --delete-data` revokes the session (agent and author).** On the 7.1 host (user scope; the system scope with a real login is not run live: `make systemd-smoke` covers it without a session).
1. `pcb uninstall --delete-data --plan`: shows stop, disable, logout, removing the unit, `daemon-reload` and the two directories, changes nothing.
2. `pcb uninstall --delete-data` (answer the prompt) or with `--yes`; exit 0. Expect the logout step to say revoked; `~/.config/pcb`, `~/.local/state/pcb` and the unit are gone; `~/.local/bin/pcb` stays.
3. Author: in the web app, Security, Sessions: the `pcb` session of 7.1 (or 7.3) is no longer listed. If it is still listed, record it and revoke it by hand.
4. `pcb service status` exits 10 (`pcb is not installed as a service`, hint `pcb install`); `pcb status` exits 0 and shows logged-out.

**7.7 `pcb client show --reveal` in Thunderbird, then `pcb client rotate default` (agent and author).** On the 7.1 host, serving and logged in, before 7.6.
1. Agent: `pcb client show --json --reveal | jq '.data | {username, password, url, fingerprint}'`: a password is present. `pcb client show --json | jq .data.password` is null (no `--reveal`, no password).
2. Author: in Thunderbird, a new profile or a new calendar, the URL, username `default` and that password: it connects. Agent: `pcb client list` shows `default` with a `last_used` just now.
3. Agent: `pcb client rotate default` prints a new password and `Hint: update the password in your CalDAV client`. The old password now fails: `curl -sk -u default:<old> -o /dev/null -w '%{http_code}\n' -X PROPFIND -H 'Depth: 0' "$URL"` is `401`, the new one `207`, with no restart of the daemon. Thunderbird prompts for the password on its next refresh (author action: enter the new one, the calendar syncs again).

**7.8 The GHCR package is private (complete).** [verified: author, 2026-11-12: Package settings show Visibility Private.] [verified: agent, 2026-11-12, with an empty `DOCKER_CONFIG`: the unauthenticated `docker pull ghcr.io/barben360/proton-calendar-bridge:main` answered `unauthorized` and pulled nothing.] [verified: 2026-11-12, on `dfedab5`: the `ci` run https://github.com/Barben360/proton-calendar-bridge/actions/runs/43256175509 is green (check, the three test shards and `systemd-smoke`) and the `image` run https://github.com/Barben360/proton-calendar-bridge/actions/runs/43256175781 pushed, its smoke job skipped.] The first push to main failed with `denied` (12:20 UTC); the second attempt of the run succeeded at 13:22 UTC and published `:main` and `:sha-557fe84544f6` (digest `sha256:e7d5d0c8fd32…`). Nothing here needs doing; only the optional pull after `docker login` (a token with `read:packages`, which the agent's tokens lack) is not recorded. The procedure below stays as the reference. `.github/workflows/image.yml` pushes `ghcr.io/barben360/proton-calendar-bridge:main` and `:sha-<12 hex>` as a private package. On github.com, the repository's Packages, `proton-calendar-bridge`, Package settings: confirm "Visibility" is Private (and that the package is linked to the repository). If it is Public, change it to Private at once and record it. `docker pull ghcr.io/barben360/proton-calendar-bridge:main` from a machine not logged in to ghcr.io must fail (`unauthorized` or `denied`); after `docker login ghcr.io` with a token that has `read:packages`, it succeeds. Also confirm that both workflows ran green on that push: `image` (the `push` job) and the `systemd-smoke` job of `ci.yml`.

**7.9 Write up and clean up (agent).** First the test events: the author deletes every event titled `pcb-m5` (search "pcb-m5" in the web app; the ones made in 7.1.7 and 7.2.5) and says so; the agent notes it. Then create `spec/experiments/M5-live.md` with each step's result (redacted), turn the `[assumed → step 7]` markers of SPEC §7.3, §8.1 and §13 and the E15 note to `[verified: live]` or what differed (test first for any code change, own commit), update README "Status", commit `🧪 M5 live session against the test account`.

## M1 live session (after steps 1 to 4; agent-run except the author's marked actions)

Proves `pcb` against the test account, with the main-based build in `$R/bin` (the worktrees are gone). Same rules as above, plus:

- Isolated dirs for every `pcb` command below: `live -- pcb ...` (the run root's `pcb/config` and `pcb/state`; no `export`). Run each login as `live --login <STEP> '<purpose>' -- pass-cli run --env-file scripts/pcb.env.example -- pcb login ...` (username, password, TOTP URI from the environment; never argv). Same for `pcb sync`, `pcb calendar list` and the other commands that may refresh the session.
- **At most 3 logins in the whole session** (L1 step M1.2, L2 step M1.3, L3 step M1.7), at least 60 s after the last M0 login and after each other; the ledger enforces it and is the login log, with the M0 one.
- Stop conditions are the M0 ones. A refresh or relogin answered by anything not listed in "signals to confirm" below: stop, record the raw status and Proton code, do not retry.
- The CAPTCHA is shared with step 1: if step 1's challenge was never solved, the author's single solve covers both; if M0 consumed it, M1.2 may meet a fresh one (then the author solves the new URL right away).

**M1.1 Build and pre-check.** The build is in `$R/bin` ("The run root"). `live -- pcb version`. `PROTON_PASS_AGENT_REASON="<why>" pass-cli info` must succeed (step 2 pre-check).

**M1.2 Login (L1).** `live --login M1.2 'L1' -- pass-cli run --env-file scripts/pcb.env.example -- pcb login --non-interactive --unattended-relogin --json > "$R/m1-l1.json"; echo $?` (`R=$HOME/pcb-live-run/<run>` at the head of the call: the redirect is outside `live` and writes a file of the run root; the token in it is a CAPTCHA token, not a secret)
- Exit 0, `data.state` `ready`: no CAPTCHA this time; record "no CAPTCHA seen" for E02, skip M1.3 (the budget keeps one spare login) and go to M1.4.
- Exit 10 with `error.message` naming `https://verify.proton.me/?methods=captcha&token=...` and `error.hint` `pcb login resume --captcha-token <T>` (T quoted only if needed), and `data.captcha_token` carries the token itself: the author solves the URL (`jq -r .error.message "$R/m1-l1.json"`), says "captcha solved at HH:MM"; the agent notes the challenge time (the L1 time) and goes to M1.3.
- Exit 10 for any other reason (wrong credentials, 2FA): record `error.message`, stop. Exit 20 "too soon": wait the 60 s and rerun once.

**M1.3 CAPTCHA resume (L2, E02 data point), only after a CAPTCHA at M1.2.** At least 60 s after L1, a new process: `R=$HOME/pcb-live-run/<run>; T=$(jq -r .data.captcha_token "$R/m1-l1.json"); live --login M1.3 'L2 CAPTCHA resume' -- pass-cli run --env-file scripts/pcb.env.example -- pcb login resume --captcha-token "$T" --non-interactive --unattended-relogin --json`. Expect `ready`. Record in `spec/experiments/E02-captcha-token.md` a new row: challenge time (L1), solve time (author's message), retry time, token age at retry, new process, accepted/refused (a fresh 9001 or code 12087 instead is a refusal; then the author solves the new URL at once and the agent retries within a minute: second row). If M1.3 is refused twice, stop.

**M1.4 Status and the M1 done criterion (author needed for the extra calendars).** `live -- pcb status --json` (expect `phase` `ready`, `unattended` true, `last_login` set, no network call) then `live -- pcb calendar list --json`. **M1 is done when** the list shows every calendar of the test account with `type` (normal, subscribed, holidays), `read_only`, exactly one `default`, and `keys` `ok` for each. Save the redacted output (names stay; ids and colours are not secrets but keep only what the write-up needs). Any `keys` `error`: record `keys_error` and the calendar type; do not stop. Then the author adds one holidays and one subscribed calendar in the web app (author action, can be done any time before M1.5; skip if absent and note it) and the agent reruns `live -- pcb calendar list --json` (record types and flags). No login is used.

**M1.5 Recorded fixtures and contract test.** Every `pcb-lab probe` resumes the calendar-access lab session of step 2 (`lab/<w>`; its own dir, so the later candidates did not replace it) and may rotate its tokens (only `probe refresh` is limited to once, for E01). Run: `live --lab <w> -- env PCB_LAB_APPVERSION=<its, none for 2a> pcb-lab probe calendar-access --state-dir @LAB@ --record-dir @RUN@/m1-fixtures`. It records the redacted `/calendar/v1` list, `/calendar/v1/{id}/bootstrap` and `/settings/calendar` answers (`internal/lab/record` refuses anything with a secret). Ruling R11m: only the list and the settings are fixtures and contract-tested; the probe's `/calendar/v1/{id}/bootstrap` is the old v1 shape, not the `/calendar/v2` bootstrap pcb uses, which is proven live by M1.4 (`pcb calendar list` showing `keys` `ok`), so the bootstrap file is not copied. If the lab session is gone, a fourth login is not allowed: defer this step and note it. Then:
1. `mkdir -p internal/proton/calendar/testdata && cp $R/m1-fixtures/*calendar-list.json $R/m1-fixtures/*settings-calendar.json internal/proton/calendar/testdata/`; open each file and check by eye that no token, key block, passphrase or email is in it.
2. Add `TestDecodersAcceptRecordedFixtures` to `internal/proton/calendar` (no network): serve each fixture's `body` at its recorded `path` from an `httptest` server, run `calendar.List` and `calendar.DefaultID` through `api.Client`, and assert at least one calendar, non-empty names, and a default id that is one of them (or none, if the settings fixture has none). It must go red when a field is renamed in a fixture (sabotage once, restore).
3. `go test ./internal/proton/calendar/...`, `make lint test`, commit `🧪 Record calendar fixtures from the test account`.

**M1.6 Signals to confirm.** For each item of "Signals to confirm" below, record the real HTTP status and Proton code seen in M1.2 to M1.7 (the JSON `error.message` of the failing command), or "not exercised". Items that need a deliberate failure (wrong TOTP, wrong password) are exercised only if the login budget has room (a wrong password at `pcb login` costs a login); otherwise leave them `[assumed]` and say so in the write-up. If Proton answers differently, adjust the classification in `internal/proton/auth` or `internal/proton/api` test first, in its own commit.

**M1.7 Unattended relogin after a revoke (L3; author needed).** The author revokes the `pcb` session (account.proton.me, Security, Sessions; the one created at M1.2/M1.3) and says "revoked at HH:MM". The agent, at least 60 s after the last login: `live --login M1.7 'L3 unattended relogin' -- pass-cli run --env-file scripts/pcb.env.example -- pcb calendar list --json`, then `live -- pcb status --json`. Expect exit 0 with the calendar list after exactly one unattended relogin (`status.last_relogin` set), or exit 10 with a CAPTCHA (`status.phase` `needs-captcha`, `captcha_url` set): both are results to record, and the first confirms the refusal signal for a revoked session (item "Refused refresh"). If the author is away, record "revoke skipped" and go on.

**M1.8 Logout and clean up: moved to the very end of the sequence** ("Close-out", after step 7): M2, step 6 and the author's later steps reuse this session and its dirs, so nothing here is revoked or removed before then.

**M1.9 Write up** (after M1.7; the logout result is added at the close-out). Create `spec/experiments/M1-live.md` (what each step showed, redacted: exit codes, `phase`, calendar types and flags, the login log with times, the status and Proton code for every signal below); fill the E02 data point (M1.3) or "no CAPTCHA seen in N logins"; update SPEC §5.2 (locked scope on fresh sessions: `[verified]` only if M1.2 or M1.7 exercised it; session lifetime and CAPTCHA reuse markers per E01/E02 results) and §5.4 (calendar types and keys per type, from M1.4); README Status; commit `🧪 M1 live session against the test account`.

### Signals to confirm (M1 live run)

Real-Proton answers that `internal/proton/auth` classifies on assumption (ruling R6m); each one is recorded during M1.6/M1.7, and the code is adjusted if Proton answers differently.

- Wrong TOTP on `POST /auth/v4/2fa`: assumed HTTP 422 (fake uses code 12060), mapped to `ErrTOTPRejected`; record the real status and code.
- Wrong login password on `PUT /core/v4/users/unlock`: assumed HTTP 422 with code 8002, mapped to `ErrBadCredentials`; record the real status and code.
- Refused refresh on `POST /auth/v4/refresh` (spent or revoked refresh token): assumed HTTP 401 or 422, or code 10013, mapped to `api.ErrSessionEnded`; record the real status and code (any other answer must leave the session alone).
- Wrong password on `POST /auth/v4`: assumed HTTP 422 with code 8002, mapped to `ErrBadCredentials`; record the real status and code.
- Refused or expired CAPTCHA token on `POST /auth/v4`: assumed code 12087, mapped to `ErrCaptchaRejected` (a fresh 9001 challenge instead is also possible); record what Proton sends.
- Locked scope on fresh sessions: whether salts need `PUT /core/v4/users/unlock` (403 with code 9101) at all with this client identity; record whether elevation happened.
- Calendar bootstrap on holidays and subscribed calendars: whether member passphrase and keys exist for them (`keys` `ok` or `error` per type).

## M2 live session (after the M1 live session; author edits in the web app, agent runs the commands)

Proves the cache and sync against the test account and settles E04, E05, E07, with the build in `$R/bin`. Same rules as above, plus:

- **No new login is needed**: `pcb` resumes the session left by the M1 live session (a relogin only if Proton ended it, then within the M1 budget rules: at least 60 s after the last login). `pcb-lab` resumes the calendar-access lab session of step 2 (`lab/<w>`). If either session is gone, stop and say so.
- Reuse the M1 dirs, `$R/pcb/*` (`live -- pcb ...` as in M1): M1.8 no longer removes them before this step (it is the close-out now), so no login is needed unless Proton ended the session, which is the one spare login of the budget. `pcb-lab` keeps its own dirs under `$R/lab`.
- The author's actions below are web-app edits on the **test account**, one per step, with the time noted, so the recordings can be matched. Stop conditions are the M0 ones; any 429: wait it out, never faster.
- Nothing here ever runs against the author's own account.

**M2.1 Build.** Already in `$R/bin` ("The run root"). `live -- pcb version`.

**M2.2 Initial sync (agent).** `live -- pcb sync --json; echo $?`. Expect exit 0, one entry per calendar in `data.calendars` with `mode` `initial`, `added` equal to the calendar's event count and no `error`; the seeded data set gives about 370 events in the seeded calendar(s) (`DATASET.md`). Then `live -- pcb sync --json` again at once: expect `mode` `incremental` and `added` `updated` `deleted` all 0. `live -- pcb event list --calendar <name> --json | jq '.data.events | length'` equals `added` from the first pass (and equals the count shown in the web app). Record the pass duration (`time live -- pcb sync`); no request count is printed.

**M2.3 Author needed: create, edit, move, delete, one edit at a time (author action; agent notes the UTC time of each).** In the web app, on one calendar (call it C), one event each, in this order, waiting 5 s between edits:
1. create a new event `E-m2-create`;
2. change its title;
3. change its start and end time;
4. change its description;
5. add a notification only (reminder), changing nothing else;
6. change its colour only, changing nothing else;
7. move it to a second calendar;
8. delete it;
9. (if an invite exists in the test account) answer an RSVP;
10. create, rename, recolour, set as default and delete a throw-away calendar (E05).
The author says "edits done at HH:MM".

**M2.4 Record the feed (agent; steps M2.3 and M2.4 interleave).** The agent has no TTY: with stdin not a terminal the probe waits the whole `--wait` (it prints which mode it is in) and cannot be ended with Enter. So the agent starts it with a `--wait` that covers the author's ten edits (about 5 minutes; 15 minutes with the margin) and the author edits during that window ("edits done" does not end it earlier; only an author sitting at a real terminal can press Enter). The probe takes the feed heads **before** the edits, so start it first: `live --lab <w> -- env PCB_LAB_APPVERSION=<its, none for 2a> pcb-lab probe feed --state-dir @LAB@ --calendar <C id from pcb calendar list --json> --wait 15m` (records under `spec/experiments/raw/E04/`). Expect: prints the number of pages, entries, rows re-read and gone, `refresh false`, and `feed-latest.json`, `feed-core-latest.json`, `feed-page-NN.json`, `feed-row-NN.json`, `feed-core-events.json` in `raw/E04/`. Open each file and check by eye that no token, key block, email or ciphertext is in it (the recorder refuses anything it recognises; `Data`, `Signature` and `Author` are redacted). Exit 20 or an HTTP 4xx in `statuses`: record the status and Proton code and stop (the probe never retries).

**M2.5 Sync follows the edits (agent).** `live -- pcb sync --json; echo $?`. Expect exit 0, `mode` `incremental` for C and the second calendar, `added` `updated` `deleted` reflecting the net change since M2.2, not the number of edits: an event created and deleted between two syncs never enters the cache (not counted), so expect `E-m2-create` absent from both calendars, and the sums to match the difference between `pcb event list` counts before and after (or, if the author synced in between, the moved event deleted from C and added to the second calendar), no full resync. Record the timing if run within 2 s of the last edit (lag, E04). `live -- pcb event list --calendar C --json` and the second calendar's list match the web app: the edited event shows the last title and time, the deleted one is gone. Also `live -- pcb calendar list --json` shows the throw-away calendar's creation and deletion (E05).

**M2.6 Watch follows a new edit within one interval (agent + author needed for one title edit).** `live -- pcb sync --watch --interval 30s` in a terminal, or in the background with its output in a file and ended by `SIGINT` (Ctrl-C ends it with exit 0). Expect a timestamped line per calendar every 30 s, `+0 ~0 -0` while idle (a pass that fails prints its error on stderr with "next try in X"; one calendar failing does not slow the others). The author edits one event title in the web app; within one interval (30 s, plus Proton's lag) the next pass prints `~1`, and `pcb event list` shows the new title. Ctrl-C: exit 0.

**M2.7 Stale cursor and fallback (agent, optional).** With no `pcb sync` running (stop any `--watch` first: the sync lock is held for the whole watch), corrupt one calendar's cursor **in place**: `sqlite3 "$HOME/pcb-live-run/<run>/pcb/state/pcb.db" "update calendars set cursor='garbage' where id='<C>'"`. Never run pcb on a copy of the state directory: the copy holds the same single-use refresh token, so a refresh in the copy spends the original's and the next real command must log in again (on the CAPTCHA-gated account). Then `live -- pcb sync --json`. Expect one of: the calendar `mode` `full`, `error` empty, exit 0 (Proton refused the cursor with a 4xx or answered `Refresh`, and the calendar's last listing is more than 5 min old); or, when its last listing is under 5 min old (`pcb status` shows `full <time>`), exit 40 with the calendar's `error` naming the cause ("Proton refused the feed cursor" or "Proton asked to refresh the feed") and the time it retries (M2-R10, M2-R11); or an error naming another status and code. Record which, with the status and code (E04 question 3). Then run `live -- pcb sync --json` again: after a listing, expect `incremental`, exit 0 (the new cursor works); after a deferral, expect exit 40 again with no `modelevents` request for that calendar until the stated time (the feed is not read during the gap), and `full`, exit 0, on the first pass after it.

**M2.8 Clean up and write up.** Remove the throw-away data created above (events and calendar). Fill `spec/experiments/E04-calendar-feed.md`, `E05-core-events.md`, `E07-row-changes.md` (each "Results" and the E07 table), turn the `[assumed → E04/E05/E07]` markers in SPEC §5.5, §7.2, §8.1, §8.2 and §17, fix `internal/proton/events` or the syncer rules where Proton differs (test first, own commit), commit `🧪 M2 live session against the test account`.

## Close-out (after step 7, before the step 5 write-up)

**M1.8 Logout and clean up** (moved here from the M1 section; it used to revoke the session and delete the dirs that M2, step 6 and step 7 reuse).
1. `live -- pcb logout --json` (expect `revoked` true if the session was alive), then `live -- pcb status --json` shows `logged-out`. Check the revoke at account.proton.me if the author is present, else rely on `revoked`.
2. Remove only what this run made and nothing else needs: `rm -rf "$R/pcb" "$R/m4-live" "$R/lab/2b" "$R/lab/2c" "$R/lab/2d" "$R/m1-fixtures" "$R/m1-l1.json"`. **Keep** `$R/bin`, `$R/lab/e01`, `$R/e01` and `$R/logins.tsv` while the E01 timer runs (14 days); `$R/lab/<w>` and `$R/seed` stay until the write-up is done. Removing a dir does not revoke its session: the author may revoke the leftover lab and proton-cal sessions in the web app (Security, Sessions), except E01's.
3. The step 7 VM is thrown away (after `docker rmi pcb:local` and 7.2.7). Never delete anything outside `~/pcb-live-run/<run>/` and the VM.

Then step 5.
