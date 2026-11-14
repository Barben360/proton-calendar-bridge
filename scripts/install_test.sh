#!/bin/sh
# Test of install.sh (POSIX sh, runs under dash and macOS /bin/sh). Each case
# installs from a fake release (tags v0.1.0 and v0.2.0, stub binaries) served
# through PCB_INSTALL_BASE_URL=file://..., in a fresh HOME and prefix, with a
# PATH of the tools install.sh needs and no real gh. Prints PASS last; exits 1
# if any expectation failed.
# Usage: sh scripts/install_test.sh        (make install-test)
#   PCB_TEST_SH=dash sh scripts/install_test.sh runs install.sh under dash.
set -u

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/.." && pwd)
inst=$here/install.sh
SH=$(command -v "${PCB_TEST_SH:-sh}") || { echo "no shell ${PCB_TEST_SH:-sh}"; exit 1; }
tmp=$(mktemp -d)
trap 'chmod -R u+w "$tmp" 2>/dev/null; rm -rf "$tmp"' EXIT
failures=0
case_name=
case_failed=0

# --- helpers ---------------------------------------------------------------

fail() { echo "FAIL $case_name: $*"; failures=$((failures + 1)); case_failed=1; }
want_rc() { [ "$rc" = "$1" ] || fail "exit $rc, want $1; stderr: $(cat "$c/err")"; }
has() { grep -qF -- "$2" "$1" 2>/dev/null || fail "$(basename "$1") lacks '$2'"; }
lacks() { if grep -qF -- "$2" "$1" 2>/dev/null; then fail "$(basename "$1") has '$2'"; fi; }
exists() { for f; do [ -e "$f" ] || fail "missing $f"; done; }
absent() { for f; do [ ! -e "$f" ] || fail "unexpected $f"; done; }
same() { cmp -s "$1" "$2" || fail "$1 differs from $2"; }
# shellcheck disable=SC2012 # one known path: only its mode column is read
mode() { [ "$(ls -ld "$1" 2>/dev/null | cut -c1-10)" = "$2" ] || fail "$1 mode '$(ls -ld "$1" 2>/dev/null | cut -c1-10)', want $2"; }
line() { grep -qxF -- "$2" "$1" 2>/dev/null || fail "$(basename "$1") has no line '$2': $(cat "$1" 2>/dev/null)"; }

sum256() { if command -v sha256sum >/dev/null; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -d' ' -f1; }

# tools DIR [EXCLUDED...]: DIR holds links to the tools install.sh may use.
tools() {
  td=$1; shift
  mkdir -p "$td"
  for t in sh mktemp curl wget tar gzip sha256sum shasum awk sed grep cut mkdir cp mv chmod rm cat head tr ls uname dirname; do
    for x; do [ "$t" = "$x" ] && continue 2; done
    tp=$(command -v "$t") && ln -s "$tp" "$td/$t"
  done
}

# stub_bins DIR VERSION OS_ARCH: stub pcb and pcb-ui; the comment tells archives apart.
stub_bins() {
  mkdir -p "$1"
  cat >"$1/pcb" <<EOF
#!/bin/sh
# $3
case "\$*" in
"version --json") echo '{"command":"version","data":{"version":"$2"}}' ;;
version) echo "pcb version $2" ;;
"status --json --daemon") exit "\${PCB_STUB_DAEMON_RC:-20}" ;;
esac
EOF
  printf '#!/bin/sh\n# %s\necho "pcb-ui %s"\n' "$3" "$2" >"$1/pcb-ui"
  chmod 0755 "$1/pcb" "$1/pcb-ui"
}

# mkarchive OS ARCH BINDIR OUTDIR: package.sh with GNU tar, else the same flat
# layout with plain tar (macOS).
mkarchive() {
  if tar --version 2>/dev/null | grep -q GNU; then
    SOURCE_DATE_EPOCH=0 bash "$root/scripts/release/package.sh" "$@" >/dev/null || exit 1
  else
    s=$tmp/flat; rm -rf "$s"; mkdir -p "$s"
    cp "$3/pcb" "$root/LICENSE" "$root/README.md" "$s/"
    if [ "$1" = linux ]; then
      cp "$3/pcb-ui" "$s/"; cp "$root/packaging/pcb-ui.desktop" "$s/"
      cp "$root/internal/ui/desktop/icons/app.svg" "$s/pcb-ui.svg"
    fi
    (cd "$s" && tar -czf "$4/pcb_$1_$2.tar.gz" -- *) || exit 1
  fi
}

