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
# $HOME/pcb-live-run/<name>/ (the run root; <name> is the run, e.g. a date),
# all four under the same run root. XDG_CONFIG_HOME, XDG_STATE_HOME and
# XDG_DATA_HOME are checked the same way when set (a stray default would
# otherwise reach ~/.config/pcb, ~/.local/state/pcb, ~/.local/share/pcb or
# ~/.config/proton-cal, the author's real ones).
# On success it prints the resolved dirs and exports PCB_LIVE_RUN, the run root.
#
# It also defines live_login STEP PURPOSE, the login ledger: it refuses a
# login within 60 s of the last one or beyond the budget (11 by default,
# PCB_LIVE_LOGIN_BUDGET), else records it in $PCB_LIVE_RUN/logins.tsv.
# Bash and GNU realpath only. The guard reads no file of the dirs it checks.

live_guard_fail() { printf 'live guard: %s\n' "$*" >&2; }

# live_guard_dir NAME VALUE: prints the resolved dir, or fails with the reason.
live_guard_dir() {
  local name=$1 value=${2-} home base res
  if [[ -z $value ]]; then live_guard_fail "$name is not set"; return 1; fi
  if [[ $value != /* ]]; then live_guard_fail "$name=$value is not an absolute path"; return 1; fi
  if [[ $value == *$'\n'* ]]; then live_guard_fail "$name holds a newline"; return 1; fi
  home=$(realpath -m -- "$HOME") || return 1
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
    r=${res#"$(realpath -m -- "$HOME")"/pcb-live-run/}
    r=${r%%/*}
    if [[ -z $run ]]; then run=$r; first=$n; fi
    if [[ $r != "$run" ]]; then
      live_guard_fail "$n is in run '$r' but $first is in run '$run': one run root only"
      return 1
    fi
    out+=("$n=$res")
  done
  PCB_LIVE_RUN=$(realpath -m -- "$HOME")/pcb-live-run/$run
  export PCB_LIVE_RUN
  printf 'live guard ok, run root %s\n' "$PCB_LIVE_RUN"
  printf '  %s\n' "${out[@]}"
}

live_now() { printf '%s\n' "${PCB_LIVE_NOW:-$(date +%s)}"; }

# live_login STEP PURPOSE: call right before a command that logs in.
# Returns 20 (the lab's "too soon" code) when refused; nothing is recorded then.
live_login() {
  local step=${1-} purpose=${2-} ledger budget=${PCB_LIVE_LOGIN_BUDGET:-11} now last count
  if [[ -z $step || -z $purpose ]]; then live_guard_fail "live_login STEP PURPOSE"; return 2; fi
  if [[ -z ${PCB_LIVE_RUN-} ]]; then live_guard >/dev/null || return 1; fi
  ledger=$PCB_LIVE_RUN/logins.tsv
  now=$(live_now)
  count=0 last=0
  if [[ -f $ledger ]]; then
    count=$(grep -c . "$ledger" || true)
    last=$(tail -n 1 "$ledger" | cut -f1)
  fi
  if (( count >= budget )); then
    live_guard_fail "login budget spent ($count of $budget): stop and ask the author"
    return 20
  fi
  if (( count > 0 && now - last < 60 )); then
    live_guard_fail "last login was $((now - last)) s ago: wait $((60 - (now - last))) s (60 s between logins)"
    return 20
  fi
  (umask 077; mkdir -p "$PCB_LIVE_RUN"; printf '%s\t%s\t%s\t%s\n' "$now" "$(date -u -d "@$now" +%FT%TZ)" "$step" "$purpose" >> "$ledger")
  printf 'live login %s of %s recorded (%s: %s)\n' "$((count + 1))" "$budget" "$step" "$purpose"
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
