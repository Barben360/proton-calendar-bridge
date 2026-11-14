#!/bin/sh
# Installs a pcb release (pcb, and on Linux pcb-ui with its desktop entry and
# icon) from GitHub, after checking its SHA-256 and, when gh is logged in, its
# build attestation. Never uses sudo.
#   curl -fsSL https://github.com/Barben360/proton-calendar-bridge/releases/latest/download/install.sh | sh
#   curl -fsSL .../install.sh | sh -s -- --no-desktop
# POSIX sh. Only functions until the last line, so a truncated download runs
# nothing. Exit codes: 0 done, 1 failed, 2 bad invocation.

REPO=Barben360/proton-calendar-bridge

usage() {
  cat <<'EOF'
Usage: install.sh [--prefix DIR] [--version vX.Y.Z] [--no-desktop] [--no-ui] [--verify]

  --prefix DIR       install under DIR/bin and DIR/share (default ~/.local)
  --version vX.Y.Z   that release instead of the latest stable one
  --no-desktop       no desktop entry nor icon
  --no-ui            pcb only: no pcb-ui, no desktop entry (servers)
  --verify           require the attestation check (needs gh, logged in)
  -h, --help         this help
EOF
}

say() { printf '%s\n' "$*"; }
warn() { printf 'install.sh: %s\n' "$*" >&2; }
die() { warn "$*"; exit 1; }
die_usage() {
  warn "$*"
  usage >&2
  exit 2
}

parse_args() {
  prefix='' prefix_set=0 version='' version_set=0 no_desktop=0 no_ui=0 verify=0
  while [ $# -gt 0 ]; do
    case $1 in
    --prefix) [ $# -ge 2 ] || die_usage "--prefix needs a directory"; prefix=$2 prefix_set=1; shift ;;
    --prefix=*) prefix=${1#--prefix=} prefix_set=1 ;;
    --version) [ $# -ge 2 ] || die_usage "--version needs a version"; version=$2 version_set=1; shift ;;
    --version=*) version=${1#--version=} version_set=1 ;;
    --no-desktop) no_desktop=1 ;;
    --no-ui) no_ui=1 ;;
    --verify) verify=1 ;;
    -h | --help) usage; exit 0 ;;
    *) die_usage "unknown argument: $1" ;;
    esac
    shift
  done
  [ "$prefix_set" = 0 ] || [ -n "$prefix" ] || die_usage "--prefix needs a directory"
  if [ -z "$prefix" ]; then
    [ -n "${HOME:-}" ] || die_usage "HOME is not set: give --prefix DIR"
    prefix=$HOME/.local
  fi
}

# check_version: vX.Y.Z[-pre] or X.Y.Z[-pre] (the release tag form); sets tag.
check_version() {
  tag=
  [ "$version_set" = 0 ] && return 0
  case $version in *"
"*) die_usage "bad --version: not vX.Y.Z" ;; esac
  case $version in v*) tag=$version ;; *) tag=v$version ;; esac
  printf '%s\n' "$tag" |
    grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$' ||
    die_usage "bad --version '$version': want vX.Y.Z or vX.Y.Z-PRERELEASE"
}

# check_prefix: refuses the characters a desktop entry's Exec= cannot take
# safely. make_prefix makes it absolute.
check_prefix() {
  case $prefix in
  *[%\"\$\\\`]* | *"
"*) die_usage "--prefix must not contain % \" \$ \\ \` or a newline: $prefix" ;;
  esac
}

detect_platform() {
  s=${PCB_INSTALL_OS:-$(uname -s)}
  m=${PCB_INSTALL_ARCH:-$(uname -m)}
  case $s in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) die_usage "unsupported OS: $s (Linux or macOS)" ;;
  esac
  case $m in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) die_usage "unsupported architecture: $m (x86_64 or arm64)" ;;
  esac
  if [ "$os" = darwin ]; then
    no_ui=1
    say "pcb-ui is Linux only for now: installing pcb only."
  fi
  [ "$no_ui" = 1 ] && no_desktop=1
  archive=pcb_${os}_${arch}.tar.gz
}

