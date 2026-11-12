#!/usr/bin/env bash
# Test of guard.sh (bats-free). Runs against a throwaway HOME: it never looks at
# the real home. Prints PASS last; exits 1 on the first failed expectation.
# Usage: scripts/live/guard_test.sh        (make live-preflight-test)
set -uo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
export HOME=$tmp/home
mkdir -p "$HOME"
R=$HOME/pcb-live-run/t1
failures=0
# The hosts file (PCB_LIVE_HOSTS): one production and one workstation host, generic names.
hosts=$tmp/hosts
printf '# hosts live commands refuse\nproduction prod-host\nworkstation ws-host   # the session machine\n' > "$hosts"

# envi VAR=VALUE... CMD...: a clean environment in test mode, the throwaway home as the account home.
envi() { env -i HOME="$HOME" PATH="$PATH" PCB_LIVE_TEST=1 PCB_LIVE_HOME="$HOME" PCB_LIVE_HOSTS="$hosts" "$@"; }

# check LABEL ACTUAL WANT: equal strings or a failure.
check() {
  if [[ $2 == "$3" ]]; then echo "ok   $1"; else echo "FAIL $1: got '$2', want '$3'"; failures=$((failures + 1)); fi
}

# run_guard NAME=VALUE...: sources the guard in a clean bash with exactly these variables.
run_guard() {
  # shellcheck disable=SC2016 # $1 is for the inner bash
  envi "$@" bash -c '. "$1"' _ "$here/guard.sh"
}
good=(PCB_CONFIG_DIR="$R/pcb/config" PCB_STATE_DIR="$R/pcb/state" PCB_LAB_DIR="$R/lab/2a")

accepts() { # LABEL VARS...
  local label=$1; shift
  if out=$(run_guard "$@" 2>&1); then echo "ok   accepts $label"; else
    echo "FAIL should accept $label: $out"; failures=$((failures + 1)); fi
}
refuses() { # LABEL WANT_IN_MESSAGE VARS...
  local label=$1 want=$2; shift 2
  if out=$(run_guard "$@" 2>&1); then
    echo "FAIL should refuse $label, it said: $out"; failures=$((failures + 1))
  elif [[ $out != *"$want"* ]]; then
    echo "FAIL refused $label but without '$want': $out"; failures=$((failures + 1))
  else echo "ok   refuses $label"; fi
}
with() { # VAR=VALUE: the good set with one variable replaced
  local k=${1%%=*} v=() x
  for x in "${good[@]}"; do [[ $x == "$k="* ]] || v+=("$x"); done
  printf '%s\0' "${v[@]}" "$1"
}
refuses_set() { # LABEL WANT VAR=VALUE
  local label=$1 want=$2 args=()
  mapfile -d '' args < <(with "$3")
  refuses "$label" "$want" "${args[@]}"
}

accepts "a run-root layout" "${good[@]}"
accepts "a run-root layout with proton-cal" "${good[@]}" PCB_GUARD_PROTON_CAL=1 PROTON_CAL_CONFIG_DIR="$R/seed"

# The author's real dirs, each as PCB_STATE_DIR, PCB_CONFIG_DIR and PCB_LAB_DIR.
for real in .config/pcb .config/proton-cal .local/state/pcb .local/share/pcb .local/state/pcb-lab \
            .local/share/pcb-m1-live .config .local/state .local/bin; do
  for var in PCB_CONFIG_DIR PCB_STATE_DIR PCB_LAB_DIR; do
    refuses_set "$var=~/$real" "outside" "$var=$HOME/$real"
  done
done
refuses "PROTON_CAL_CONFIG_DIR=~/.config/proton-cal" "outside" "${good[@]}" PROTON_CAL_CONFIG_DIR="$HOME/.config/proton-cal"
refuses "proton-cal unset when needed" "PROTON_CAL_CONFIG_DIR is not set" "${good[@]}" PCB_GUARD_PROTON_CAL=1
refuses "XDG_CONFIG_HOME at the default" "outside" "${good[@]}" XDG_CONFIG_HOME="$HOME/.config"
refuses "XDG_STATE_HOME at the default" "outside" "${good[@]}" XDG_STATE_HOME="$HOME/.local/state"