# mkrelease DIR VERSION: DIR holds the archives of VERSION and checksums.txt.
mkrelease() {
  mkdir -p "$1"
  for oa in linux_amd64 linux_arm64 darwin_amd64 darwin_arm64; do
    b=$tmp/bins/$2/$oa
    stub_bins "$b" "$2" "$oa"
    mkarchive "${oa%_*}" "${oa#*_}" "$b" "$1"
  done
  (cd "$1" && for f in *.tar.gz; do printf '%s  %s\n' "$(sum256 "$f")" "$f"; done >checksums.txt)
}

# The fake release: download/<tag>/ and latest/download/ (v0.2.0), as GitHub.
mkrelease "$tmp/release/download/v0.1.0" v0.1.0
mkrelease "$tmp/release/download/v0.2.0" v0.2.0
mkdir -p "$tmp/release/latest"
cp -R "$tmp/release/download/v0.2.0" "$tmp/release/latest/download"
# A valid archive that is not the released one (for the checksum cases).
mkdir -p "$tmp/evil"
mkrelease "$tmp/evil" v6.6.6
tools "$tmp/tools"
tools "$tmp/tools-nodl" curl wget
v1=$tmp/bins/v0.1.0/linux_amd64
v2=$tmp/bins/v0.2.0/linux_amd64

# new_case NAME: fresh HOME, cwd, release copy and stub dir; prefix $p.
new_case() {
  case_name=$1
  case_failed=0
  c=$tmp/cases/$1
  mkdir -p "$c/home" "$c/cwd" "$c/stubs"
  cp -R "$tmp/release" "$c/rel"
  p=$c/prefix
  cwd=$c/cwd
  run_path=$tmp/tools
  os=Linux arch=x86_64
  export GH_STUB_AUTH=0 GH_STUB_VERIFY=0 GH_STUB_LOG="$c/gh.log"
  unset PCB_STUB_DAEMON_RC
}
end_case() { [ "$case_failed" = 0 ] && echo "ok   $case_name"; }

# stub NAME BODY: a #!/bin/sh stub on the case's PATH.
stub() {
  printf '#!/bin/sh\n%s\n' "$2" >"$c/stubs/$1"
  chmod 0755 "$c/stubs/$1"
  run_path=$c/stubs:$run_path
}
stub_gh() {
  # shellcheck disable=SC2016 # expanded by the stub
  stub gh 'echo "$*" >>"$GH_STUB_LOG"
case $1 in auth) exit "$GH_STUB_AUTH" ;; attestation) exit "$GH_STUB_VERIFY" ;; esac
exit 3'
}

# run ARGS...: install.sh ARGS from $cwd; sets rc, writes $c/out and $c/err.
run() {
  (cd "$cwd" && env HOME="$c/home" PATH="$run_path" PCB_INSTALL_BASE_URL="file://$c/rel" \
    PCB_INSTALL_OS="$os" PCB_INSTALL_ARCH="$arch" "$SH" "$inst" "$@" >"$c/out" 2>"$c/err")
  rc=$?
}

# --- cases -----------------------------------------------------------------

test_default_install() {
  new_case default_install
  run --prefix "$p"
  want_rc 0
  same "$p/bin/pcb" "$v2/pcb"
  same "$p/bin/pcb-ui" "$v2/pcb-ui"
  mode "$p/bin/pcb" -rwxr-xr-x
  mode "$p/bin/pcb-ui" -rwxr-xr-x
  d=$p/share/applications/pcb-ui.desktop
  exists "$d" "$p/share/icons/hicolor/scalable/apps/pcb-ui.svg"
  line "$d" "Exec=$p/bin/pcb-ui"
  line "$d" "Icon=pcb-ui"
  mode "$d" -rw-r--r--
  same "$p/share/icons/hicolor/scalable/apps/pcb-ui.svg" "$root/internal/ui/desktop/icons/app.svg"
  has "$c/out" "pcb version v0.2.0"
  absent "$c/home/.local"
  end_case
}

