# shellcheck shell=bash
# Shared by guard.sh, run.sh and preflight.sh. Defines functions only.
#
# Test hooks (PCB_LIVE_HOME, PCB_LIVE_NOW, PCB_LIVE_HOSTNAME, PCB_LIVE_HOSTS,
# PCB_LIVE_VIRT, PCB_LIVE_VM_MARKER, PCB_LIVE_LOGIN_BUDGET, PCB_LIVE_GLOBAL_BUDGET) are honoured
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

# The hosts live commands refuse are not named here: they are listed in a local,
# untracked file, $HOME/.config/pcb-live/hosts (PCB_LIVE_HOSTS in test mode
# only). One host per line, "#" starts a comment:
#   production <hostname>    never a live host, in every mode
#   workstation <hostname>   the machine you run the session from: refused for
#                            --vm, warned about by preflight (refused for step 7)
# An empty file means no such hosts; a missing or unreadable file refuses
# (fail closed). Names match case-insensitively on their first label, so an
# FQDN matches its short name.
live_hosts_file() { live_hook PCB_LIVE_HOSTS "${HOME-}/.config/pcb-live/hosts"; }

# live_host_name: this host's short name, lower case.
live_host_name() {
  local host
  host=$(live_hook PCB_LIVE_HOSTNAME "$(hostname -s 2>/dev/null || hostname)")
  host=${host,,}
  printf '%s\n' "${host%%.*}"
}

# live_host_role: prints this host's role in the hosts file (production,
# workstation, or an empty line when unlisted). Fails, printing why and how to
# create the file, when the file is missing, unreadable or malformed.
live_host_role() {
  local file host line role name extra n=0 found=""
  file=$(live_hosts_file)
  if [[ ! -f $file || ! -r $file ]]; then
    printf 'live: %s is missing or unreadable: it lists the hosts live commands refuse. Create it (an empty file means none):\n' "$file" >&2
    printf '%s\n' "  mkdir -p ~/.config/pcb-live && printf 'production <name>\nworkstation <name>\n' > ~/.config/pcb-live/hosts" >&2
    return 1
  fi
  host=$(live_host_name)
  [[ -n $host ]] || { echo "live: cannot read this host's name: refused" >&2; return 1; }
  while IFS= read -r line || [[ -n $line ]]; do
    n=$((n + 1))
    read -r role name extra <<<"${line%%#*}"
    [[ -z ${role-} ]] && continue
    if [[ ($role != production && $role != workstation) || -z ${name-} || -n ${extra-} ]]; then
      printf "live: %s line %d: want 'production <hostname>' or 'workstation <hostname>': refused\n" "$file" "$n" >&2
      return 1
    fi
    name=${name,,}
    if [[ ${name%%.*} == "$host" ]]; then
      if [[ $role == production ]]; then found=production; elif [[ -z $found ]]; then found=workstation; fi
    fi
  done <"$file"
  printf '%s\n' "$found"
}

# live_host_check [all]: refuses the production host always, the workstation
# too when $1 is "all" (--vm). Prints why on failure.
live_host_check() {
  local role host
  role=$(live_host_role) || return 1
  host=$(live_host_name)
  case $role in
    production) echo "live: host $host is your production server ($(live_hosts_file)): never a live host: refused" >&2; return 1 ;;
    workstation) if [[ ${1-} == all ]]; then echo "live: host $host is your workstation ($(live_hosts_file)), not a throwaway VM: refused" >&2; return 1; fi ;;
  esac
}

# live_vm_check: step 7's fence. The marker the author creates on the VM
# (/etc/pcb-live-vm containing "throwaway"), a VM or container per
# systemd-detect-virt, and a host that the hosts file lists neither as
# production nor as workstation.
live_vm_check() {
  local marker virt
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
  live_host_check all
}
