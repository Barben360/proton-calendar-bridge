#!/usr/bin/env bash
# Guard for every live command (live-readiness R1, triage G10/G11).
#
# SOURCE it, in the same command as the call it protects:
#
#   ( export PCB_CONFIG_DIR=$R/pcb/config PCB_STATE_DIR=$R/pcb/state; \
#     . scripts/live/guard.sh && bin/pcb status )
#
# It refuses (return 1; exit 1 when executed) unless every one of
#   PCB_CONFIG_DIR   pcb's config dir (internal/paths EnvConfigDir)
#   PCB_STATE_DIR    pcb's state dir (internal/paths EnvStateDir)
#   PCB_LAB_DIR      the dir handed to `pcb-lab ... --state-dir` (pcb-lab has
#                    no variable of its own: without the flag it would fall
#                    back to $(dirname $PCB_STATE_DIR)/pcb-lab)
#   PROTON_CAL_CONFIG_DIR   proton-cal's config dir; required when
#                    PCB_GUARD_PROTON_CAL=1, checked whenever it is set
# is set, absolute, and, after symlinks are resolved, a sub-directory of
# <home>/pcb-live-run/<name>/ (home from the user database, not $HOME) (the run root; <name> is the run, e.g. a date),
# all four under the same run root. XDG_CONFIG_HOME, XDG_STATE_HOME and
# XDG_DATA_HOME are checked the same way when set (a stray default would
# otherwise reach ~/.config/pcb, ~/.local/state/pcb, ~/.local/share/pcb or
# ~/.config/proton-cal, the author's real ones).
# On success it prints the resolved dirs and exports PCB_LIVE_RUN, the run root.
#
# It also defines live_login STEP PURPOSE, the login ledger: it refuses a
# login within 60 s of the last one or beyond the budget (11 per run, 3 with
# PCB_LIVE_VM=1, and a global ledger above the run roots), else records it in $PCB_LIVE_RUN/logins.tsv.
# Bash and GNU realpath only. The guard reads no file of the dirs it checks.

# shellcheck source=common.sh
. "$(dirname "${BASH_SOURCE[0]}")/common.sh"

live_guard_fail() { printf 'live guard: %s\n' "$*" >&2; }