test_default_prefix() {
  new_case default_prefix
  run
  want_rc 0
  same "$c/home/.local/bin/pcb" "$v2/pcb"
  line "$c/home/.local/share/applications/pcb-ui.desktop" "Exec=$c/home/.local/bin/pcb-ui"
  end_case
}

test_no_desktop() {
  new_case no_desktop
  run --prefix "$p" --no-desktop
  want_rc 0
  exists "$p/bin/pcb" "$p/bin/pcb-ui"
  absent "$p/share"
  end_case
}

test_no_ui() {
  new_case no_ui
  run --prefix "$p" --no-ui
  want_rc 0
  same "$p/bin/pcb" "$v2/pcb"
  absent "$p/bin/pcb-ui" "$p/share"
  end_case
}

test_darwin() {
  new_case darwin
  os=Darwin arch=arm64
  run --prefix "$p"
  want_rc 0
  same "$p/bin/pcb" "$tmp/bins/v0.2.0/darwin_arm64/pcb"
  absent "$p/bin/pcb-ui" "$p/share"
  has "$c/out" "pcb-ui is Linux only for now"
  end_case
}

test_arch_names() {
  new_case arch_names
  for a in x86_64:linux_amd64 amd64:linux_amd64 aarch64:linux_arm64 arm64:linux_arm64; do
    arch=${a%:*}
    run --prefix "$p/$arch"
    want_rc 0
    same "$p/$arch/bin/pcb" "$tmp/bins/v0.2.0/${a#*:}/pcb"
  done
  arch=riscv64
  run --prefix "$p/riscv"
  want_rc 2
  has "$c/err" riscv64
  absent "$p/riscv"
  os=FreeBSD arch=x86_64
  run --prefix "$p/bsd"
  want_rc 2
  has "$c/err" FreeBSD
  end_case
}

# keep_previous: the v0.1.0 install is byte-identical, with no staged file left.
keep_previous() {
  same "$p/bin/pcb" "$v1/pcb"
  same "$p/bin/pcb-ui" "$v1/pcb-ui"
  same "$p/share/applications/pcb-ui.desktop" "$c/desktop.before"
  [ -z "$(find "$p" -name '*.new.*')" ] || fail "staged files left: $(find "$p" -name '*.new.*')"
}

test_bad_checksum_keeps_previous() {
  new_case bad_checksum_keeps_previous
  run --prefix "$p" --version v0.1.0
  want_rc 0
  cp "$p/share/applications/pcb-ui.desktop" "$c/desktop.before"
  l=$c/rel/latest/download
  cp "$l/pcb_linux_amd64.tar.gz" "$c/good.tar.gz"
  cp "$tmp/evil/pcb_linux_amd64.tar.gz" "$l/"
  run --prefix "$p"
  want_rc 1
  has "$c/err" pcb_linux_amd64.tar.gz
  keep_previous
  cp "$c/good.tar.gz" "$l/pcb_linux_amd64.tar.gz"
  grep -v pcb_linux_amd64 "$l/checksums.txt" >"$c/sums" && cp "$c/sums" "$l/checksums.txt"
  run --prefix "$p"
  want_rc 1
  has "$c/err" pcb_linux_amd64.tar.gz
  keep_previous
  end_case
}

test_upgrade_replaces() {
  new_case upgrade_replaces
  run --prefix "$p" --version v0.1.0
  want_rc 0
  same "$p/bin/pcb" "$v1/pcb"
  echo "X-Stale=1" >>"$p/share/applications/pcb-ui.desktop"
  run --prefix "$p"
  want_rc 0
  same "$p/bin/pcb" "$v2/pcb"
  same "$p/bin/pcb-ui" "$v2/pcb-ui"
  lacks "$p/share/applications/pcb-ui.desktop" "X-Stale"
  line "$p/share/applications/pcb-ui.desktop" "Exec=$p/bin/pcb-ui"
  [ -z "$(find "$p" -name '*.new.*')" ] || fail "staged files left"
  end_case
}

test_version_forms() {
  new_case version_forms
  run --prefix "$p" --version 0.1.0
  want_rc 0
  same "$p/bin/pcb" "$v1/pcb"
  has "$c/out" "pcb version v0.1.0"
  run --prefix "$p" --version v9.9.9
  want_rc 1
  has "$c/err" v9.9.9
  run --prefix "$p" --version 0.3.0-rc.1
  want_rc 1
  has "$c/err" v0.3.0-rc.1
  for bad in latest-ish v1.2 01.2.3 v1.2.3+meta ""; do
    run --prefix "$p/bad" --version "$bad"
    want_rc 2
    absent "$p/bad"
  done
  run --prefix "$p" --version
  want_rc 2
  same "$p/bin/pcb" "$v1/pcb"
  end_case
}

