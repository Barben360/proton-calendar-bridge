#!/usr/bin/env bash
# Packages one release archive, flat contents: OUTDIR/pcb_<OS>_<ARCH>.tar.gz
# (linux, darwin) or OUTDIR/pcb_windows_amd64.zip (windows).
# Reproducible: sorted names, fixed mtime (SOURCE_DATE_EPOCH, default the HEAD
# commit time), owner 0:0, modes 0755 (binaries) and 0644 (the rest).
# The tarballs need GNU tar: Linux runners, or macOS with gnu-tar's gnubin
# first on PATH. The zip is written by mkzip.py (python3 zipfile, level 9, no
# extra fields): the same bytes on any host with the same zlib, and no
# dependency on an Info-ZIP zip, which a workstation may lack.
# Usage: scripts/release/package.sh OS ARCH BINDIR OUTDIR
#   OS linux|darwin|windows, ARCH amd64|arm64 (windows: amd64 only); linux
#   needs BINDIR/pcb and BINDIR/pcb-ui, darwin BINDIR/pcb, windows
#   BINDIR/pcb.exe and BINDIR/pcb-ui.exe. Exit 2 on a refusal.
set -euo pipefail

die() { echo "package.sh: $*" >&2; exit 2; }

(($# == 4)) || die "usage: package.sh OS ARCH BINDIR OUTDIR"
os=$1 arch=$2 bindir=$3 outdir=$4
case $os in linux | darwin | windows) ;; *) die "unknown OS '$os' (linux, darwin or windows)" ;; esac
case $arch in amd64 | arm64) ;; *) die "unknown ARCH '$arch' (amd64 or arm64)" ;; esac
[[ $os != windows || $arch == amd64 ]] || die "windows has only ARCH amd64, not '$arch'"

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
bins=(pcb)
[[ $os == linux ]] && bins+=(pcb-ui)
[[ $os == windows ]] && bins=(pcb.exe pcb-ui.exe)
for b in "${bins[@]}"; do
  [[ -f $bindir/$b ]] || die "missing binary $bindir/$b"
done

mkdir -p "$outdir"
outdir=$(cd "$outdir" && pwd)
bindir=$(cd "$bindir" && pwd)
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
cd "$stage"
for b in "${bins[@]}"; do install -m 0755 "$bindir/$b" "$stage/$b"; done
install -m 0644 "$root/LICENSE" "$root/README.md" "$stage/"
if [[ $os == linux ]]; then
  install -m 0644 "$root/packaging/pcb-ui.desktop" "$stage/pcb-ui.desktop"
  install -m 0644 "$root/internal/ui/desktop/icons/app.svg" "$stage/pcb-ui.svg"
fi

epoch=${SOURCE_DATE_EPOCH:-$(git -C "$root" log -1 --format=%ct 2>/dev/null)} || die "no SOURCE_DATE_EPOCH and no git history"
if [[ $os == windows ]]; then
  archive=$outdir/pcb_${os}_${arch}.zip
  rm -f "$archive"
  python3 -I "$root/scripts/release/mkzip.py" "$archive" "$epoch" "$stage" LICENSE README.md "${bins[@]}" || exit 2
  echo "$archive"
  exit 0
fi
archive=$outdir/pcb_${os}_${arch}.tar.gz
# gzip -n: no name or time in the header.
tar --sort=name --owner=0 --group=0 --numeric-owner --mtime="@$epoch" \
  -cf - -- * | gzip -n >"$archive"
echo "$archive"
