# Synthetic data set of the test account

Later milestones (sync, CalDAV) are tested against the Proton test account (vault `pcb-dev`, item `proton-calendar-bridge-test`), so it must hold years of varied events. The data set is declared in `testdata/seed/dataset.yaml` and written to the account by `pcb-lab seed apply`, which drives the author's proton-cal fork CLI (built at `origin/main`, `bf71f0f` on 2026-11-11) as an external tool. Our own write path comes in M4; none of proton-cal's code is copied.

Status: built and tested offline; **not yet applied** (the account's login is blocked by a pending CAPTCHA, ruling R12/R20). See "Pending live run".

**Never edit the data set between a partial `seed apply` and its resume**: the ids (`s001`...) are positional, so an edit shifts them and the resume would skip or duplicate the wrong events.

## Content

The file holds generator parameters and explicit special cases. `internal/lab/seed` expands them deterministically (PCG generator, `seed: 42`; same file, same events, ids `s001`...). `pcb-lab seed plan` prints the result without touching the network.

| Part | Count | What |
|---|---|---|
| Timed singles `s001`-`s300` | 300 | 2021-01-01 to 2027-06-30, 15 min to 3 h, start 07:00-19:45 local; zones Europe/Paris 70 %, America/New_York 12 %, Asia/Tokyo 10 %, UTC 8 %; calendars default 55 %, Work 30 %, Sport 15 % |
| All-day `s301`-`s340` | 40 | 60 % single day, 40 % two to five days (end inclusive) |
| Series `s341`-`s348` | 8 | daily x10 (Paris); weekly until 2027-01-01 (New York); every 2 weeks x20; monthly `BYMONTHDAY=15` x24; last Friday of the month `FREQ=MONTHLY;BYSETPOS=-1;BYDAY=FR` (unbounded); yearly all-day anniversary x7; weekly x8 across the 2026-03-29 Paris DST change; daily x14 in Tokyo |
| Text cases `s349`-`s352` | 4 | emoji and unicode summary (including a ZWJ family emoji); 2 KB description; location with `,` `;` `\` and newlines; three reminders and a colour |

Each series has one edited occurrence (moved and renamed `... (edited)`, summary `pcb-seed <id>.e ...`) and one deleted occurrence: 8 edits, 8 deletions. Reminders over the generated events: calendar default, none, `15m`, `1d`, `email:1h`. Colours: calendar default plus strawberry, pacific, fern, carrot, purple, reef, cobalt.

Calendars: the account's **default** calendar (found by its `is_default` flag in `proton-cal calendars -o json`, never by name: the name `My calendar` in the file is informational), `Work` and `Sport` (found by name, created with `proton-cal create calendar` when missing).

Every summary starts `pcb-seed <id> `. That prefix is how a rerun finds what exists and how to wipe.

## How the seeder works

`pcb-lab seed apply` (flags `--dataset`, `--bin proton-cal`, `--config-dir`, `--interval 2s`):

1. Sets `PROTON_CAL_CONFIG_DIR` to `--config-dir`, else `$XDG_STATE_HOME/pcb-lab-seed` (default `~/.local/state/pcb-lab-seed`), so proton-cal's session never mixes with the author's own; **the live run always passes `--config-dir` under the run root** (`$R/seed`, HUMAN-CHECKS "The run root"), never the default. The child gets an environment allowlist (`PATH`, `HOME`, `XDG_*`, `TZ`, `LANG`, `LC_*`, `SSL_CERT_FILE/DIR`, `HTTP(S)_PROXY`, `NO_PROXY`) and no credential variable; apply never logs in.
2. Lists calendars, creates the missing ones, lists existing `pcb-seed` events (`events --all-calendars --from 2020-12-31 --days 2374 -o json`: the range widened by a day each side, since proton-cal filters in its own time zone).
3. Plans the missing commands (`Plan`), then runs them in order, **one write per 2 s**. Commands carry `{cal}` (calendar id) and `{id}` (event id of the series an edit or delete addresses); the id comes from the `-o json` output of the create, or from the listing on a resumed run.
4. **Stops on the first 429 / rate limit, on a write or a listing** (exit 40) and prints how to resume: rerun the same command. Idempotent: an event whose id exists is skipped; a series without its `.e` edit gets the edit; a series still listing its to-delete occurrence gets the delete.

About 370 writes, so roughly 12 minutes at 2 s each.

`pcb-lab seed plan [--existing events.json]` prints the same commands offline (`--json` for an envelope), skipping what the given listing already holds.

## Pending live run

Not run yet: proton-cal's own login on the test account would hit the same CAPTCHA handling as `pcb-lab login` (HUMAN-CHECKS step 4, after steps 1 to 3). Everything below runs through `scripts/live/run.sh` (`live` for short, HUMAN-CHECKS "The run root"): one call per command, so `PROTON_CAL_CONFIG_DIR` can never be lost between calls and fall back to the author's own `~/.config/proton-cal` (the guard refuses it). `R=$HOME/pcb-live-run/<run>`; the worktrees of the earlier milestones are gone, so everything is built from main or from the fork's `origin/main` into `$R/bin`.

```
cd <a checkout of main of this repository>

# 1. build the fork at its current main into the run root (a scratch worktree of the fork; the clone's own tree is not touched)
git -C /home/jduff/Workspace/github.com/Barben360/proton-cal fetch origin
git -C /home/jduff/Workspace/github.com/Barben360/proton-cal worktree add --detach /tmp/pc-main origin/main
(cd /tmp/pc-main && go build -o "$R/bin/proton-cal" ./cmd/proton-cal)
git -C /home/jduff/Workspace/github.com/Barben360/proton-cal worktree remove /tmp/pc-main
go build -trimpath -o "$R/bin/pcb-lab" ./cmd/pcb-lab        # if not built yet

# 2. log proton-cal in with its own isolated config dir ($R/seed, set by live --proton-cal); credentials from pass-cli, never typed
live --proton-cal --login 4 'seed: proton-cal login' -- pass-cli run --env-file scripts/seed.env.example -- proton-cal login --no-prompt
#    with a solved CAPTCHA token (also from a new process, 60 s after any other lab login; the ledger enforces the 60 s):
live --proton-cal --login 4 'seed: proton-cal login with a solved token' -- pass-cli run --env-file scripts/seed.env.example -- proton-cal login --no-prompt --captcha-token <token>

# 3. check what is there, then seed (the same dir for the child: --config-dir)
live -- pcb-lab seed plan | tail -1
live --proton-cal -- pcb-lab seed apply --bin @RUN@/bin/proton-cal --config-dir @RUN@/seed
```

`scripts/seed.env.example` has the same three `pass://` references as `scripts/lab.env.example` under proton-cal's variable names (`PROTON_CAL_USERNAME`, `PROTON_CAL_PASSWORD`, `PROTON_CAL_TOTP`; the TOTP URI lets proton-cal compute a fresh code).

- Exit 40 with a rate-limit message: wait a few minutes and rerun step 3's `apply` unchanged (the same `live` call); it resumes.
- The login counts as a lab login (one per run, 60 s apart, honour `Retry-After`; AGENTS.md). A new CAPTCHA is a new row in `E02-captcha-token.md`.
- After a successful run, record the final counts here (`live --proton-cal -- proton-cal events --all-calendars --from 2020-12-31 --days 2374 -o json | jq length`) and any occurrence edit proton-cal refused.

Things the offline tests cannot show and the first live run may reveal: proton-cal limits `--count` to 49 (all series stay below), requires a series start that matches its RRULE (the file's starts do), and may refuse moving an all-day occurrence or an unbounded RRULE; a refusal shows as a failed command naming its id (`s346.e`, ...), to fix in the file.

## Reseed

Edit `testdata/seed/dataset.yaml` (or its `seed:`), wipe, rerun `seed apply`. Without wiping, a changed generator would leave old events beside new ones: ids are positional (`s042` is whatever the 42nd generated event is), so only add *new* explicit entries or wipe first.

## Wipe

Delete every event whose summary starts with `pcb-seed` (it is the whole data set, nothing else). One guarded call, so the config dir is the run root's (`$R/seed`, never `~/.config/proton-cal`); the loop runs in a `bash -c` of that call:

```
live --proton-cal -- bash -c '
  proton-cal events --all-calendars --from 2020-12-31 --days 2374 -o json \
    | jq -r ".[] | select(.summary | startswith(\"pcb-seed \")) | \"\(.id)\t\(.calendar_id)\"" | sort -u \
    | while IFS=$(printf "\t") read -r id cal; do proton-cal delete event "$id" --calendar "$cal"; sleep 2; done'
```

Deleting a series id removes the series with its edited occurrences; ids of edited occurrences listed afterwards may already be gone (ignore the error). The `Work` and `Sport` calendars stay; delete them with `live --proton-cal -- proton-cal delete calendar Work --yes` if needed.