pick_downloader() {
  if command -v curl >/dev/null 2>&1; then
    dl=curl
  elif command -v wget >/dev/null 2>&1; then
    dl=wget
  else
    die "neither curl nor wget found: install one of them"
  fi
}

# fetch FILE: downloads FILE of the release into the staging directory.
fetch() {
  base=${PCB_INSTALL_BASE_URL:-https://github.com/$REPO/releases}
  if [ -n "$tag" ]; then url=$base/download/$tag/$1; else url=$base/latest/download/$1; fi
  if [ "$dl" = curl ]; then
    curl -fsSL -o "$stage/$1" "$url"
  else
    wget -q -O "$stage/$1" "$url"
  fi || die "cannot download $1 of ${tag:-the latest release} ($url): no such release, or a network error"
}

# check_sum: the archive's SHA-256 against its line in checksums.txt.
check_sum() {
  if command -v sha256sum >/dev/null 2>&1; then
    sum='sha256sum'
  elif command -v shasum >/dev/null 2>&1; then
    sum='shasum -a 256'
  else
    die "neither sha256sum nor shasum found: cannot check the download"
  fi
  want=$(awk -v f="$archive" '$2 == f || $2 == "*" f {print $1; exit}' "$stage/checksums.txt")
  [ -n "$want" ] || die "checksums.txt has no line for $archive: refusing it"
  got=$($sum "$stage/$archive" | awk '{print $1}')
  [ "$got" = "$want" ] || die "$archive does not match checksums.txt (got $got, want $want): refusing it"
}

# check_attestation: gh logged in checks (a failure stops); otherwise skipped,
# unless --verify.
check_attestation() {
  if command -v gh >/dev/null 2>&1 && gh auth status >/dev/null 2>&1; then
    gh attestation verify "$stage/$archive" --repo "$REPO" >"$stage/gh.out" 2>&1 || {
      cat "$stage/gh.out" >&2
      die "the attestation of $archive does not verify: refusing it"
    }
    say "Attestation of $archive verified."
  elif [ "$verify" = 1 ]; then
    die "--verify needs gh, installed and logged in (gh auth login)"
  else
    say "Note: attestation not checked (gh missing or not logged in); the SHA-256 was."
  fi
}

extract() {
  { mkdir "$stage/x" && tar -xzf "$stage/$archive" -C "$stage/x"; } || die "cannot unpack $archive"
  for f in $(items_src); do
    [ -f "$stage/x/$f" ] || die "$archive has no $f"
  done
}

# items: SRC:DEST:MODE per installed file, DEST relative to the prefix.
items() {
  echo pcb:bin/pcb:0755
  [ "$no_ui" = 1 ] && return 0
  echo pcb-ui:bin/pcb-ui:0755
  [ "$no_desktop" = 1 ] && return 0
  echo pcb-ui.desktop:share/applications/pcb-ui.desktop:0644
  echo pcb-ui.svg:share/icons/hicolor/scalable/apps/pcb-ui.svg:0644
}
items_src() { items | cut -d: -f1; }

# make_prefix: creates the target directories, then makes the prefix absolute.
make_prefix() {
  for it in $(items); do
    rest=${it#*:}
    dest=${rest%:*}
    mkdir -p "$prefix/${dest%/*}" 2>/dev/null || unwritable
  done
  prefix=$(CDPATH='' cd -- "$prefix" && pwd) || unwritable
}
unwritable() {
  die "cannot write to $prefix: choose another directory with --prefix DIR, or run as a user who can write it"
}

# desktop_entry: the archive's entry with Exec= set to the absolute path,
# quoted per the Desktop Entry spec when it holds a reserved character.
desktop_entry() {
  exe=$prefix/bin/pcb-ui
  case $exe in *[[:space:]\'\>\<~\|\&\;\*\?\#\(\)]*) exe="\"$exe\"" ;; esac
  while IFS= read -r l || [ -n "$l" ]; do
    case $l in
    Exec=pcb-ui*) printf 'Exec=%s%s\n' "$exe" "${l#Exec=pcb-ui}" ;;
    *) printf '%s\n' "$l" ;;
    esac
  done <"$stage/x/pcb-ui.desktop" >"$stage/x/pcb-ui.desktop.out" &&
    mv "$stage/x/pcb-ui.desktop.out" "$stage/x/pcb-ui.desktop"
}

# install_files: copies each file beside its target, then renames them all,
# so a failure leaves the previous version whole.
install_files() {
  staged=1
  for it in $(items); do
    src=${it%%:*} rest=${it#*:}
    dest=$prefix/${rest%:*} mode=${rest#*:}
    { cp "$stage/x/$src" "$dest.new.$$" && chmod "$mode" "$dest.new.$$"; } 2>/dev/null || unwritable
  done
  for it in $(items); do
    rest=${it#*:}
    dest=$prefix/${rest%:*}
    mv -f "$dest.new.$$" "$dest" || die "cannot replace $dest"
  done
  staged=0
  if [ "$no_desktop" = 0 ] && command -v update-desktop-database >/dev/null 2>&1; then
    update-desktop-database "$prefix/share/applications" >/dev/null 2>&1 || true
  fi
}

cleanup() {
  if [ "${staged:-0}" = 1 ]; then
    for it in $(items); do
      rest=${it#*:}
      rm -f "$prefix/${rest%:*}.new.$$"
    done
  fi
  [ -n "${stage:-}" ] && rm -rf "$stage"
}

path_hint() {
  case ":$PATH:" in *":$prefix/bin:"*) return 0 ;; esac
  say ""
  say "$prefix/bin is not on your PATH. Add this line to your shell's profile (~/.profile):"
  say "  export PATH=\"$prefix/bin:\$PATH\""
}

# ldd_hint: names the missing libraries of pcb-ui, when ldd finds any.
ldd_hint() {
  [ "$os" = linux ] && [ "$no_ui" = 0 ] || return 0
  command -v ldd >/dev/null 2>&1 || return 0
  missing=$(ldd "$prefix/bin/pcb-ui" 2>/dev/null | awk '/not found/ {print $1}' | tr '\n' ' ')
  [ -n "$missing" ] || return 0
  warn "pcb-ui is missing libraries: $missing"
  warn "  Debian, Ubuntu: sudo apt-get install libgl1-mesa-dev libwayland-dev libxkbcommon-dev xorg-dev"
  warn "  Arch: sudo pacman -S libglvnd libxcursor libxrandr libxinerama libxi wayland libxkbcommon"
}

report() {
  say ""
  "$prefix/bin/pcb" version || warn "$prefix/bin/pcb does not start"
  if [ "$no_ui" = 0 ]; then
    say "Next: start pcb-ui from your applications menu, or run pcb login then pcb install for a service."
  elif [ "$os" = linux ]; then
    say "Next: run pcb login, then pcb install to run it as a service."
  else
    say "Next: run pcb login."
  fi
  if "$prefix/bin/pcb" status --json --daemon >/dev/null 2>&1; then
    say "A pcb daemon (pcb serve, the service or pcb-ui) is running: restart it to use the new version."
  fi
}

main() {
  parse_args "$@"
  check_version
  check_prefix
  detect_platform
  pick_downloader
  stage=$(mktemp -d) || die "cannot create a temporary directory"
  trap cleanup EXIT
  trap 'exit 1' HUP INT TERM
  say "Installing $archive (${tag:-latest release}) under $prefix"
  fetch checksums.txt
  fetch "$archive"
  check_sum
  check_attestation
  extract
  make_prefix
  [ "$no_desktop" = 1 ] || desktop_entry || die "cannot write the desktop entry"
  install_files
  path_hint
  ldd_hint
  report
}

main "$@"
