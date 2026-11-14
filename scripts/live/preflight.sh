#!/usr/bin/env bash
# Preflight for the live session (live-readiness R1). Calls no Proton API and
# logs nobody in.
#
# Usage: scripts/live/preflight.sh RUN_ROOT [--step7]
#   RUN_ROOT  $HOME/pcb-live-run/<run>, e.g. ~/pcb-live-run/2026-11-14
#   --step7   this host will run step 7 (reboot, network cut, --delete-data):
#             your workstation is then refused, not just warned about.
# The hosts it refuses come from $HOME/.config/pcb-live/hosts (common.sh); it
# refuses to run without that file.
# Exit 0: every check passed (warnings may print); 1: at least one FAIL.
set -uo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=common.sh
. "$here/common.sh"
uhome=$(live_home) || exit 2
root=${1-}
step7=0
[[ ${2-} == --step7 ]] && step7=1
fails=0
ok()   { printf 'ok    %s\n' "$*"; }
warn() { printf 'WARN  %s\n' "$*"; }
fail() { printf 'FAIL  %s\n' "$*"; fails=$((fails + 1)); }

if [[ -z $root ]]; then
  echo "usage: preflight.sh RUN_ROOT [--step7]   (RUN_ROOT = \$HOME/pcb-live-run/<run>)" >&2
  exit 2
fi
# The hosts file first: without it nothing is checked (fail closed).
host_role=$(live_host_role) || { printf '\npreflight: refused: nothing was checked\n'; exit 1; }

# The standard layout of a run root; HUMAN-CHECKS names the same dirs.
export PCB_CONFIG_DIR=$root/pcb/config
export PCB_STATE_DIR=$root/pcb/state
export PCB_LAB_DIR=$root/lab/2a
export PROTON_CAL_CONFIG_DIR=$root/seed
export PCB_GUARD_PROTON_CAL=1
unset XDG_CONFIG_HOME XDG_STATE_HOME XDG_DATA_HOME

# 1. The guard accepts the layout and refuses the author's real dirs.
# shellcheck source=guard.sh
if guard_out=$(. "$here/guard.sh" 2>&1); then
  ok "guard accepts the run-root layout"
  printf '%s\n' "$guard_out" | sed 's/^/        /'
else
  fail "guard refuses the run-root layout: $guard_out"
fi
for bad in "$uhome/.config/pcb" "$uhome/.config/proton-cal" "$uhome/.local/state/pcb" "$uhome/.local/share/pcb"; do
  # shellcheck source=guard.sh
  if (PCB_STATE_DIR=$bad; . "$here/guard.sh") >/dev/null 2>&1; then
    fail "guard ACCEPTS $bad"
  fi
done
ok "guard refuses the author's real dirs"

# 2. Binaries, built into the run root (never ~/.local/bin).
tools=(pcb pcb-lab proton-cal)
(( step7 )) && tools=(pcb)   # step 7 runs pcb only
for b in "${tools[@]}"; do
  p=$root/bin/$b
  if [[ ! -x $p ]]; then
    fail "$p missing (build it: HUMAN-CHECKS 'Build into the run root')"
  elif [[ $b == pcb ]]; then
    v=$("$p" version 2>&1 | head -n 1)
    if [[ $v == *dev* ]]; then fail "$p is not stamped ($v): use make build"; else ok "$b: $v"; fi
  else
    ok "$b: sha256 $(sha256sum "$p" | cut -c1-12)"
  fi
done

# 3. Free ports (pcb serve 1080, TLS 8443).
for port in 1080 8443; do
  if (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then
    fail "port $port is already in use"
  else
    ok "port $port is free"
  fi
done

# 4. The host.
host=$(live_host_name)
case $host_role in
  production) fail "host $host is your production server ($(live_hosts_file)): never run anything live here" ;;
  workstation)
    if (( step7 )); then
      fail "host $host is your workstation: step 7 reboots, cuts the network and deletes data; use a throwaway VM"
    else
      warn "host $host is your workstation: fine for steps 1-6; step 7 (reboot, network cut, --delete-data) must not run here"
    fi ;;
  *) ok "host $host (neither production nor workstation in $(live_hosts_file))" ;;
esac

# 4b. Step 7 wants a host that has never run pcb (7.1) and deletes data (7.6).
if (( step7 )); then
  if vmerr=$(live_vm_check 2>&1); then ok "step 7: VM marker, virtualisation and host name accepted"; else fail "step 7: $vmerr"; fi
  seen=0
  for d in "$uhome/.config/pcb" "$uhome/.local/state/pcb" "$uhome/.config/systemd/user/pcb.service"; do
    if [[ -e $d ]]; then fail "$d exists: this host has run pcb; step 7 needs a throwaway VM that never did"; seen=1; fi
  done
  if (( ! seen )); then ok "step 7: no pcb dir or unit on this host yet"; fi
fi

# 5. Login budget and ledger.
budget=$(( step7 ? 3 : 11 ))
budget=$(live_hook PCB_LIVE_LOGIN_BUDGET "$budget")
ledger=$root/logins.tsv
if [[ -f $ledger ]]; then
  used=$(grep -c . "$ledger" || true)
  last=$(tail -n 1 "$ledger" | cut -f2)
  ok "login budget: $used of $budget used, last at $last"
  (( used >= budget )) && fail "login budget spent"
else
  ok "login budget: 0 of $budget used (ledger $ledger)"
fi
echo "      this host: 10 planned (2a 2 with its fresh CAPTCHA, 2b-2d 3, E01 1, seed 1, M1 3) + 1 spare (M2); step 7 adds 3 on its VM (own ledger, PCB_LIVE_LOGIN_BUDGET=3): 13-14 in all; 60 s between logins; the author stays reachable for CAPTCHAs"

if (( fails )); then
  printf '\npreflight: %d FAIL: do not start the live session\n' "$fails"
  exit 1
fi
printf '\npreflight: ok\n'
