# Live-session scripts

Keep every live command (anything that can reach the Proton test account) away from the author's own account and directories. The order of the session is `spec/HUMAN-CHECKS.md`.

| File | What |
|---|---|
| `run.sh` | One guarded command: `scripts/live/run.sh --run <name> [--lab <dir>] [--pcb <dir>] [--proton-cal] [--login STEP PURPOSE] -- COMMAND ...`. Sets the run-root dirs, sources the guard, records the login, runs the command (`@RUN@` and `@LAB@` in its arguments expand to the run root and the lab dir) with `$R/bin` first on `PATH`. |
| `guard.sh` | Sourced by `run.sh` (or by hand, in the same command). Refuses unless `PCB_CONFIG_DIR`, `PCB_STATE_DIR`, `PCB_LAB_DIR` (the dir passed to `pcb-lab --state-dir`; the lab has no variable of its own) and, when used, `PROTON_CAL_CONFIG_DIR` are set, absolute and, symlinks resolved, below `~/pcb-live-run/<run>/`. Defines `live_login` (the login ledger). |
| `preflight.sh` | `make live-preflight RUN=~/pcb-live-run/<run> [STEP7=1]`: guard, the three binaries in `$R/bin`, ports 1080 and 8443, the host (morpheus refused, blackwater warned or refused for step 7), the login budget. Calls no API. |
| `guard_test.sh` | `make live-preflight-test` (also in CI): the guard refuses each real dir, relative, unset and symlinked paths and accepts a run-root dir. Runs in a throwaway `HOME`. |

## Run root layout

```
~/pcb-live-run/<run>/
  bin/            pcb, pcb-lab, proton-cal (built here, never ~/.local/bin)
  pcb/config, pcb/state     the pcb session M1, M2 and step 6 share
  lab/2a 2b 2c 2d e01       one pcb-lab session each (--state-dir)
  seed/                     proton-cal's config dir (PROTON_CAL_CONFIG_DIR)
  logins.tsv                the login ledger: epoch, UTC time, step, purpose
```

## Rules

- `PCB_CONFIG_DIR` and `PCB_STATE_DIR` are used as they are (`internal/paths`); without them pcb would take `~/.config/pcb` and `~/.local/state/pcb`, and `pcb-lab` `~/.local/state/pcb-lab`: this is what the guard prevents.
- The login ledger refuses a login within 60 s of the last one or beyond the budget (`PCB_LIVE_LOGIN_BUDGET`, 11 on the main host; step 7 on its VM uses its own run root with 3). A refused login exits 20 and is not recorded.
- Bash and GNU `realpath` only. Nothing here reads the contents of a dir it checks.