# Shape.
refuses_set "a relative path" "not an absolute path" "PCB_STATE_DIR=pcb-live-run/t1/state"
refuses_set "a tilde that was never expanded" "not an absolute path" "PCB_STATE_DIR=~/pcb-live-run/t1/state"
refuses_set "an empty variable" "is not set" "PCB_STATE_DIR="
refuses "an unset PCB_CONFIG_DIR" "PCB_CONFIG_DIR is not set" PCB_STATE_DIR="$R/pcb/state" PCB_LAB_DIR="$R/lab/2a"
refuses "an unset PCB_STATE_DIR" "PCB_STATE_DIR is not set" PCB_CONFIG_DIR="$R/pcb/config" PCB_LAB_DIR="$R/lab/2a"
refuses "an unset PCB_LAB_DIR" "PCB_LAB_DIR is not set" PCB_CONFIG_DIR="$R/pcb/config" PCB_STATE_DIR="$R/pcb/state"
refuses_set "the run root itself" "not the run root itself" "PCB_STATE_DIR=$R"
refuses_set "pcb-live-run itself" "not the run root itself" "PCB_STATE_DIR=$HOME/pcb-live-run"
refuses_set "\$HOME" "outside" "PCB_STATE_DIR=$HOME"
refuses_set "a .. escape out of the run root" "outside" "PCB_STATE_DIR=$R/../../.config/pcb"
refuses_set "a sibling prefix of the run root" "outside" "PCB_STATE_DIR=$HOME/pcb-live-run-evil/t1/state"
refuses "two different runs" "one run root only" PCB_CONFIG_DIR="$R/pcb/config" PCB_STATE_DIR="$HOME/pcb-live-run/t2/state" PCB_LAB_DIR="$R/lab/2a"

# Symlinks are resolved: a link inside the run root that points at a real dir.
mkdir -p "$R/pcb" "$HOME/.config/pcb"
ln -s "$HOME/.config/pcb" "$R/pcb/linked"
refuses_set "a symlink into ~/.config/pcb" "outside" "PCB_CONFIG_DIR=$R/pcb/linked"
ln -s "$HOME/.config" "$R/cfg"
refuses_set "a symlink to ~/.config, a sub-dir of it" "outside" "PCB_CONFIG_DIR=$R/cfg/pcb"
ln -s "$HOME/pcb-live-run/t1" "$HOME/via-link"
accepts "a link that resolves inside the run root (the resolved path is what counts)" "${good[@]}" PCB_STATE_DIR="$HOME/via-link/state"

# The guard prints what it resolved.
out=$(run_guard "${good[@]}" 2>&1)
check "prints the resolved dirs" "$(grep -c "PCB_STATE_DIR=$R/pcb/state" <<<"$out")" 1

# The login ledger.
ledger_run() { # NOW STEP PURPOSE [BUDGET]
  # shellcheck disable=SC2016 # $1..$3 are for the inner bash
  envi "${good[@]}" PCB_LIVE_NOW="$1" PCB_LIVE_LOGIN_BUDGET="${4:-3}" \
    bash -c '. "$1" >/dev/null && live_login "$2" "$3"' _ "$here/guard.sh" "$2" "$3" 2>&1
}
ledger_expect() { # LABEL WANT_RC OUTPUT_PART NOW STEP PURPOSE [BUDGET]
  local label=$1 want=$2 part=$3; shift 3
  out=$(ledger_run "$@"); rc=$?
  if [[ $rc != "$want" || $out != *"$part"* ]]; then
    echo "FAIL ledger $label: rc=$rc (want $want), out: $out"; failures=$((failures + 1))
  else echo "ok   ledger $label"; fi
}
ledger_expect "first login is recorded" 0 "login 1 recorded" 1000 2a "Other, solved token"
ledger_expect "a login 30 s later is refused" 20 "wait 30 s" 1030 2b "too soon"
ledger_expect "a login 60 s later is recorded" 0 "login 2 recorded" 1060 2b "linux-mail"
ledger_expect "a refused login is not recorded" 0 "login 3 recorded" 1120 2c "web-calendar"
ledger_expect "beyond the budget is refused" 20 "budget spent (3 of 3" 9999 2d "one too many"
check "ledger holds exactly the three recorded logins" "$(grep -c . "$R/logins.tsv")" 3
check "ledger keeps step order" "$(cut -f3 "$R/logins.tsv" | paste -sd,)" "2a,2b,2c"

