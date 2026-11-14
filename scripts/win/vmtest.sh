#!/usr/bin/env bash
# Runs go commands on a Windows VM over ssh, on a copy of the working tree
# (tracked and untracked files, not ignored ones).
#
# Usage:
#   PCB_WIN_SSH='-p PORT user@host' scripts/win/vmtest.sh test [-ui] [-race] ARGS...
#   PCB_WIN_SSH='-p PORT user@host' scripts/win/vmtest.sh run 'CMD'
#
# test: go test ARGS in the copy; CGO_ENABLED=0 (as pcb), =1 with -ui (pcb-ui).
#       Each ARG reaches go test verbatim (double-quoted for cmd.exe); an ARG
#       containing a double quote, a percent sign or a newline is refused.
# run:  CMD in the copy, as a raw cmd.exe command line (no quoting is added).
# The VM's remote shell must be cmd.exe (the OpenSSH default on Windows).
set -euo pipefail

usage() {
  sed -n '2,/^set -/p' "${BASH_SOURCE[0]}" | sed '$d; s/^# \{0,1\}//' >&2
  exit 2
}

[[ $# -ge 1 ]] || usage
mode=$1; shift
case $mode in test | run) ;; *) usage ;; esac
if [[ -z ${PCB_WIN_SSH:-} ]]; then
  echo "vmtest.sh: set PCB_WIN_SSH='-p PORT user@host' (the ssh arguments of the Windows VM)" >&2
  exit 2
fi
read -ra target <<<"$PCB_WIN_SSH"

# stdout only: ssh may print warnings on stderr.
os=$(ssh "${target[@]}" 'echo %OS%' 2>/dev/null | tr -d '\r' || true)
if [[ $os != Windows_NT ]]; then
  echo "vmtest.sh: the VM's remote shell must be cmd.exe (echo %OS% printed '$os', want Windows_NT)" >&2
  exit 2
fi

# No local path leaves the host: the directory name is a hash of it.
dir=pcb-vmtest-$(printf %s "$PWD" | sha256sum | cut -c1-8)

# The remote command is built on the client on purpose (SC2029).
# shellcheck disable=SC2029
# Tracked files deleted in the working tree are left out (tar cannot stat them).
comm -z -23 <(git ls-files -co --exclude-standard -z | sort -zu) <(git ls-files -d -z | sort -zu) |
  tar --null -T - -cf - |
  ssh "${target[@]}" "(if exist $dir rmdir /s /q $dir) & mkdir $dir && tar -xf - -C $dir"

# shellcheck disable=SC2029
case $mode in
test)
  cgo=0
  words=()
  for a in "$@"; do
    case $a in
    -ui) cgo=1 ;;
    *'"'* | *%* | *$'\n'*)
      echo "vmtest.sh: argument '$a' contains a double quote, a percent sign or a newline: cmd.exe cannot carry it verbatim" >&2
      exit 2
      ;;
    *) words+=("\"$a\"") ;;
    esac
  done
  cmdline=
  for w in ${words[@]+"${words[@]}"}; do cmdline+=" $w"; done
  ssh "${target[@]}" "cd $dir && set \"CGO_ENABLED=$cgo\" && go test$cmdline" 2> >(grep -v -i 'post-quantum\|vulnerable\|openssh.com/pq' >&2)
  ;;
run)
  [[ $# -eq 1 ]] || usage
  ssh "${target[@]}" "cd $dir && $1" 2> >(grep -v -i 'post-quantum\|vulnerable\|openssh.com/pq' >&2)
  ;;
esac
