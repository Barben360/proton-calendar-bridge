# shellcheck shell=bash
# Shared by guard.sh, run.sh and preflight.sh. Defines functions only.
#
# Test hooks (PCB_LIVE_HOME, PCB_LIVE_NOW, PCB_LIVE_HOSTNAME, PCB_LIVE_VIRT,
# PCB_LIVE_VM_MARKER, PCB_LIVE_LOGIN_BUDGET, PCB_LIVE_GLOBAL_BUDGET) are honoured
# ONLY when PCB_LIVE_TEST=1; otherwise they are ignored.

live_test() { [[ ${PCB_LIVE_TEST-} == 1 ]]; }

# live_hook NAME DEFAULT: the value of the test hook NAME when testing, else DEFAULT.
live_hook() {
  if live_test && [[ -n ${!1-} ]]; then printf '%s\n' "${!1}"; else printf '%s\n' "$2"; fi
}

# live_home: the account's home from the user database, not $HOME (which a
# caller can change).
live_home() {
  local h
  if live_test && [[ -n ${PCB_LIVE_HOME-} ]]; then printf '%s\n' "$PCB_LIVE_HOME"; return; fi
  h=$(getent passwd "$(id -un)" | cut -d: -f6)
  [[ $h == /* ]] || { printf 'live: cannot find the home of %s\n' "$(id -un)" >&2; return 1; }
  printf '%s\n' "$h"
}

# live_vm_check: step 7's fence. The marker the author creates on the VM
# (/etc/pcb-live-vm containing "throwaway"), a VM or container per
# systemd-detect-virt, and a host name that is not morpheus or blackwater.
live_vm_check() {
  local marker virt host
  marker=$(live_hook PCB_LIVE_VM_MARKER /etc/pcb-live-vm)
  if [[ ! -r $marker ]] || ! grep -qx 'throwaway' "$marker"; then
    echo "live: $marker missing or without the word 'throwaway': the author creates it on the VM (sudo sh -c 'echo throwaway > /etc/pcb-live-vm')" >&2
    return 1
  fi
  if live_test && [[ -n ${PCB_LIVE_VIRT-} ]]; then
    virt=$PCB_LIVE_VIRT
  else
    virt=$(systemd-detect-virt 2>/dev/null) || virt=none
  fi
  if [[ -z $virt || $virt == none ]]; then
    echo "live: systemd-detect-virt reports no VM or container (or is missing): refused" >&2
    return 1
  fi
  host=$(live_hook PCB_LIVE_HOSTNAME "$(hostname -s 2>/dev/null || hostname)")
  host=${host,,}
  case $host in
    morpheus* | blackwater*) echo "live: host $host is not a throwaway VM: refused" >&2; return 1 ;;
  esac
}