test_unknown_flag() {
  new_case unknown_flag
  run --prefix "$p" --frobnicate
  want_rc 2
  has "$c/err" "--frobnicate"
  has "$c/err" "Usage:"
  absent "$p"
  run --help
  want_rc 0
  has "$c/out" "Usage:"
  end_case
}

test_prefix_forms() {
  new_case prefix_forms
  run --prefix="$p/eq"
  want_rc 0
  same "$p/eq/bin/pcb" "$v2/pcb"
  line "$p/eq/share/applications/pcb-ui.desktop" "Exec=$p/eq/bin/pcb-ui"
  run --prefix ./rel-p
  want_rc 0
  same "$c/cwd/rel-p/bin/pcb" "$v2/pcb"
  line "$c/cwd/rel-p/share/applications/pcb-ui.desktop" "Exec=$c/cwd/rel-p/bin/pcb-ui"
  has "$c/out" "under $c/cwd/rel-p"
  run --prefix "$p/with space"
  want_rc 0
  same "$p/with space/bin/pcb" "$v2/pcb"
  line "$p/with space/share/applications/pcb-ui.desktop" "Exec=\"$p/with space/bin/pcb-ui\""
  run --prefix=
  want_rc 2
  absent "$c/home/.local"
  run --prefix --no-ui
  want_rc 2
  has "$c/err" "--prefix needs a directory, not the option --no-ui"
  absent "$c/home/.local" "$c/cwd/--no-ui"
  end_case
}

test_prefix_refused_chars() {
  new_case prefix_refused_chars
  mkdir "$c/cw\$d" "$c/cw%x"
  before=$(ls -A "$c")
  for ch in % '"' '$' "\\" '`'; do
    run --prefix "$c/a${ch}b"
    want_rc 2
    has "$c/err" "--prefix must not contain"
    rm -f "$c/out" "$c/err"
    [ "$(ls -A "$c")" = "$before" ] || fail "'$ch' wrote: $(ls -A "$c")"
    [ -z "$(ls -A "$c/home")$(ls -A "$c/cwd")" ] || fail "'$ch' wrote in home or cwd"
  done
  # A relative prefix is checked once made absolute: the cwd's name counts.
  for cwd in "$c/cw\$d" "$c/cw%x"; do
    run --prefix ./p
    want_rc 2
    has "$c/err" "--prefix must not contain"
    [ -z "$(ls -A "$cwd")$(ls -A "$c/home")" ] || fail "relative prefix from $cwd wrote"
  done
  end_case
}

test_unwritable_prefix() {
  new_case unwritable_prefix
  if [ "$(id -u)" = 0 ]; then echo "SKIP $case_name: root can write a 0555 directory"; return 0; fi
  mkdir -p "$p"
  chmod 0555 "$p"
  run --prefix "$p"
  want_rc 1
  has "$c/err" "choose another directory with --prefix"
  absent "$p/bin"
  end_case
}

test_verify_without_gh() {
  new_case verify_without_gh
  run --prefix "$p" --verify
  want_rc 1
  has "$c/err" gh
  absent "$p/bin/pcb"
  end_case
}

test_gh_verify_failure_stops() {
  new_case gh_verify_failure_stops
  stub_gh
  GH_STUB_VERIFY=1
  run --prefix "$p"
  want_rc 1
  absent "$p/bin/pcb" "$p/bin/pcb-ui"
  has "$c/gh.log" "--repo Barben360/proton-calendar-bridge"
  end_case
}

test_gh_verify_ok() {
  new_case gh_verify_ok
  stub_gh
  run --prefix "$p" --verify
  want_rc 0
  same "$p/bin/pcb" "$v2/pcb"
  grep -q '^attestation verify .*/pcb_linux_amd64\.tar\.gz --repo Barben360/proton-calendar-bridge$' "$c/gh.log" ||
    fail "gh not called as expected: $(cat "$c/gh.log")"
  end_case
}