# run.sh: the one-command form. A fake pcb in the run root prints what it was given.
mkdir -p "$HOME/pcb-live-run/t3/bin"
cat > "$HOME/pcb-live-run/t3/bin/pcb" <<'FAKE'
#!/usr/bin/env bash
echo "pcb sees config=$PCB_CONFIG_DIR state=$PCB_STATE_DIR lab=$PCB_LAB_DIR xdg=${XDG_STATE_HOME-unset} args=$*"
FAKE
chmod +x "$HOME/pcb-live-run/t3/bin/pcb"
runsh() { envi "$@" ; }
out=$(runsh XDG_STATE_HOME="$HOME/.local/state" "$here/run.sh" --run t3 --lab 2b -- pcb status 2>&1); rc=$?
check "run.sh runs the command with the run-root dirs and no XDG" "$rc $(grep 'pcb sees' <<<"$out")" "0 pcb sees config=$HOME/pcb-live-run/t3/pcb/config state=$HOME/pcb-live-run/t3/pcb/state lab=$HOME/pcb-live-run/t3/lab/2b xdg=unset args=status"
out=$(runsh "$here/run.sh" --run t3 --lab 2c -- pcb --state-dir @LAB@ --rec @RUN@/x 2>&1)
check "run.sh expands @LAB@ and @RUN@ in the arguments" "$(grep -o 'args=.*' <<<"$out")" "args=--state-dir $HOME/pcb-live-run/t3/lab/2c --rec $HOME/pcb-live-run/t3/x"
out=$(runsh "$here/run.sh" --run '../../.config' -- pcb status 2>&1); rc=$?
check "run.sh refuses a run name that is a path" "$rc" 2
out=$(runsh "$here/run.sh" --run t3 --lab '../..' -- pcb status 2>&1); rc=$?
check "run.sh refuses a lab name that is a path" "$rc" 2
out=$(runsh "$here/run.sh" --run t4 -- pcb status 2>&1); rc=$?
check "run.sh refuses a pcb missing from the run root" "$rc" 1
ln -s "$HOME/.config/pcb" "$HOME/pcb-live-run/t3/linkdir"
mkdir -p "$HOME/pcb-live-run/t5/bin" "$HOME/.config/pcb"
cp "$HOME/pcb-live-run/t3/bin/pcb" "$HOME/pcb-live-run/t5/bin/pcb"
ln -s "$HOME/.config/pcb" "$HOME/pcb-live-run/t5/pcb"
out=$(runsh "$here/run.sh" --run t5 -- pcb status 2>&1); rc=$?
check "run.sh refuses a run whose pcb dir is a link to ~/.config/pcb" "$rc" 1
check "run.sh did not run pcb there" "$(grep -c 'pcb sees' <<<"$out")" 0
# M3: a wrapped tool is found by basename, also by absolute path; only the run root's build is allowed.
out=$(runsh "$here/run.sh" --run t3 -- env pcb status 2>&1); rc=$?
check "run.sh runs a wrapped bare pcb from the run root" "$rc" 0
out=$(runsh "$here/run.sh" --run t3 -- env "$HOME/pcb-live-run/t3/bin/pcb" status 2>&1); rc=$?
check "run.sh accepts the run root's absolute pcb" "$rc" 0
out=$(runsh "$here/run.sh" --run t3 -- env /usr/local/bin/pcb status 2>&1); rc=$?
check "run.sh refuses a wrapped absolute pcb elsewhere" "$rc" 1
out=$(runsh "$here/run.sh" --run t3 -- env /home/x/.local/bin/pcb-lab login 2>&1); rc=$?
check "run.sh refuses a wrapped absolute pcb-lab elsewhere" "$rc" 1
out=$(runsh "$here/run.sh" --run t3 -- env pcb-lab login 2>&1); rc=$?
check "run.sh refuses a wrapped pcb-lab missing from the run root" "$rc" 1

runsh PCB_LIVE_NOW=2000 "$here/run.sh" --run t3 --login 2a 'first' -- pcb status >/dev/null 2>&1
out=$(runsh PCB_LIVE_NOW=2010 "$here/run.sh" --run t3 --login 2b 'too soon' -- pcb status 2>&1); rc=$?
check "run.sh --login refuses a second login at once (exit 20)" "$rc" 20
check "run.sh --login did not run the command when refused" "$(grep -c 'pcb sees' <<<"$out")" 0

