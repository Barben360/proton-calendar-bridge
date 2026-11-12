# Live-session scripts

Keep every live command (anything that can reach the Proton test account) away from the author's own account and directories. The order of the session is `spec/HUMAN-CHECKS.md`.

| File | What |
|---|---|
| `run.sh` | One guarded command: `scripts/live/run.sh --run <name> [--lab <dir>] [--pcb <dir>] [--proton-cal] [--login STEP PURPOSE] -- COMMAND ...`. Sets the run-root dirs, sources the guard, records the login, runs the command (`@RUN@` and `@LAB@` in its arguments expand to the run root and the lab dir) with `$R/bin` first on `PATH`. |
| `guard.sh` | Sourced by `run.sh` (or by hand, in the same command). Refuses unless `PCB_CONFIG_DIR`, `PCB_STATE_DIR`, `PCB_LAB_DIR` (the dir passed to `pcb-lab --state-dir`; the lab has no variable of its own) and, when used, `PROTON_CAL_CONFIG_DIR` are set, absolute and, symlinks resolved, below `~/pcb-live-run/<run>/`. Defines `live_login` (the login ledger). |
| `common.sh` | Shared functions: the account's home from the user database (not `$HOME`), the step 7 fence, and the test hooks (`PCB_LIVE_HOME`, `PCB_LIVE_NOW`, `PCB_LIVE_HOSTNAME`, `PCB_LIVE_VIRT`, `PCB_LIVE_VM_MARKER`, budgets), honoured only with `PCB_LIVE_TEST=1`. |
| `preflight.sh` | `make live-preflight RUN=~/pcb-live-run/<run> [STEP7=1]`: guard, the three binaries in `$R/bin`, ports 1080 and 8443, the host (morpheus refused, blackwater warned or refused for step 7), the login budget. Calls no API. |
| `guard_test.sh` | `make live-preflight-test` (also in CI): the guard refuses each real dir, relative, unset and symlinked paths and accepts a run-root dir. Runs in a throwaway `HOME`. |

## `--vm` (step 7 only)

`run.sh --vm` is for the throwaway VM, where pcb deliberately runs with the host's default dirs, so the dir guard cannot apply and `PCB_*_DIR` are removed from the command's environment. It runs only when `/etc/pcb-live-vm` exists and contains the word `throwaway` (the author creates it on the VM), `systemd-detect-virt` reports a VM or container, and the host name is not morpheus or blackwater (any case). Its ledger budget is 3. No `pass-cli` or secret exists on the VM: the author types the credentials at the prompts of an interactive `pcb login`.

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
- The login ledger (taken under `flock`, so parallel logins cannot both pass) refuses a login within 60 s of the last one or beyond the budget: 11 per run root, 14 in the global ledger `~/pcb-live-run/.ledger` shared by all runs; on the step 7 VM 3 and 3. A refused login exits 20 and records nothing.
- Bash and GNU `realpath` only. Nothing here reads the contents of a dir it checks.
