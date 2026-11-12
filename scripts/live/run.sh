#!/usr/bin/env bash
# One guarded command for the live session (live-readiness R1, G10): sets the
# run-root dirs, sources guard.sh, optionally records a login in the ledger,
# then runs the command. Because it is ONE command, a split run cannot lose the
# variables and fall back to ~/.config/pcb or ~/.local/state/pcb.
#
# Usage:
#   scripts/live/run.sh --run NAME [--lab DIR] [--pcb DIR] [--proton-cal]
#                       [--vm] [--login STEP PURPOSE] -- COMMAND [ARGS...]
#
#   --run NAME     the run: dirs live under $HOME/pcb-live-run/NAME/ (e.g. a date)
#   --lab DIR      PCB_LAB_DIR=$R/lab/DIR, what the command passes as `--state-dir`
#                  (default "unused")
#   --pcb DIR      PCB_CONFIG_DIR=$R/DIR/config, PCB_STATE_DIR=$R/DIR/state
#                  (default "pcb": the session M1, M2 and step 6 share)
#   --proton-cal   PROTON_CAL_CONFIG_DIR is required: it is $R/seed
#   --login S P    call live_login S P first: refused within 60 s of the last
#                  login or beyond the budget (exit 20), else recorded
#   --vm           step 7 only, on its throwaway VM: pcb runs with the host's
#                  default dirs there (that is what `pcb install` tests), so the dir
#                  guard cannot apply and PCB_*_DIR are removed from the command's
#                  environment; instead it requires the marker /etc/pcb-live-vm
#                  ("throwaway", created by the author on the VM) and a VM or
#                  container per systemd-detect-virt, refuses blackwater and
#                  morpheus by host name, and the ledger keeps a budget of 3
# In the arguments, @RUN@ is replaced by $R and @LAB@ by $R/lab/DIR (the
# calling shell cannot know them).
# The command sees PCB_CONFIG_DIR, PCB_STATE_DIR, PCB_LAB_DIR, PROTON_CAL_CONFIG_DIR
# and PCB_LIVE_RUN, and finds pcb, pcb-lab and proton-cal in $R/bin first. The
# XDG_* variables are removed. Credentials stay the caller's business
# (pass-cli run --env-file ... --): this script reads and passes none.
set -uo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
name="" lab=unused pcbdir=pcb proton=0 vm=0 step="" purpose=""
usage() { echo "usage: run.sh --run NAME [--lab DIR] [--pcb DIR] [--proton-cal] [--vm] [--login STEP PURPOSE] -- COMMAND [ARGS...]" >&2; exit 2; }
while (( $# )); do
  case $1 in
    --run) name=${2-}; shift 2 || usage ;;
    --lab) lab=${2-}; shift 2 || usage ;;
    --pcb) pcbdir=${2-}; shift 2 || usage ;;
    --proton-cal) proton=1; shift ;;
    --vm) vm=1; shift ;;
    --login) step=${2-}; purpose=${3-}; shift 3 || usage ;;
    --) shift; break ;;
    *) usage ;;
  esac
done
(( $# )) || usage
for v in "$name" "$lab" "$pcbdir"; do
  [[ $v =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]] || { echo "run.sh: '$v' is not a plain name (letters, digits, . _ -)" >&2; exit 2; }
done

# shellcheck source=common.sh
. "$here/common.sh"
if (( vm )); then
  live_vm_check || { echo "run.sh --vm: refused, nothing was run" >&2; exit 1; }
  export PCB_LIVE_VM=1
fi
home=$(live_home) || exit 1

R=$home/pcb-live-run/$name
export PCB_CONFIG_DIR=$R/$pcbdir/config
export PCB_STATE_DIR=$R/$pcbdir/state
export PCB_LAB_DIR=$R/lab/$lab
export PROTON_CAL_CONFIG_DIR=$R/seed
(( proton )) && export PCB_GUARD_PROTON_CAL=1
unset XDG_CONFIG_HOME XDG_STATE_HOME XDG_DATA_HOME

# shellcheck source=guard.sh
. "$here/guard.sh" || { echo "run.sh: the guard refused, nothing was run" >&2; exit 1; }

# The first word whose basename is one of our tools must be the run root's
# build ($R/bin/<tool>, bare or by that absolute path), also when the command
# is wrapped (env ..., pass-cli run ... --).
for w in "$@"; do
  w=${w//@RUN@/$PCB_LIVE_RUN}
  case ${w##*/} in
    pcb | pcb-lab | proton-cal)
      if [[ $w == */* && $w != "$PCB_LIVE_RUN/bin/${w##*/}" ]]; then
        echo "run.sh: $w is not the run root's build ($PCB_LIVE_RUN/bin/${w##*/}): refused" >&2; exit 1
      fi
      [[ -x $PCB_LIVE_RUN/bin/${w##*/} ]] || { echo "run.sh: $PCB_LIVE_RUN/bin/${w##*/} is missing: build it into the run root first" >&2; exit 1; }
      break ;;
  esac
done
if [[ -n $step ]]; then live_login "$step" "$purpose" || exit; fi
args=()
for a in "$@"; do
  a=${a//@RUN@/$PCB_LIVE_RUN}
  args+=("${a//@LAB@/$PCB_LAB_DIR}")
done
if (( vm )); then
  unset PCB_CONFIG_DIR PCB_STATE_DIR PCB_LAB_DIR PROTON_CAL_CONFIG_DIR
else
  (umask 077; mkdir -p "$PCB_CONFIG_DIR" "$PCB_STATE_DIR" "$PCB_LAB_DIR"; if (( proton )); then mkdir -p "$PROTON_CAL_CONFIG_DIR"; fi)
fi
PATH=$PCB_LIVE_RUN/bin:$PATH exec "${args[@]}"