# run.sh --vm: step 7's VM keeps the host's default dirs; the fence is a marker, virt and host name.
vmhome=$tmp/vmhome; mkdir -p "$vmhome"
echo throwaway > "$tmp/marker"; echo nope > "$tmp/badmarker"
vm() { # HOSTNAME VIRT MARKER NOW RUN [run.sh args after --run RUN]
  local host=$1 virt=$2 marker=$3 now=$4 run=$5; shift 5
  env -i HOME="$vmhome" PATH="$PATH" PCB_LIVE_TEST=1 PCB_LIVE_HOME="$vmhome" PCB_LIVE_HOSTS="$hosts" PCB_LIVE_HOSTNAME="$host" PCB_LIVE_VIRT="$virt" \
    PCB_LIVE_VM_MARKER="$marker" PCB_LIVE_NOW="$now" "$here/run.sh" --vm --run "$run" "$@" 2>&1
}
out=$(vm ws-host kvm "$tmp/marker" 1 v1 -- pcb status); rc=$?
check "run.sh --vm refuses the workstation" "$rc $(grep -c 'workstation' <<<"$out")" "1 1"
out=$(vm PROD-HOST kvm "$tmp/marker" 1 v1 -- pcb status); rc=$?
check "run.sh --vm refuses the production host in any case" "$rc $(grep -c 'production' <<<"$out")" "1 1"
out=$(vm WS-Host.lan kvm "$tmp/marker" 1 v1 -- pcb status); rc=$?
check "run.sh --vm refuses a capitalised workstation FQDN" "$rc $(grep -c 'workstation' <<<"$out")" "1 1"
out=$(vm prod-host.example.org kvm "$tmp/marker" 1 v1 -- pcb status); rc=$?
check "run.sh --vm refuses a production FQDN" "$rc $(grep -c 'production' <<<"$out")" "1 1"
out=$(vm deb13-vm kvm "$tmp/nomarker" 1 v1 -- true); rc=$?
check "run.sh --vm refuses a missing marker" "$rc" 1
out=$(vm deb13-vm kvm "$tmp/badmarker" 1 v1 -- true); rc=$?
check "run.sh --vm refuses a marker without 'throwaway'" "$rc" 1
out=$(vm deb13-vm none "$tmp/marker" 1 v1 -- true); rc=$?
check "run.sh --vm refuses systemd-detect-virt none" "$rc" 1
out=$(vm deb13-vm kvm "$tmp/marker" 1 v1 -- env); rc=$?
check "run.sh --vm on a marked VM exports no pcb dir" "$rc $(grep -cE '^(PCB_CONFIG_DIR|PCB_STATE_DIR|PCB_LAB_DIR)=' <<<"$out")" "0 0"
# Without PCB_LIVE_TEST the hooks are ignored: the real marker is absent here, so a VM claim is refused.
# The hosts file is then $HOME's (the throwaway one, empty here), so the refusal is the marker's.
mkdir -p "$vmhome/.config/pcb-live"; : > "$vmhome/.config/pcb-live/hosts"
out=$(env -i HOME="$vmhome" PATH="$PATH" PCB_LIVE_HOSTNAME=deb13-vm PCB_LIVE_VIRT=kvm PCB_LIVE_VM_MARKER="$tmp/marker" "$here/run.sh" --vm --run v1 -- true 2>&1); rc=$?
check "test hooks are ignored without PCB_LIVE_TEST=1 (marker override)" "$rc $(grep -c '/etc/pcb-live-vm missing' <<<"$out")" "1 1"
# shellcheck disable=SC2016 # $1 is for the inner bash
out=$(env -i HOME="$vmhome" PATH="$PATH" PCB_LIVE_HOME="$vmhome" PCB_LIVE_NOW=1 bash -c '. "$1"; echo "$(live_home) $(live_hook PCB_LIVE_NOW x)"' _ "$here/common.sh")
check "PCB_LIVE_HOME and PCB_LIVE_NOW are ignored without PCB_LIVE_TEST=1" "$(if [[ $out == "$vmhome "* || $out == *" 1" ]]; then echo leaked; else echo ignored; fi)" ignored
for i in 1 2 3; do
  vm deb13-vm kvm "$tmp/marker" $((5000 + 100 * i)) v2 --login "7.$i" 'vm login' -- true >/dev/null
