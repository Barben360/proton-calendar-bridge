#!/usr/bin/env bash
# Test of vmtest.sh (bats-free): a fake ssh stands in for the VM. Prints PASS
# last; exits 1 if any expectation failed.
# Usage: scripts/win/vmtest_test.sh        (make vmtest-test)
set -uo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
vm=$here/vmtest.sh
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
failures=0

check() { # LABEL ACTUAL WANT
  if [[ $2 == "$3" ]]; then echo "ok   $1"; else echo "FAIL $1: got '$2', want '$3'"; failures=$((failures + 1)); fi
}
contains() { # LABEL HAYSTACK NEEDLE
  if [[ $2 == *"$3"* ]]; then echo "ok   $1"; else echo "FAIL $1: '$3' not in '$2'"; failures=$((failures + 1)); fi
}

bin=$tmp/bin; mkdir -p "$bin"
# The fake ssh logs one line per call (arguments separated by |), answers the
# shell probe from $FAKE_OS, and saves the stdin of the transfer call.
cat > "$bin/ssh" <<'FAKE'
#!/usr/bin/env bash
args=$(IFS='|'; echo "$*")
echo "$args" >> "$FAKE_LOG"
echo "** post-quantum warning (stderr noise)" >&2
case "${*: -1}" in
  *'echo %OS%'*) echo "${FAKE_OS:-Windows_NT}" ;;
  *'tar -xf -'*) cat > "$FAKE_STDIN" ;;
esac
FAKE
chmod +x "$bin/ssh"
export PATH=$bin:$PATH FAKE_LOG=$tmp/log FAKE_STDIN=$tmp/stdin

# A throwaway repository: tracked go.mod, untracked new file.
repo=$tmp/repo; mkdir -p "$repo"
git -C "$repo" init -q
echo 'module x' > "$repo/go.mod"; git -C "$repo" add go.mod
echo hi > "$repo/brand_new_file.txt"
echo gone > "$repo/deleted.txt"; git -C "$repo" add deleted.txt
git -C "$repo" -c user.name=t -c user.email=t@t commit -qm base
rm "$repo/deleted.txt" # tracked, deleted in the working tree

run() { (cd "$repo" && "$vm" "$@") 2>"$tmp/err" >"$tmp/out"; rc=$?; }

unset PCB_WIN_SSH
run test ./x
check "no PCB_WIN_SSH exits 2" "$rc" 2
contains "no PCB_WIN_SSH names the variable" "$(cat "$tmp/err")" PCB_WIN_SSH

: > "$tmp/log"
export PCB_WIN_SSH='-p 2222 user@host'
run test ./internal/paths -v
check "test exits 0" "$rc" 0
check "ssh target words" "$(sed -n 2p "$tmp/log" | cut -d'|' -f1-3)" "-p|2222|user@host"
remote=$(tail -n 1 "$tmp/log")
if [[ $remote =~ pcb-vmtest-[0-9a-f]{8} ]]; then echo "ok   remote dir name"; else echo "FAIL remote dir name: $remote"; failures=$((failures + 1)); fi
contains "CGO off by default" "$remote" 'set "CGO_ENABLED=0"'
contains "go test command" "$remote" 'go test "./internal/paths" "-v"'
# cmd.exe: "if exist X cmd & next" runs next only when X exists, so group it.
contains "stale copy removed, then mkdir unconditionally" "$(sed -n 2p "$tmp/log")" '(if exist pcb-vmtest-'
contains "mkdir outside the if" "$(sed -n 2p "$tmp/log")" ') & mkdir pcb-vmtest-'
check "no local path leaves the host" "$(grep -c "$tmp" "$tmp/log")" 0
listing=$(tar -tf "$tmp/stdin" 2>&1)
contains "tar has tracked file" "$listing" go.mod
contains "tar has untracked file" "$listing" brand_new_file.txt
if [[ $listing == *deleted.txt* ]]; then echo "FAIL deleted tracked file is in the tar"; failures=$((failures + 1)); else echo "ok   deleted tracked file left out"; fi

: > "$tmp/log"
run test -ui ./cmd/pcb-ui
check "-ui exits 0" "$rc" 0
contains "-ui turns CGO on" "$(tail -n 1 "$tmp/log")" 'set "CGO_ENABLED=1"'
contains "-ui does not pass -ui to go" "$(tail -n 1 "$tmp/log")" 'go test "./cmd/pcb-ui"'

: > "$tmp/log"
run test -race ./x
contains "-race reaches go test" "$(tail -n 1 "$tmp/log")" 'go test "-race" "./x"'

: > "$tmp/log"
run run 'go version'
check "run exits 0" "$rc" 0
contains "run executes the command" "$(tail -n 1 "$tmp/log")" '&& go version'

: > "$tmp/log"
run test -run 'TestA|TestB' './my dir'
contains "pipe and space args are quoted words" "$(tail -n 1 "$tmp/log")" 'go test "-run" "TestA|TestB" "./my dir"'

for bad in 'a"b' '%PATH%' $'a\nb'; do
  : > "$tmp/log"
  run test -run "$bad"
  check "refuses argument $(printf %q "$bad")" "$rc" 2
  contains "refusal explains" "$(cat "$tmp/err")" 'cmd.exe cannot carry'
done

: > "$tmp/log"
run test
check "test without arguments exits 0" "$rc" 0
contains "test without arguments runs go test" "$(tail -n 1 "$tmp/log")" '&& go test'

run
check "no mode exits 2" "$rc" 2
contains "usage prints the whole header" "$(cat "$tmp/err")" 'must be cmd.exe'

FAKE_OS=PowerShell run test ./x
check "PowerShell default shell exits 2" "$rc" 2
contains "PowerShell message" "$(cat "$tmp/err")" 'cmd.exe'

if [[ $failures -eq 0 ]]; then echo PASS; else echo "$failures failure(s)"; exit 1; fi
