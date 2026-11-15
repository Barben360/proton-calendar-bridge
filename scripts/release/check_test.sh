#!/usr/bin/env bash
# Test of check.sh (bats-free). Prints PASS last; exits 1 on the first failed case.
# Usage: bash scripts/release/check_test.sh        (make release-test)
set -eu

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
check=$here/check.sh
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
case_name=

fail() { echo "FAIL $case_name: $*" >&2; exit 1; }

# status CMD...: the exit status of CMD, whatever it is.
status() { local s=0; "$@" >/dev/null 2>&1 || s=$?; echo "$s"; }

# want_status LABEL WANT CMD...
want_status() {
  local label=$1 want=$2; shift 2
  local got; got=$(status "$@")
  [ "$got" = "$want" ] || fail "$label: exit $got, want $want"
}

# stdin_status WANT TAG STDIN SUBCOMMAND: exit status of `check.sh SUBCOMMAND TAG` fed STDIN.
stdin_status() {
  local want=$1 tag=$2 input=$3 sub=$4 got=0
  printf '%s' "$input" | "$check" "$sub" "$tag" >/dev/null 2>&1 || got=$?
  [ "$got" = "$want" ] || fail "$sub $tag over [$input]: exit $got, want $want"
}

test_tag_kind() {
  [ "$("$check" tag-kind v1.2.3)" = stable ] || fail "v1.2.3 not stable"
  [ "$("$check" tag-kind v0.1.0-rc.1)" = prerelease ] || fail "v0.1.0-rc.1 not prerelease"
  local bad
  for bad in 1.2.3 v1.2 v01.2.3 v1.2.3+meta v1.2.3- vx.y.z ""; do
    want_status "refuse '$bad'" 2 "$check" tag-kind "$bad"
  done
  local err; err=$("$check" tag-kind v1.2 2>&1 >/dev/null || true)
  [ "$err" = "check: v1.2 is not vMAJOR.MINOR.PATCH[-PRERELEASE]" ] || fail "message: $err"
}

test_is_latest() {
  stdin_status 1 v1.3.0 $'v1.2.9\nv1.10.0\n' is-latest
  stdin_status 0 v1.10.0 $'v1.9.0\n' is-latest
  stdin_status 1 v1.2.4 $'v1.3.0\n' is-latest
  stdin_status 1 v2.0.0-rc.1 $'v1.0.0\n' is-latest
  stdin_status 0 v1.0.0 '' is-latest
  stdin_status 0 v1.4.0 $'garbage\nv1.3.0\nv9.9.9-rc.1\n\nlatest\n' is-latest
  stdin_status 0 v1.0.0 $'v1.0.0\n' is-latest
}

test_is_latest_minor() {
  stdin_status 0 v1.2.4 $'v1.3.0\nv1.2.3\n' is-latest-minor
  stdin_status 1 v1.2.3 $'v1.2.10\n' is-latest-minor
  stdin_status 1 v1.2.3-rc.1 '' is-latest-minor
  # v1.20.0 is another line than v1.2: the prefix is "v1.2.", not "v1.2".
  stdin_status 0 v1.2.4 $'v1.20.0\n' is-latest-minor
}

test_image_tags() {
  [ "$("$check" image-tags v1.2.3 yes yes)" = $'1.2.3\n1.2\nlatest' ] || fail "v1.2.3 yes yes"
  [ "$("$check" image-tags v1.2.4 no yes)" = $'1.2.4\n1.2' ] || fail "v1.2.4 no yes"
  [ "$("$check" image-tags v1.2.4 no no)" = 1.2.4 ] || fail "v1.2.4 no no"
  [ "$("$check" image-tags v1.3.0-rc.1 yes yes)" = 1.3.0-rc.1 ] || fail "prerelease"
  # Flags other than yes|no are refused, a pre-release's too.
  local flags l m
  for flags in "true no" "no Yes" "1 0" "yes -"; do
    read -r l m <<<"$flags"
    want_status "image-tags v1.2.3 $flags" 2 "$check" image-tags v1.2.3 "$l" "$m"
  done
  want_status "image-tags empty LATEST" 2 "$check" image-tags v1.2.3 "" no
  want_status "image-tags empty MINOR_LATEST" 2 "$check" image-tags v1.2.3 yes ""
  want_status "image-tags prerelease maybe" 2 "$check" image-tags v1.3.0-rc.1 maybe no
}