done
out=$(vm deb13-vm kvm "$tmp/marker" 6000 v2 --login 7.4 'a fourth' -- true); rc=$?
check "run.sh --vm budget is 3 (the fourth login is refused)" "$rc" 20
check "run.sh --vm ledger holds three logins" "$(grep -c . "$vmhome/pcb-live-run/v2/logins.tsv")" 3

# M7: the production host is refused without --vm too (any case, FQDN); the workstation is the normal host; a missing tool burns no ledger slot.
mh=$tmp/mhome; mkdir -p "$mh/pcb-live-run/m1/bin"
cp "$HOME/pcb-live-run/t3/bin/pcb" "$mh/pcb-live-run/m1/bin/pcb"
hrun() { # HOSTS_FILE HOSTNAME NOW [run.sh args before --]  (command after --)
  local file=$1 host=$2 now=$3; shift 3
  env -i HOME="$mh" PATH="$PATH" PCB_LIVE_TEST=1 PCB_LIVE_HOME="$mh" PCB_LIVE_HOSTS="$file" PCB_LIVE_HOSTNAME="$host" PCB_LIVE_NOW="$now" "$here/run.sh" --run m1 "$@" 2>&1
}
mrun() { hrun "$hosts" "$@"; } # HOSTNAME NOW [run.sh args before --]
out=$(mrun prod-host 1 -- pcb status); rc=$?
check "run.sh refuses the production host without --vm" "$rc $(grep -c 'pcb sees' <<<"$out") $(grep -c 'production' <<<"$out")" "1 0 1"
out=$(mrun PROD-HOST 1 -- pcb status); rc=$?
check "run.sh refuses the production host without --vm, upper case" "$rc $(grep -c 'pcb sees' <<<"$out")" "1 0"
out=$(mrun Prod-Host.lan 1 -- pcb status); rc=$?
check "run.sh refuses the production host without --vm, as an FQDN" "$rc $(grep -c 'pcb sees' <<<"$out")" "1 0"
out=$(mrun ws-host 1 -- pcb status); rc=$?
check "run.sh runs on the workstation without --vm" "$rc $(grep -c 'pcb sees' <<<"$out")" "0 1"
out=$(mrun prod-host-vm 1 -- pcb status); rc=$?
check "run.sh runs on a host whose name only starts with the production one" "$rc" 0
out=$(mrun .lan 1 -- pcb status); rc=$?
check "run.sh refuses a host name with an empty first label" "$rc $(grep -c "cannot read this host's name" <<<"$out")" "1 1"
out=$(mrun ws-host 1 --login 1 'typo' -- no-such-tool-xyz status); rc=$?
check "run.sh --login: an unknown command is refused before the ledger" "$rc $(test -e "$mh/pcb-live-run/m1/logins.tsv" && echo ledger)" "1 "
nopass=$tmp/nopass; mkdir -p "$nopass"
for t in bash env getent id cut grep tail date flock realpath mkdir hostname dirname cat sed; do ln -sf "$(command -v "$t")" "$nopass/$t"; done
out=$(env -i HOME="$mh" PATH="$nopass" PCB_LIVE_TEST=1 PCB_LIVE_HOME="$mh" PCB_LIVE_HOSTS="$hosts" PCB_LIVE_HOSTNAME=ws-host PCB_LIVE_NOW=1 "$nopass/bash" "$here/run.sh" --run m1 --login 1 'no pass-cli' -- env pass-cli run -- pcb status 2>&1); rc=$?
check "run.sh --login: pass-cli missing is refused before the ledger" "$rc $(grep -c 'pass-cli not found' <<<"$out") $(test -e "$mh/pcb-live-run/m1/logins.tsv" && echo ledger)" "1 1 "
# M9: --proton-cal needs the run root's proton-cal.
out=$(mrun ws-host 1 --proton-cal -- pcb status); rc=$?
check "run.sh --proton-cal refuses a proton-cal missing from the run root" "$rc $(grep -c 'proton-cal' <<<"$out") $(grep -c 'pcb sees' <<<"$out")" "1 1 0"

