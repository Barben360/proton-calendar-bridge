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
}

test_image_tags() {
  [ "$("$check" image-tags v1.2.3 yes yes)" = $'1.2.3\n1.2\nlatest' ] || fail "v1.2.3 yes yes"
  [ "$("$check" image-tags v1.2.4 no yes)" = $'1.2.4\n1.2' ] || fail "v1.2.4 no yes"
  [ "$("$check" image-tags v1.2.4 no no)" = 1.2.4 ] || fail "v1.2.4 no no"
  [ "$("$check" image-tags v1.3.0-rc.1 yes yes)" = 1.3.0-rc.1 ] || fail "prerelease"
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

for case_name in test_tag_kind test_is_latest test_is_latest_minor test_image_tags test_on_main; do
  "$case_name"
  echo "ok   $case_name"
done
echo PASS