# image-build.yml's publish job runs no repository code, so it repeats
# image-tag-list's grammar inline: both must say the same.
test_tag_grammar_in_publish() {
  local wf=$here/../../.github/workflows/image-build.yml ours theirs
  ours=$(grep -F "not an image tag" "$check" | sed -n 's/.*=~ \(.*\) \]\].*/\1/p')
  theirs=$(grep -F "not an image tag" "$wf" | sed -n 's/.*=~ \(.*\) \]\].*/\1/p')
  if [ -z "$ours" ] || [ "$(wc -l <<<"$ours")" != 1 ]; then fail "no single grammar in check.sh: [$ours]"; fi
  if [ -z "$theirs" ] || [ "$(wc -l <<<"$theirs")" != 1 ]; then fail "no single grammar in image-build.yml: [$theirs]"; fi
  [ "$ours" = "$theirs" ] || fail "check.sh has $ours, image-build.yml's publish $theirs"
}

test_on_main() {
  local r=$tmp/repo
  git init -q -b main "$r"
  git -C "$r" -c user.name=t -c user.email=t@example.invalid commit -q --allow-empty -m on-main
  local main_sha; main_sha=$(git -C "$r" rev-parse HEAD)
  git -C "$r" checkout -q -b other
  git -C "$r" -c user.name=t -c user.email=t@example.invalid commit -q --allow-empty -m off-main
  local other_sha; other_sha=$(git -C "$r" rev-parse HEAD)
  (cd "$r" && "$check" on-main "$main_sha" main) || fail "main commit refused"
  # shellcheck disable=SC2016 # $1..$3 are for the inner bash
  want_status "other-branch commit" 1 bash -c 'cd "$1" && "$2" on-main "$3" main' _ "$r" "$check" "$other_sha"
}

# image_tag_list STDIN: stdout of `check.sh image-tag-list` fed STDIN.
image_tag_list() { printf '%s' "$1" | "$check" image-tag-list; }

test_image_tag_list() {
  [ "$(image_tag_list $'1.2.3\n1.2\nlatest\n')" = $'1.2.3\n1.2\nlatest' ] || fail "release tags"
  # Blank lines are skipped; a last line with no newline counts.
  [ "$(image_tag_list $'\nsha-0123456789ab\n\n')" = sha-0123456789ab ] || fail "blank lines"
  [ "$(image_tag_list 'a_b.c-d')" = a_b.c-d ] || fail "no trailing newline"
  local max; max=$(printf 'a%.0s' {1..128})
  [ "$(image_tag_list "$max")" = "$max" ] || fail "128 characters refused"
  local bad
  for bad in "${max}a" 'bad tag' -lead .lead a/b a:b ' x' $'ok\nb@d' $'ok\n\t'; do
    printf '%s' "$bad" | want_status "refuse [$bad]" 2 "$check" image-tag-list
  done
  local err
  err=$(image_tag_list $'1.2.3\nbad tag\n' 2>&1 >/dev/null || true)
  [ "$err" = "check: not an image tag: 'bad tag'" ] || fail "message: $err"
  for bad in '' $'\n\n'; do
    printf '%s' "$bad" | want_status "refuse no tag [$bad]" 2 "$check" image-tag-list
  done
  err=$(image_tag_list $'\n' 2>&1 >/dev/null || true)
  [ "$err" = "check: no tag given" ] || fail "message: $err"
  # Nothing is printed when any tag is refused: no partial list.
  [ -z "$(image_tag_list $'1.2.3\nbad tag\n' 2>/dev/null)" ] || fail "partial list printed"
}