# The hosts file itself: fail closed when missing; empty means no such hosts; comments ignored.
out=$(hrun "$tmp/no-hosts" ws-host 1 -- pcb status); rc=$?
check "run.sh refuses a missing hosts file, nothing run" "$rc $(grep -c 'pcb sees' <<<"$out")" "1 0"
check "run.sh prints how to create the hosts file" "$(grep -cF "mkdir -p ~/.config/pcb-live && printf 'production <name>\nworkstation <name>\n' > ~/.config/pcb-live/hosts" <<<"$out")" 1
mkdir -p "$tmp/hosts-dir"
out=$(hrun "$tmp/hosts-dir" ws-host 1 -- pcb status); rc=$?
check "run.sh refuses a hosts file that is a directory" "$rc $(grep -c 'pcb sees' <<<"$out")" "1 0"
if (( $(id -u) != 0 )); then
  printf 'production prod-host\n' > "$tmp/hosts-unreadable"; chmod 000 "$tmp/hosts-unreadable"
  out=$(hrun "$tmp/hosts-unreadable" ws-host 1 -- pcb status); rc=$?
  check "run.sh refuses an unreadable hosts file" "$rc $(grep -c 'pcb sees' <<<"$out")" "1 0"
fi
: > "$tmp/hosts-empty"
out=$(hrun "$tmp/hosts-empty" prod-host 1 -- pcb status); rc=$?
check "an empty hosts file is accepted (no such hosts)" "$rc $(grep -c 'pcb sees' <<<"$out")" "0 1"
out=$(hrun "$tmp/hosts-empty" ws-host 1 --vm -- true); rc=$?
check "an empty hosts file still leaves the --vm fence (no marker here)" "$rc $(grep -c 'pcb-live-vm' <<<"$out")" "1 1"
printf '# production c-host\n   # workstation c-host\n\nproduction prod-host # the server\n' > "$tmp/hosts-comments"
out=$(hrun "$tmp/hosts-comments" c-host 1 -- pcb status); rc=$?
check "commented-out lines are ignored" "$rc $(grep -c 'pcb sees' <<<"$out")" "0 1"
out=$(hrun "$tmp/hosts-comments" prod-host 1 -- pcb status); rc=$?
check "a line with a trailing comment still counts" "$rc $(grep -c 'pcb sees' <<<"$out")" "1 0"
printf 'server prod-host\n' > "$tmp/hosts-bad"
out=$(hrun "$tmp/hosts-bad" ws-host 1 -- pcb status); rc=$?
check "a malformed hosts line is refused (fail closed)" "$rc $(grep -c 'pcb sees' <<<"$out") $(grep -c 'line 1' <<<"$out")" "1 0 1"
printf 'workstation ws-host\nproduction prod-host extra\n' > "$tmp/hosts-extra"
out=$(hrun "$tmp/hosts-extra" ws-host 1 -- pcb status); rc=$?
check "a hosts line with an extra field is refused (fail closed)" "$rc $(grep -c 'pcb sees' <<<"$out") $(grep -c 'line 2' <<<"$out")" "1 0 1"
printf 'workstation ws-host\nproduction prod-host' > "$tmp/hosts-nonl"
out=$(hrun "$tmp/hosts-nonl" prod-host 1 -- pcb status); rc=$?
check "a last hosts line without a newline still counts" "$rc $(grep -c 'pcb sees' <<<"$out")" "1 0"
printf 'production prod-host\r\n\r\nworkstation ws-host\r\n' > "$tmp/hosts-crlf"
out=$(hrun "$tmp/hosts-crlf" prod-host 1 -- pcb status); rc=$?
check "a CRLF hosts file still refuses its production host" "$rc $(grep -c 'pcb sees' <<<"$out") $(grep -c 'is your production server' <<<"$out")" "1 0 1"
out=$(hrun "$tmp/hosts-crlf" ws-host 1 -- pcb status); rc=$?
check "a CRLF hosts file still lets the workstation run without --vm" "$rc $(grep -c 'pcb sees' <<<"$out")" "0 1"
# A host listed both ways is production (refused in every mode), whatever the order.
printf 'production dual-host\nworkstation dual-host\n' > "$tmp/hosts-dual1"
printf 'workstation dual-host\nproduction dual-host\n' > "$tmp/hosts-dual2"
for f in "$tmp/hosts-dual1" "$tmp/hosts-dual2"; do
  out=$(hrun "$f" dual-host 1 -- pcb status); rc=$?
  check "a host listed as production and workstation is refused without --vm (${f##*-})" "$rc $(grep -c 'pcb sees' <<<"$out") $(grep -c 'is your production server' <<<"$out")" "1 0 1"