test_gh_logged_out_skips() {
  new_case gh_logged_out_skips
  stub_gh
  GH_STUB_AUTH=1
  run --prefix "$p"
  want_rc 0
  same "$p/bin/pcb" "$v2/pcb"
  has "$c/out" "attestation not checked"
  lacks "$c/gh.log" "attestation verify"
  run --prefix "$p" --verify
  want_rc 1
  end_case
}

test_path_warning() {
  new_case path_warning
  run --prefix "$p"
  want_rc 0
  has "$c/out" "$p/bin is not on your PATH"
  has "$c/out" "export PATH=\"$p/bin:\$PATH\""
  run_path=$run_path:$p/bin
  run --prefix "$p"
  want_rc 0
  lacks "$c/out" "not on your PATH"
  lacks "$c/out" "export PATH="
  end_case
}

test_daemon_hint() {
  new_case daemon_hint
  run --prefix "$p"
  want_rc 0
  lacks "$c/out" "restart"
  export PCB_STUB_DAEMON_RC=0
  run --prefix "$p"
  want_rc 0
  has "$c/out" "restart"
  end_case
}

test_ldd_warning() {
  new_case ldd_warning
  stub ldd 'printf "\tlibc.so.6 => /lib/libc.so.6\n"'
  run --prefix "$p"
  want_rc 0
  lacks "$c/err" "missing"
  stub ldd 'printf "\tlibGL.so.1 => not found\n\tlibc.so.6 => /lib/libc.so.6\n"'
  run --prefix "$p"
  want_rc 0
  has "$c/err" "libGL.so.1"
  lacks "$c/err" "libc.so.6"
  has "$c/err" "libgl1-mesa-dev"
  run --prefix "$p" --no-ui
  want_rc 0
  lacks "$c/err" "libGL.so.1"
  end_case
}

test_update_desktop_database() {
  new_case update_desktop_database
  # shellcheck disable=SC2016 # expanded by the stub
  stub update-desktop-database 'echo "$*" >"$GH_STUB_LOG"; exit 1'
  run --prefix "$p"
  want_rc 0
  line "$c/gh.log" "$p/share/applications"
  end_case
}

test_truncated_script_does_nothing() {
  new_case truncated_script_does_nothing
  total=$(wc -l <"$inst")
  n=1
  while [ "$n" -lt "$total" ]; do
    (cd "$c/cwd" && head -n "$n" "$inst" | env HOME="$c/home" PATH="$run_path" PCB_INSTALL_BASE_URL="file://$c/rel" \
      PCB_INSTALL_OS="$os" PCB_INSTALL_ARCH="$arch" "$SH" -s -- --prefix "$p" >"$c/out" 2>"$c/err")
    if [ -e "$p" ] || [ -n "$(ls -A "$c/home")$(ls -A "$c/cwd")" ]; then fail "the first $n lines wrote"; break; fi
    n=$((n + 1))
  done
  # The whole script, piped the same way, does install: the pipe itself works.
  (cd "$c/cwd" && env HOME="$c/home" PATH="$run_path" PCB_INSTALL_BASE_URL="file://$c/rel" \
    PCB_INSTALL_OS="$os" PCB_INSTALL_ARCH="$arch" "$SH" -s -- --prefix "$p" <"$inst" >"$c/out" 2>"$c/err")
  same "$p/bin/pcb" "$v2/pcb"
  end_case
}

test_no_downloader() {
  new_case no_downloader
  run_path=$tmp/tools-nodl
  run --prefix "$p"
  want_rc 1
  has "$c/err" curl
  has "$c/err" wget
  absent "$p"
  end_case
}

test_default_install
test_default_prefix
test_no_desktop
test_no_ui
test_darwin
test_arch_names
test_bad_checksum_keeps_previous
test_upgrade_replaces
test_version_forms
test_unknown_flag
test_prefix_forms
test_prefix_refused_chars
test_unwritable_prefix
test_verify_without_gh
test_gh_verify_failure_stops
test_gh_verify_ok
test_gh_logged_out_skips
test_path_warning
test_daemon_hint
test_ldd_warning
test_update_desktop_database
test_truncated_script_does_nothing
test_no_downloader

if [ "$failures" -gt 0 ]; then echo "$failures failed"; exit 1; fi
echo PASS
