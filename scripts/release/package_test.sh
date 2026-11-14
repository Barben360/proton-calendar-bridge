#!/usr/bin/env bash
# Test of package.sh (bats-free). Stub binaries in a temp dir. Prints PASS last;
# exits 1 if any expectation failed.
# Usage: scripts/release/package_test.sh        (make release-test)
set -uo pipefail
export TZ=UTC # tar -tv prints local time

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
pkg=$here/package.sh
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
failures=0

check() { # LABEL ACTUAL WANT
  if [[ $2 == "$3" ]]; then echo "ok   $1"; else echo "FAIL $1: got '$2', want '$3'"; failures=$((failures + 1)); fi
}

bin=$tmp/bin; mkdir -p "$bin"
printf '#!/bin/sh\necho pcb\n' > "$bin/pcb"
printf '#!/bin/sh\necho pcb-ui\n' > "$bin/pcb-ui"
chmod 0600 "$bin/pcb" "$bin/pcb-ui" # the archive sets the modes, not the inputs

out=$tmp/out; mkdir -p "$out"

"$pkg" linux amd64 "$bin" "$out" >/dev/null 2>&1; rc=$?
check "linux packages" "$rc" 0
a=$out/pcb_linux_amd64.tar.gz
check "linux contents" "$(tar -tzf "$a" | LC_ALL=C sort | tr '\n' ' ')" "LICENSE README.md pcb pcb-ui pcb-ui.desktop pcb-ui.svg "
check "pcb mode and owner" "$(tar -tvzf "$a" --numeric-owner | awk '$NF=="pcb"{print $1, $2}')" "-rwxr-xr-x 0/0"
check "pcb-ui mode" "$(tar -tvzf "$a" | awk '$NF=="pcb-ui"{print $1}')" "-rwxr-xr-x"
check "LICENSE mode and owner" "$(tar -tvzf "$a" --numeric-owner | awk '$NF=="LICENSE"{print $1, $2}')" "-rw-r--r-- 0/0"
check "svg is the app icon" "$(tar -xzOf "$a" pcb-ui.svg | cmp - "$here/../../internal/ui/desktop/icons/app.svg" && echo same)" same

sum1=$(sha256sum "$a" | cut -d' ' -f1)
mkdir -p "$tmp/out2"
SOURCE_DATE_EPOCH=$(git -C "$here" log -1 --format=%ct) "$pkg" linux amd64 "$bin" "$tmp/out2" >/dev/null 2>&1
check "rebuild is byte-identical" "$(sha256sum "$tmp/out2/pcb_linux_amd64.tar.gz" | cut -d' ' -f1)" "$sum1"
sleep 1; touch "$bin/pcb" "$bin/pcb-ui" # file mtimes must not leak in
"$pkg" linux amd64 "$bin" "$tmp/out2" >/dev/null 2>&1
check "rebuild after touching inputs is identical" "$(sha256sum "$tmp/out2/pcb_linux_amd64.tar.gz" | cut -d' ' -f1)" "$sum1"
SOURCE_DATE_EPOCH=86400 "$pkg" linux amd64 "$bin" "$tmp/out2" >/dev/null 2>&1
check "SOURCE_DATE_EPOCH sets the mtime" "$(tar -tvzf "$tmp/out2/pcb_linux_amd64.tar.gz" | awk '$NF=="pcb"{print $4}')" "1970-01-02"

"$pkg" darwin arm64 "$bin" "$out" >/dev/null 2>&1; rc=$?
check "darwin packages" "$rc" 0
check "darwin contents" "$(tar -tzf "$out/pcb_darwin_arm64.tar.gz" | LC_ALL=C sort | tr '\n' ' ')" "LICENSE README.md pcb "

msg=$("$pkg" windows amd64 "$bin" "$out" 2>&1); rc=$?
check "unknown OS refused" "$rc $(grep -c windows <<<"$msg")" "2 1"
msg=$("$pkg" linux riscv "$bin" "$out" 2>&1); rc=$?
check "unknown ARCH refused" "$rc $(grep -c riscv <<<"$msg")" "2 1"
check "no archive for refused OS" "$(find "$out" -name "*windows*" | wc -l)" 0
(cd "$tmp" && "$pkg" linux arm64 bin rel/out >/dev/null 2>&1); rc=$?
check "relative BINDIR and OUTDIR" "$rc $(find "$tmp/rel/out" -type f -printf %f)" "0 pcb_linux_arm64.tar.gz"

nb=$tmp/nobin; mkdir -p "$nb"; cp "$bin/pcb" "$nb/pcb"
msg=$("$pkg" linux amd64 "$nb" "$tmp/out3" 2>&1); rc=$?
check "linux without pcb-ui refused" "$rc $(grep -c pcb-ui <<<"$msg")" "2 1"
"$pkg" darwin amd64 "$nb" "$tmp/out3" >/dev/null 2>&1; rc=$?
check "darwin needs only pcb" "$rc" 0
rm "$nb/pcb"
msg=$("$pkg" darwin amd64 "$nb" "$tmp/out3" 2>&1); rc=$?
check "missing pcb refused" "$rc $(grep -c pcb <<<"$msg")" "2 1"
msg=$("$pkg" linux amd64 "$bin" 2>&1); rc=$?
check "missing argument refused" "$rc" 2

if ((failures > 0)); then echo "$failures failed"; exit 1; fi
echo PASS