test_reachable() {
  local o=$tmp/origin w=$tmp/work g=(-c user.name=t -c user.email=t@example.invalid -c commit.gpgSign=false -c tag.gpgSign=false)
  git init -q -b main "$o"
  git -C "$o" "${g[@]}" commit -q --allow-empty -m on-main
  local on_main; on_main=$(git -C "$o" rev-parse HEAD)
  git -C "$o" checkout -q --detach
  git -C "$o" "${g[@]}" commit -q --allow-empty -m tag-only
  local tagged; tagged=$(git -C "$o" rev-parse HEAD)
  git -C "$o" "${g[@]}" tag -a -m t1 t1
  git -C "$o" checkout -q -b feature main
  git -C "$o" "${g[@]}" commit -q --allow-empty -m feature-only
  local feature; feature=$(git -C "$o" rev-parse HEAD)
  git -C "$o" checkout -q main
  git clone -q "$o" "$w"
  git -C "$w" "${g[@]}" commit -q --allow-empty -m local-only
  local local_only; local_only=$(git -C "$w" rev-parse HEAD)
  # shellcheck disable=SC2016 # $1..$3 are for the inner bash
  local in_w='cd "$1" && "$2" reachable "$3"'
  want_status "a commit of a branch" 0 bash -c "$in_w" _ "$w" "$check" "$on_main"
  # On an unmerged branch other than the default one: still this repository's.
  want_status "a commit of a non-default branch only" 0 bash -c "$in_w" _ "$w" "$check" "$feature"
  want_status "a commit of a tag only" 0 bash -c "$in_w" _ "$w" "$check" "$tagged"
  # The local branch holds it, no remote branch nor tag does.
  want_status "a commit of no remote branch nor tag" 1 bash -c "$in_w" _ "$w" "$check" "$local_only"
  want_status "an unknown commit" 1 bash -c "$in_w" _ "$w" "$check" 0123456789abcdef0123456789abcdef01234567
  want_status "a short SHA" 2 bash -c "$in_w" _ "$w" "$check" "${on_main::12}"
  want_status "a branch name" 2 bash -c "$in_w" _ "$w" "$check" main
}

test_msi_version() {
  [ "$("$check" msi-version v1.2.3)" = 1.2.3 ] || fail "v1.2.3"
  [ "$("$check" msi-version v0.2.0-rc.1)" = 0.2.0 ] || fail "v0.2.0-rc.1"
  [ "$("$check" msi-version v255.255.65535)" = 255.255.65535 ] || fail "the largest MSI version"
  # Not a tag (a pull request's build): 0.0.0.
  [ "$("$check" msi-version "")" = 0.0.0 ] || fail "no tag"
  local bad
  # 2^64+1 wraps to 1 in bash arithmetic: refused by its length.
  for bad in 1.2.3 v1.2 v1.2.3+meta v1.2.3- v256.0.0 v0.256.0 v0.0.65536 "v1.2.3 " \
    v18446744073709551617.0.0 v0.18446744073709551617.0 v0.0.18446744073709551617; do
    want_status "msi-version refuses '$bad'" 2 "$check" msi-version "$bad"
  done
  want_status "msi-version with two tags" 2 "$check" msi-version v1.2.3 v1.2.4
  local err; err=$("$check" msi-version v256.0.0 2>&1 >/dev/null || true)
  [ "$err" = "check: v256.0.0: an MSI version is at most 255.255.65535" ] || fail "message: $err"
}

for case_name in test_tag_kind test_is_latest test_is_latest_minor test_image_tags test_image_tag_list test_tag_grammar_in_publish test_on_main test_reachable test_msi_version; do
  "$case_name"
  echo "ok   $case_name"
done
echo PASS
