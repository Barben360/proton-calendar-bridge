#!/usr/bin/env bash
# Test of licensertf.py, and the drift check of packaging/windows/license.rtf.
# Prints PASS last; exits 1 on the first failed case.
# Usage: bash scripts/release/licensertf_test.sh        (make release-test)
set -u

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
root=$(cd "$here/../.." && pwd)
gen=(python3 -I "$here/licensertf.py")
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

fail() { echo "FAIL $*" >&2; exit 1; }
ok() { echo "ok   $*"; }

# The committed RTF is the LICENSE's.
"${gen[@]}" --check "$root/LICENSE" "$root/packaging/windows/license.rtf" ||
  fail "packaging/windows/license.rtf drifted from LICENSE"
ok "license.rtf is LICENSE's"

# A changed LICENSE is caught, its message names the RTF.
sed 's/free of charge/for a fee/' "$root/LICENSE" >"$tmp/LICENSE"
grep -q 'for a fee' "$tmp/LICENSE" || fail "the test's edit of LICENSE did not apply"
msg=$("${gen[@]}" --check "$tmp/LICENSE" "$root/packaging/windows/license.rtf" 2>&1); rc=$?
[ "$rc" = 1 ] || fail "a changed LICENSE: --check exit $rc, want 1"
grep -qF "packaging/windows/license.rtf is not" <<<"$msg" || fail "message: $msg"
ok "a changed LICENSE fails the check"

# Writing then checking agrees; a missing RTF fails.
"${gen[@]}" "$tmp/LICENSE" "$tmp/out.rtf" || fail "write exit $?"
"${gen[@]}" --check "$tmp/LICENSE" "$tmp/out.rtf" || fail "check of a fresh write exit $?"
grep -qF 'for a fee' "$tmp/out.rtf" || fail "the written RTF lacks the text"
"${gen[@]}" --check "$tmp/LICENSE" "$tmp/none.rtf" 2>/dev/null; rc=$?
[ "$rc" = 1 ] || fail "a missing RTF: exit $rc, want 1"
ok "write, then check; a missing RTF fails"

# Paragraphs, escapes and characters beyond ASCII (U+00EB, U+1F600).
printf 'a {b}\nc\\d\n\n\n  Zo\303\253 \360\237\230\200  \n' >"$tmp/in.txt"
"${gen[@]}" "$tmp/in.txt" "$tmp/in.rtf" || fail "write exit $?"
body=$(sed -n 's/.*\\fs18 //p' "$tmp/in.rtf")$'\n'$(sed -n '2,$p' "$tmp/in.rtf")
want='a \{b\} c\\d\par'$'\n''Zo\u235? \u-10179?\u-8704?\par'$'\n''}'
[ "$body" = "$want" ] || fail "escapes: got [$body], want [$want]"
LC_ALL=C grep -q '[^ -~]' "$tmp/in.rtf" && fail "the RTF holds non-ASCII bytes"
head -c 6 "$tmp/in.rtf" | grep -qF '{\rtf1' || fail "no RTF header"
ok "paragraphs, escapes, non-ASCII"

"${gen[@]}" "$tmp/in.txt" 2>/dev/null; rc=$?
[ "$rc" = 2 ] || fail "one argument: exit $rc, want 2"
ok "usage error"
echo PASS