# live_guard_dir NAME VALUE: prints the resolved dir, or fails with the reason.
live_guard_dir() {
  local name=$1 value=${2-} home base res
  if [[ -z $value ]]; then live_guard_fail "$name is not set"; return 1; fi
  if [[ $value != /* ]]; then live_guard_fail "$name=$value is not an absolute path"; return 1; fi
  if [[ $value == *$'\n'* ]]; then live_guard_fail "$name holds a newline"; return 1; fi
  home=$(live_home) || return 1
  home=$(realpath -m -- "$home") || return 1
  base=$home/pcb-live-run
  res=$(realpath -m -- "$value") || { live_guard_fail "$name=$value cannot be resolved"; return 1; }
  case $res in
    "$base"/*/*) ;;
    "$base" | "$base"/*)
      live_guard_fail "$name=$value resolves to $res: a dir must sit below $base/<run>/ (not the run root itself)"
      return 1 ;;
    *)
      live_guard_fail "$name=$value resolves to $res, outside $base/<run>/ (refused: it could be the author's real data)"
      return 1 ;;
  esac
  printf '%s\n' "$res"
}

# live_guard: the whole check. Prints the resolved dirs.
live_guard() {
  local names=(PCB_CONFIG_DIR PCB_STATE_DIR PCB_LAB_DIR) n res run="" first="" r
  [[ ${PCB_GUARD_PROTON_CAL-} == 1 || -n ${PROTON_CAL_CONFIG_DIR-} ]] && names+=(PROTON_CAL_CONFIG_DIR)
  [[ -n ${XDG_CONFIG_HOME-} ]] && names+=(XDG_CONFIG_HOME)
  [[ -n ${XDG_STATE_HOME-} ]] && names+=(XDG_STATE_HOME)
  [[ -n ${XDG_DATA_HOME-} ]] && names+=(XDG_DATA_HOME)
  local out=()
  for n in "${names[@]}"; do
    res=$(live_guard_dir "$n" "${!n-}") || return 1
    r=${res#"$(realpath -m -- "$(live_home)")"/pcb-live-run/}
    r=${r%%/*}
    if [[ -z $run ]]; then run=$r; first=$n; fi
    if [[ $r != "$run" ]]; then
      live_guard_fail "$n is in run '$r' but $first is in run '$run': one run root only"
      return 1
    fi
    out+=("$n=$res")
  done
  PCB_LIVE_RUN=$(realpath -m -- "$(live_home)")/pcb-live-run/$run
  export PCB_LIVE_RUN
  printf 'live guard ok, run root %s\n' "$PCB_LIVE_RUN" >&2
  printf '  %s\n' "${out[@]}" >&2
}

live_now() { live_hook PCB_LIVE_NOW "$(date +%s)"; }

# live_ledger_check FILE BUDGET NOW [EXTRA]: 0 when a login is allowed by that
# ledger. A non-empty EXTRA (the author's reason, ruling L7) waives the budget,
# never the 60 s spacing.
live_ledger_check() {
  local ledger=$1 budget=$2 now=$3 extra=${4-} count=0 last=0
  if [[ -f $ledger ]]; then
    count=$(grep -c . "$ledger" || true)
    last=$(tail -n 1 "$ledger" | cut -f1)
  fi
  if (( count >= budget )) && [[ -z $extra ]]; then
    live_guard_fail "login budget spent ($count of $budget in $ledger): stop and ask the author"
    return 20
  fi
  if (( count > 0 && now - last < 60 )); then
    live_guard_fail "last login was $((now - last)) s ago: wait $((60 - (now - last))) s (60 s between logins)"
    return 20
  fi
}

# live_login STEP PURPOSE [EXTRA_REASON]: call right before a command that logs in.
# Checks the run's ledger and the global one above the run roots
# (<home>/pcb-live-run/.ledger), under one flock, then records in both.
# EXTRA_REASON (non-empty) is the author's override of the budgets (main and
# global): it is printed loudly and recorded with the login in both ledgers.
# Returns 20 (the lab's "too soon" code) when refused; nothing is recorded then.
live_login() {
  local step=${1-} purpose=${2-} extra=${3-} home base budget gbudget now rc
  if [[ -z $step || -z $purpose ]]; then live_guard_fail "live_login STEP PURPOSE"; return 2; fi
  if [[ -z ${PCB_LIVE_RUN-} ]]; then live_guard >/dev/null || return 1; fi
  home=$(realpath -m -- "$(live_home)") || return 1
  base=$home/pcb-live-run
  budget=$(live_hook PCB_LIVE_LOGIN_BUDGET "$([[ ${PCB_LIVE_VM-} == 1 ]] && echo 3 || echo 11)")
  gbudget=$(live_hook PCB_LIVE_GLOBAL_BUDGET "$([[ ${PCB_LIVE_VM-} == 1 ]] && echo 3 || echo 14)")
  now=$(live_now)
  (
    umask 077
    mkdir -p "$base" "$PCB_LIVE_RUN"
    exec 9>"$base/.ledger.lock"
    flock 9 || exit 1
    live_ledger_check "$PCB_LIVE_RUN/logins.tsv" "$budget" "$now" "$extra" || exit
    live_ledger_check "$base/.ledger" "$gbudget" "$now" "$extra" || exit
    if [[ -n $extra ]]; then
      what="$purpose [EXTRA LOGIN: $extra]"
      printf 'live: EXTRA LOGIN by the author, past the budgets if need be (%s)\n' "$extra" >&2
    fi
    row=$(printf '%s\t%s\t%s\t%s\t%s' "$now" "$(date -u -d "@$now" +%FT%TZ)" "$step" "${what:-$purpose}" "${PCB_LIVE_RUN##*/}")
    printf '%s\n' "${row%$'\t'*}" >> "$PCB_LIVE_RUN/logins.tsv"
    printf '%s\n' "$row" >> "$base/.ledger"
    printf 'live login %s recorded (%s: %s)\n' "$(grep -c . "$PCB_LIVE_RUN/logins.tsv")" "$step" "${what:-$purpose}" >&2
  )
  rc=$?
  return "$rc"
}

# Sourced: check now (return 1 when refused). Executed: check, then exit.
if live_guard; then live_guard_ok=1; else live_guard_ok=0; fi
if [[ ${BASH_SOURCE[0]} == "$0" ]]; then
  [[ $live_guard_ok == 1 ]]; exit
fi
if [[ $live_guard_ok == 1 ]]; then
  unset live_guard_ok
else
  unset live_guard_ok
  return 1
fi