done
# Without PCB_LIVE_TEST=1, PCB_LIVE_HOSTS is ignored: $HOME's file (empty, throwaway) is read, not the hook's (missing).
hh=$tmp/hhome; mkdir -p "$hh/.config/pcb-live"; : > "$hh/.config/pcb-live/hosts"
# shellcheck disable=SC2016 # $1 is for the inner bash
out=$(env -i HOME="$hh" PATH="$PATH" PCB_LIVE_HOSTS="$tmp/no-hosts" bash -c '. "$1"; live_host_check all && echo accepted' _ "$here/common.sh" 2>&1)
check "PCB_LIVE_HOSTS is ignored without PCB_LIVE_TEST=1" "$out" accepted
rm "$hh/.config/pcb-live/hosts"
# shellcheck disable=SC2016 # $1 is for the inner bash
out=$(env -i HOME="$hh" PATH="$PATH" PCB_LIVE_HOSTS="$hosts" bash -c '. "$1"; live_host_check all && echo accepted' _ "$here/common.sh" 2>&1)
check "without PCB_LIVE_TEST=1, a missing \$HOME/.config/pcb-live/hosts is refused" "$(grep -c "$hh/.config/pcb-live/hosts" <<<"$out") $(grep -c accepted <<<"$out")" "1 0"

# preflight.sh: the same hosts file; the workstation is warned about, refused for step 7.
pfh=$tmp/pfhome; mkdir -p "$pfh/pcb-live-run/pf"
pf() { # HOSTS_FILE HOSTNAME [--step7]
  env -i HOME="$pfh" PATH="$PATH" PCB_LIVE_TEST=1 PCB_LIVE_HOME="$pfh" PCB_LIVE_HOSTS="$1" PCB_LIVE_HOSTNAME="$2" \
    PCB_LIVE_VIRT=kvm PCB_LIVE_VM_MARKER="$tmp/marker" "$here/preflight.sh" "$pfh/pcb-live-run/pf" "${@:3}" 2>&1
}
out=$(pf "$hosts" prod-host); rc=$?
check "preflight fails the production host" "$rc $(grep -c '^FAIL  host prod-host' <<<"$out")" "1 1"
out=$(pf "$hosts" PROD-HOST.lan --step7); rc=$?
check "preflight --step7 fails the production host (upper case, FQDN)" "$rc $(grep -c '^FAIL  host prod-host' <<<"$out")" "1 1"
out=$(pf "$hosts" ws-host)
check "preflight warns about the workstation for steps 1-6" "$(grep -c '^WARN  host ws-host' <<<"$out") $(grep -c '^FAIL  host' <<<"$out")" "1 0"
out=$(pf "$hosts" WS-Host.lan --step7)
check "preflight --step7 fails the workstation" "$(grep -c '^FAIL  host ws-host' <<<"$out")" 1
out=$(pf "$hosts" other-host)
check "preflight accepts an unlisted host" "$(grep -c '^ok    host other-host' <<<"$out")" 1
out=$(pf "$tmp/hosts-empty" prod-host)
check "preflight accepts an empty hosts file" "$(grep -c '^ok    host prod-host' <<<"$out")" 1
out=$(pf "$tmp/no-hosts" other-host); rc=$?
check "preflight refuses a missing hosts file before any check" "$rc $(grep -c '^ok' <<<"$out") $(grep -cF 'mkdir -p ~/.config/pcb-live' <<<"$out")" "1 0 1"

# The global ledger above the run roots: spacing and budget hold across runs.
gh=$tmp/ghome; mkdir -p "$gh"
glogin() { # NOW RUN [BUDGET]
  env -i HOME="$gh" PATH="$PATH" PCB_LIVE_TEST=1 PCB_LIVE_HOME="$gh" PCB_LIVE_HOSTS="$hosts" PCB_LIVE_NOW="$1" PCB_LIVE_GLOBAL_BUDGET="${3:-14}" \
    "$here/run.sh" --run "$2" --login G 'global' -- true 2>&1
}
glogin 100 g1 >/dev/null
out=$(glogin 120 g2); rc=$?
check "global ledger: a login in another run 20 s later is refused" "$rc" 20
glogin 200 g2 2 >/dev/null
out=$(glogin 300 g3 2); rc=$?
check "global ledger: the budget counts logins of all runs" "$rc" 20
check "global ledger holds two logins" "$(grep -c . "$gh/pcb-live-run/.ledger")" 2

