#!/usr/bin/env bash
# Release tag rules (spec/designs/2026-11-14-release-process-design.md §5.1-5.2),
# called by .github/workflows/release.yml and image-build.yml.
# Linux runners only.
#
#   check.sh tag-kind TAG                       stable | prerelease
#   check.sh is-latest TAG                      existing tags on stdin; 0 when TAG is the newest stable
#   check.sh is-latest-minor TAG                same, among the tags of TAG's MAJOR.MINOR line
#   check.sh image-tags TAG LATEST MINOR_LATEST image tags, one per line (flags: yes|no)
#   check.sh image-tag-list                     tags on stdin, one per line; prints the non-blank ones, refuses
#                                               a bad one or none (image-build.yml's tags input)
#   check.sh owner OWNER ACTOR...               0 when every ACTOR is OWNER
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
    [[ $2 == yes || $2 == no ]] || die "LATEST is '$2', want yes or no"
    [[ $3 == yes || $3 == no ]] || die "MINOR_LATEST is '$3', want yes or no"
    version=${1#v}
    echo "$version"
    if stable "$1"; then
      if [[ $3 == yes ]]; then echo "${version%.*}"; fi
      if [[ $2 == yes ]]; then echo latest; fi
    fi
    ;;
  image-tag-list)
    [[ $# -eq 0 ]] || die "usage: image-tag-list (tags on stdin)"
    tags=()
    while IFS= read -r t || [[ -n $t ]]; do
      [[ -n $t ]] || continue
      [[ $t =~ ^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$ ]] || die "not an image tag: '$t'"
      tags+=("$t")
    done
    ((${#tags[@]} > 0)) || die "no tag given"
    printf '%s\n' "${tags[@]}"
    ;;
  owner)
    [[ $# -ge 2 && -n $1 ]] || die "usage: owner OWNER ACTOR..."
    owner=$1
    shift
    for a in "$@"; do
      [[ $a == "$owner" ]] || { echo "check: $a is not the owner $owner" >&2; exit 1; }
    done
    ;;
  on-main)
    [[ $# -ge 1 && $# -le 2 ]] || die "usage: on-main SHA [BASE]"
    git merge-base --is-ancestor "$1" "${2:-origin/main}"
    ;;
  *)
    die "usage: check.sh tag-kind|is-latest|is-latest-minor|image-tags|image-tag-list|owner|on-main ..."
    ;;
esac
