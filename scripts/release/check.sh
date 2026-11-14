#!/usr/bin/env bash
# Release tag rules (spec/designs/2026-11-14-release-process-design.md §5.1-5.2),
# called by .github/workflows/release.yml. Linux runners only.
#
#   check.sh tag-kind TAG                       stable | prerelease
#   check.sh is-latest TAG                      existing tags on stdin; 0 when TAG is the newest stable
#   check.sh is-latest-minor TAG                same, among the tags of TAG's MAJOR.MINOR line
#   check.sh image-tags TAG LATEST MINOR_LATEST image tags, one per line (flags: yes|no)
#   check.sh on-main SHA [BASE]                 0 when SHA is an ancestor of BASE (default origin/main)
set -eu

tag_re='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$'

die() { echo "check: $*" >&2; exit 2; }

# valid TAG: a vMAJOR.MINOR.PATCH[-PRERELEASE] tag.
valid() { [[ $1 =~ $tag_re ]]; }

# stable TAG: valid and without a pre-release suffix.
stable() { valid "$1" && [[ $1 != *-* ]]; }

# ge A B: stable tag A >= stable tag B, field by field as integers.
ge() {
  local a=${1#v} b=${2#v} i
  local -a av bv
  IFS=. read -r -a av <<<"$a"
  IFS=. read -r -a bv <<<"$b"
  for i in 0 1 2; do
    ((10#${av[i]} > 10#${bv[i]})) && return 0
    ((10#${av[i]} < 10#${bv[i]})) && return 1
  done
  return 0
}

# newest TAG [PREFIX]: reads tags on stdin; 0 when TAG is stable and >= every stable
# tag read (restricted to those starting with PREFIX, e.g. "v1.2.").
newest() {
  local tag=$1 prefix=${2:-} other
  stable "$tag" || return 1
  while IFS= read -r other || [[ -n $other ]]; do
    stable "$other" || continue
    [[ $other == "$prefix"* ]] || continue
    ge "$tag" "$other" || return 1
  done
  return 0
}

cmd=${1:-}
[[ $# -gt 0 ]] && shift
case $cmd in
  tag-kind)
    [[ $# -eq 1 ]] || die "usage: tag-kind TAG"
    valid "$1" || die "$1 is not vMAJOR.MINOR.PATCH[-PRERELEASE]"
    if stable "$1"; then echo stable; else echo prerelease; fi
    ;;
  is-latest)
    [[ $# -eq 1 ]] || die "usage: is-latest TAG"
    newest "$1"
    ;;
  is-latest-minor)
    [[ $# -eq 1 ]] || die "usage: is-latest-minor TAG"
    stable "$1" || exit 1
    newest "$1" "${1%.*}."
    ;;
  image-tags)
    [[ $# -eq 3 ]] || die "usage: image-tags TAG LATEST MINOR_LATEST"
    valid "$1" || die "$1 is not vMAJOR.MINOR.PATCH[-PRERELEASE]"
    version=${1#v}
    echo "$version"
    if stable "$1"; then
      if [[ $3 == yes ]]; then echo "${version%.*}"; fi
      if [[ $2 == yes ]]; then echo latest; fi
    fi
    ;;
  on-main)
    [[ $# -ge 1 && $# -le 2 ]] || die "usage: on-main SHA [BASE]"
    git merge-base --is-ancestor "$1" "${2:-origin/main}"
    ;;
  *)
    die "usage: check.sh tag-kind|is-latest|is-latest-minor|image-tags|on-main ..."
    ;;
esac