# L7: --extra-login "<reason>" permits one login past the budget, never the 60 s spacing.
eh=$tmp/ehome; mkdir -p "$eh"
elogin() { # NOW RUN BUDGET GBUDGET [run.sh args before --]
  local now=$1 run=$2 b=$3 gb=$4; shift 4
  env -i HOME="$eh" PATH="$PATH" PCB_LIVE_TEST=1 PCB_LIVE_HOME="$eh" PCB_LIVE_HOSTS="$hosts" PCB_LIVE_NOW="$now" PCB_LIVE_LOGIN_BUDGET="$b" PCB_LIVE_GLOBAL_BUDGET="$gb" \
    "$here/run.sh" --run "$run" "$@" -- true 2>&1
}
elogin 100 e1 1 14 --login 1 'first' >/dev/null
out=$(elogin 200 e1 1 14 --login 2 'past the main budget'); rc=$?
check "extra: past the budget without --extra-login is refused (20)" "$rc" 20
out=$(elogin 200 e1 1 14 --login 2 'past the main budget' --extra-login ''); rc=$?
check "extra: an empty reason is refused (2)" "$rc 20" "2 20"
out=$(elogin 200 e1 1 14 --extra-login 'no login here'); rc=$?
check "extra: --extra-login without --login is refused (2)" "$rc" 2
check "extra: refusals recorded nothing" "$(grep -c . "$eh/pcb-live-run/e1/logins.tsv") $(grep -c . "$eh/pcb-live-run/.ledger")" "1 1"
out=$(elogin 130 e1 1 14 --login 2 'too soon' --extra-login 'CAPTCHA re-solve'); rc=$?
check "extra: the 60 s spacing still holds (20, 'wait 30 s')" "$rc $(grep -c 'wait 30 s' <<<"$out")" "20 1"
out=$(elogin 200 e1 1 14 --login 2 'solve again' --extra-login 'CAPTCHA re-solve'); rc=$?
check "extra: one login past the main budget passes, loudly" "$rc $(grep -c '^live: EXTRA LOGIN' <<<"$out")" "0 1"
check "extra: both ledgers record the reason" "$(grep -c 'CAPTCHA re-solve' "$eh/pcb-live-run/e1/logins.tsv") $(grep -c 'CAPTCHA re-solve' "$eh/pcb-live-run/.ledger")" "1 1"
out=$(elogin 300 e2 14 2 --login 1 'second run' --extra-login 'second attempt after a partial run'); rc=$?
check "extra: past the global budget passes in a second run" "$rc" 0
out=$(elogin 400 e2 14 2 --login 2 'again'); rc=$?
check "extra: the next login without the override is refused again (global)" "$rc" 20

# Parallel logins: the ledger lock lets exactly one through.
for round in 1 2 3; do
  ph=$tmp/par$round; mkdir -p "$ph"
  for i in 1 2 3 4 5 6; do
    env -i HOME="$ph" PATH="$PATH" PCB_LIVE_TEST=1 PCB_LIVE_HOME="$ph" PCB_LIVE_HOSTS="$hosts" PCB_LIVE_NOW=100 \
      "$here/run.sh" --run p --login "P$i" 'parallel' -- true >"$tmp/par$round.$i.out" 2>&1 &
    pids[i]=$!
  done
  ok=0 refused=0
  for i in 1 2 3 4 5 6; do
    wait "${pids[i]}"; rc=$?
    if (( rc == 0 )); then ok=$((ok + 1)); elif (( rc == 20 )) && grep -q '60 s' "$tmp/par$round.$i.out"; then refused=$((refused + 1)); fi
  done
  check "parallel round $round: one login recorded, five refused with the 60 s message" "$ok $refused $(grep -c . "$ph/pcb-live-run/p/logins.tsv")" "1 5 1"
done

if (( failures )); then echo "FAIL: $failures expectation(s)"; exit 1; fi
echo PASS
