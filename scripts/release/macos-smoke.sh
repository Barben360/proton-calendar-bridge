#!/usr/bin/env bash
# Installs the release files with install.sh and runs the installed pcb.
# Meant for macOS (CI's macos job, release.yml), written for bash 3.2:
# no GNU-only tool. DIR holds the files flat: pcb_<os>_<arch>.tar.gz (os from
# uname, darwin on a Mac), install.sh and checksums.txt. The script lays them
# out as download/TAG/ in a temp dir, points PCB_INSTALL_BASE_URL at it
# (file://), installs under a temp prefix with a temp HOME, and checks that
# `pcb version --json` reports TAG and `pcb doctor --json` the platform
# <os>/<arch>. Uses jq when found, grep otherwise. Prints PASS last.
# Usage: scripts/release/macos-smoke.sh DIR TAG
set -euo pipefail

die() { echo "macos-smoke.sh: $*" >&2; exit 1; }

(($# == 2)) || die "usage: macos-smoke.sh DIR TAG"
dir=$1 tag=$2
[[ -d $dir ]] || die "no directory $dir"
dir=$(cd "$dir" && pwd)

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case $(uname -m) in
x86_64 | amd64) arch=amd64 ;;
arm64 | aarch64) arch=arm64 ;;
*) die "unsupported machine $(uname -m)" ;;
esac
archive=pcb_${os}_${arch}.tar.gz
for f in "$archive" install.sh checksums.txt; do
  [[ -f $dir/$f ]] || die "missing $dir/$f"
done

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/rel/download/$tag" "$work/home"
cp "$dir/$archive" "$dir/install.sh" "$dir/checksums.txt" "$work/rel/download/$tag/"

# A temp HOME and no token: no real gh login can switch the attestation check on.
env -u GH_TOKEN -u GITHUB_TOKEN HOME="$work/home" \
  PCB_INSTALL_BASE_URL="file://$work/rel" \
  sh "$dir/install.sh" --version "$tag" --prefix "$work/prefix" --no-desktop
pcb=$work/prefix/bin/pcb
[[ -x $pcb ]] || die "install.sh left no $pcb"

version_json=$(env HOME="$work/home" "$pcb" version --json)
doctor_json=$(env HOME="$work/home" "$pcb" doctor --json)

if command -v jq >/dev/null 2>&1; then
  got=$(jq -r '.data.version' <<<"$version_json")
  cmd=$(jq -r '.command' <<<"$doctor_json")
  plat=$(jq -r '[.data.results[] | select(.name == "platform") | .message] | first // ""' <<<"$doctor_json")
else
  got=$(sed -n 's/^ *"version": *"\([^"]*\)".*/\1/p' <<<"$version_json" | head -n 1)
  cmd=$(sed -n 's/^ *"command": *"\([^"]*\)".*/\1/p' <<<"$doctor_json" | head -n 1)
  plat=$(grep -A 2 '"name": *"platform"' <<<"$doctor_json" | sed -n 's/^ *"message": *"\([^"]*\)".*/\1/p' | head -n 1)
fi

[[ $got == "$tag" ]] || die "pcb version reports '$got', want '$tag'"
[[ $cmd == doctor ]] || die "pcb doctor --json has command '$cmd', want doctor"
[[ $plat == "$os/$arch" ]] || die "doctor platform is '$plat', want '$os/$arch'"
echo PASS
