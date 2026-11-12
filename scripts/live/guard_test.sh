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

# check LABEL ACTUAL WANT: equal strings or a failure.
check() {
  if [[ $2 == "$3" ]]; then echo "ok   $1"; else echo "FAIL $1: got '$2', want '$3'"; failures=$((failures + 1)); fi
}

# run_guard NAME=VALUE...: sources the guard in a clean bash with exactly these variables.
run_guard() {
  # shellcheck disable=SC2016 # $1 is for the inner bash
  env -i HOME="$HOME" PATH="$PATH" "$@" bash -c '. "$1"' _ "$here/guard.sh"
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
  env -i HOME="$HOME" PATH="$PATH" "${good[@]}" PCB_LIVE_NOW="$1" PCB_LIVE_LOGIN_BUDGET="${4:-3}" \
    bash -c '. "$1" >/dev/null && live_login "$2" "$3"' _ "$here/guard.sh" "$2" "$3" 2>&1
}
ledger_expect() { # LABEL WANT_RC OUTPUT_PART NOW STEP PURPOSE [BUDGET]
  local label=$1 want=$2 part=$3; shift 3
  out=$(ledger_run "$@"); rc=$?
  if [[ $rc != "$want" || $out != *"$part"* ]]; then
    echo "FAIL ledger $label: rc=$rc (want $want), out: $out"; failures=$((failures + 1))
  else echo "ok   ledger $label"; fi
}
ledger_expect "first login is recorded" 0 "login 1 of 3" 1000 2a "Other, solved token"
ledger_expect "a login 30 s later is refused" 20 "wait 30 s" 1030 2b "too soon"
ledger_expect "a login 60 s later is recorded" 0 "login 2 of 3" 1060 2b "linux-mail"
ledger_expect "a refused login is not recorded" 0 "login 3 of 3" 1120 2c "web-calendar"
ledger_expect "beyond the budget is refused" 20 "budget spent (3 of 3)" 9999 2d "one too many"
check "ledger holds exactly the three recorded logins" "$(grep -c . "$R/logins.tsv")" 3
check "ledger keeps step order" "$(cut -f3 "$R/logins.tsv" | paste -sd,)" "2a,2b,2c"

# run.sh: the one-command form. A fake pcb in the run root prints what it was given.
mkdir -p "$HOME/pcb-live-run/t3/bin"
cat > "$HOME/pcb-live-run/t3/bin/pcb" <<'FAKE'
#!/usr/bin/env bash
echo "pcb sees config=$PCB_CONFIG_DIR state=$PCB_STATE_DIR lab=$PCB_LAB_DIR xdg=${XDG_STATE_HOME-unset} args=$*"
FAKE
chmod +x "$HOME/pcb-live-run/t3/bin/pcb"
runsh() { env -i HOME="$HOME" PATH="$PATH" "$@" ; }
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
runsh "$here/run.sh" --run t3 --login 2a 'first' -- pcb status >/dev/null 2>&1
out=$(runsh "$here/run.sh" --run t3 --login 2b 'too soon' -- pcb status 2>&1); rc=$?
check "run.sh --login refuses a second login at once (exit 20)" "$rc" 20
check "run.sh --login did not run the command when refused" "$(grep -c 'pcb sees' <<<"$out")" 0

# run.sh --vm: step 7's VM keeps the host's default dirs, so only the host and the ledger are checked.
out=$(runsh PCB_LIVE_HOSTNAME=blackwater "$here/run.sh" --vm --run t3 -- pcb status 2>&1); rc=$?
check "run.sh --vm refuses blackwater" "$rc" 1
out=$(runsh PCB_LIVE_HOSTNAME=morpheus "$here/run.sh" --vm --run t3 -- pcb status 2>&1); rc=$?
check "run.sh --vm refuses morpheus" "$rc" 1
out=$(runsh PCB_LIVE_HOSTNAME=deb13-vm "$here/run.sh" --vm --run t6 -- env 2>&1); rc=$?
check "run.sh --vm on a VM exports no pcb dir" "$rc $(grep -cE '^(PCB_CONFIG_DIR|PCB_STATE_DIR|PCB_LAB_DIR)=' <<<"$out")" "0 0"
for i in 1 2 3; do
  runsh PCB_LIVE_HOSTNAME=deb13-vm PCB_LIVE_NOW=$((5000 + 100 * i)) "$here/run.sh" --vm --run t6 --login "7.$i" 'vm login' -- true >/dev/null 2>&1
done
out=$(runsh PCB_LIVE_HOSTNAME=deb13-vm PCB_LIVE_NOW=6000 "$here/run.sh" --vm --run t6 --login 7.4 'a fourth' -- true 2>&1); rc=$?
check "run.sh --vm budget is 3 (the fourth login is refused)" "$rc" 20
check "run.sh --vm ledger holds three logins" "$(grep -c . "$HOME/pcb-live-run/t6/logins.tsv")" 3

if (( failures )); then echo "FAIL: $failures expectation(s)"; exit 1; fi
echo PASS
