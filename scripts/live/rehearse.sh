#!/usr/bin/env bash
# Rehearse the live session offline (plan R2, spec/HUMAN-CHECKS.md steps 6
# and 7): the real pcb binary against pcb-fake, a fake Proton on 127.0.0.1
# seeded with the test data set. Nothing leaves the loopback interface:
# config.toml points pcb at the fake before any login, and a proxy fence
# makes any other HTTP request fail. Every pcb directory (and HOME) is in a
# temporary run root, removed at the end.
#
# Each step prints "--- step N: ..." and stops the run at its first
# mismatch (exit 1); the last line is PASS. A red rehearsal stops the live
# run.
#
#   scripts/live/rehearse.sh            # builds pcb and pcb-fake
#   REHEARSE_PCB=/path/pcb ...          # uses that pcb instead of building one
#   REHEARSE_KEEP=1 ...                 # keeps the run root (it holds a fake account only)
#
# Needs bash, go, curl, jq, base32, sha256sum.
set -euo pipefail

REPO=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
RUN=$(mktemp -d "${TMPDIR:-/tmp}/pcb-rehearse.XXXXXX")
chmod 700 "$RUN"
STEP=0
FAKE_PID=
SERVE_PID=

say() { printf '    %s\n' "$*"; }
fail() {
	printf 'FAIL (step %s): %s\n' "$STEP" "$*" >&2
	exit 1
}
for tool in curl jq base32 go; do
	command -v "$tool" >/dev/null || { printf 'rehearse.sh needs %s on PATH\n' "$tool" >&2; exit 1; }
done
step() {
	STEP=$1
	shift
	printf -- '--- step %s: %s\n' "$STEP" "$*"
}

stop_pid() { # PID: TERM, then wait for it
	local pid=$1
	[ -n "$pid" ] || return 0
	kill -TERM "$pid" 2>/dev/null || return 0
	for _ in $(seq 1 50); do
		kill -0 "$pid" 2>/dev/null || return 0
		sleep 0.1
	done
	kill -KILL "$pid" 2>/dev/null || true
}

cleanup() {
	local rc=$?
	stop_pid "$SERVE_PID"
	stop_pid "$FAKE_PID"
	if [ "$rc" -ne 0 ]; then
		for f in serve.log fake.err; do
			if [ -s "$RUN/$f" ]; then
				printf '\n=== last lines of %s ===\n' "$f" >&2
				tail -n 25 "$RUN/$f" >&2
			fi
		done
	fi
	if [ "${REHEARSE_KEEP:-}" = 1 ]; then
		printf 'run root kept: %s\n' "$RUN" >&2
	else
		rm -rf "$RUN"
	fi
	exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

# run WANT CMD...: runs CMD, its stdout in $RUN/out, stderr in $RUN/err, and
# fails unless it exits WANT.
run() {
	local want=$1 rc=0
	shift
	"$@" >"$RUN/out" 2>"$RUN/err" || rc=$?
	if [ "$rc" -ne "$want" ]; then
		local shown=() a
		for a in "$@"; do # never print a secret of the command line
			case $a in *PASSWORD=* | *TOTP=*) a="${a%%=*}=<redacted>" ;; esac
			shown+=("$a")
		done
		fail "${shown[*]} exited $rc, want $want
stdout: $(cat "$RUN/out")
stderr: $(cat "$RUN/err")"
	fi
}

# has FILE TEXT WHAT: fails unless FILE holds TEXT (a fixed string).
has() {
	grep -qF -- "$2" "$1" || fail "$3: no \"$2\" in $(basename "$1"):
$(head -c 3000 "$1")"
}
lacks() {
	if grep -qF -- "$2" "$1"; then fail "$3: \"$2\" in $(basename "$1")"; fi
}
equal() { [ "$1" = "$2" ] || fail "$3: got \"$1\", want \"$2\""; }

# until_true SECONDS WHAT CMD...: polls CMD every 0.2 s until it succeeds.
until_true() {
	local secs=$1 what=$2 i
	shift 2
	for ((i = 0; i < secs * 5; i++)); do
		if "$@" >/dev/null 2>&1; then return 0; fi
		sleep 0.2
	done
	fail "$what: not within ${secs}s"
}

free_port() {
	local p
	for _ in $(seq 1 100); do
		p=$((20000 + RANDOM % 40000))
		if ! (exec 3<>"/dev/tcp/127.0.0.1/$p") 2>/dev/null; then
			echo "$p"
			return 0
		fi
	done
	fail "no free port on 127.0.0.1"
}

# --------------------------------------------------------------------------
step 1 "build pcb and pcb-fake into the run root"
mkdir -p "$RUN/bin"
if [ -n "${REHEARSE_PCB:-}" ]; then
	cp "$REHEARSE_PCB" "$RUN/bin/pcb"
	say "pcb from REHEARSE_PCB"
else
	make -s -C "$REPO" build OUT="$RUN/bin/pcb" >/dev/null
fi
make -s -C "$REPO" pcb-fake FAKE_OUT="$RUN/bin/pcb-fake" >/dev/null
PCB=$RUN/bin/pcb
FAKE=$RUN/bin/pcb-fake
run 0 "$PCB" version
say "$(head -n 1 "$RUN/out")"

# --------------------------------------------------------------------------
step 2 "isolate every directory in the run root and fence the network"
# No credential or directory of the caller's reaches pcb: everything below
# is the run root's, and HTTP to anything but loopback goes to a closed
# port (Go and curl never proxy 127.0.0.1 or localhost).
unset PCB_USERNAME PCB_PASSWORD PCB_TOTP PCB_LAB_USERNAME PCB_LAB_PASSWORD PCB_LAB_TOTP XDG_RUNTIME_DIR
export HOME=$RUN/home
export XDG_CONFIG_HOME=$RUN/home/.config XDG_STATE_HOME=$RUN/home/.local/state
export PCB_CONFIG_DIR=$RUN/config PCB_STATE_DIR=$RUN/state
FENCE=http://127.0.0.1:9
export HTTPS_PROXY=$FENCE HTTP_PROXY=$FENCE ALL_PROXY=$FENCE https_proxy=$FENCE http_proxy=$FENCE all_proxy=$FENCE
export NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost
mkdir -p "$HOME"
(umask 077 && mkdir -p "$PCB_CONFIG_DIR" "$PCB_STATE_DIR")
[ ${#PCB_STATE_DIR} -lt 90 ] || fail "run root too long for the control socket: set TMPDIR to a short path"
run 0 "$PCB" status --json
KEY_FILE=$(jq -r .data.key_file "$RUN/out")
case $KEY_FILE in
"$RUN"/*) say "pcb resolves its files under $RUN" ;;
*) fail "pcb's key file is $KEY_FILE, outside the run root" ;;
esac

# --------------------------------------------------------------------------
step 3 "start pcb-fake on 127.0.0.1, seeded with the data set"
FAKE_USER=rehearsal
FAKE_PASS=$(head -c 24 /dev/urandom | base64 | tr -d '/+=')
TOTP_SECRET=$(head -c 20 /dev/urandom | base32 | tr -d '=\n')
TOTP_URI="otpauth://totp/$FAKE_USER?secret=$TOTP_SECRET"
(
	umask 077
	printf 'username = "%s"\npassword = "%s"\ntotp_secret = "%s"\n' "$FAKE_USER" "$FAKE_PASS" "$TOTP_SECRET" >"$RUN/account.toml"
)
"$FAKE" serve --seed "$REPO/testdata/seed/dataset.yaml" --account "$RUN/account.toml" >"$RUN/fake.out" 2>"$RUN/fake.err" &
FAKE_PID=$!
until_true 60 "pcb-fake ready" grep -qx 'Serving until SIGINT or SIGTERM' "$RUN/fake.out"
BASE=$(sed -n 's/^Base URL: //p' "$RUN/fake.out")
CTL=$(sed -n 's/^Control URL: //p' "$RUN/fake.out")
DEF_OBJECTS=$(sed -n 's/^Calendar ".*" (default): \([0-9]*\) objects.*/\1/p' "$RUN/fake.out")
case $BASE in http://127.0.0.1:*) ;; *) fail "base URL $BASE is not loopback" ;; esac
case $CTL in http://127.0.0.1:*) ;; *) fail "control URL $CTL is not loopback" ;; esac
equal "$(sed -n 's/^Username: //p' "$RUN/fake.out")" "$FAKE_USER" "fake username"
has "$RUN/fake.out" "Seeded: 4 calendars" "seeded calendars"
{ [ -n "$DEF_OBJECTS" ] && [ "$DEF_OBJECTS" -gt 100 ]; } || fail "default calendar seeded with '$DEF_OBJECTS' objects"
say "fake at $BASE, $(sed -n 's/^Seeded: //p' "$RUN/fake.out")"

# fake_calls METHOD PATH: the requests the fake received there (a path
# ending in / is a prefix).
fake_calls() { curl -sS --fail "$CTL/calls?method=$1&path=$2"; }

# --------------------------------------------------------------------------
step 4 "point pcb at the fake ([proton] base_url) before any login"
PORT=$(free_port)
(
	umask 077
	printf '[proton]\nbase_url = "%s"\n\n[caldav]\nlisten = "127.0.0.1:%s"\n' "$BASE" "$PORT" >"$PCB_CONFIG_DIR/config.toml"
)
has "$PCB_CONFIG_DIR/config.toml" "base_url = \"http://127.0.0.1:" "config.toml points at loopback"
run 0 "$PCB" status --json
equal "$(jq -r .data.phase "$RUN/out")" logged-out "phase before login"
URL=http://localhost:$PORT
DAV=$URL/dav/calendars/me
NETRC=$RUN/netrc

# --------------------------------------------------------------------------
step 5 "pcb serve --wait-for-login: waiting, CalDAV refuses"
"$PCB" serve --wait-for-login >"$RUN/serve.out" 2>"$RUN/serve.log" &
SERVE_PID=$!
until_true 30 "serve waiting for a login" grep -qF 'Waiting for a login: run pcb login' "$RUN/serve.out"
has "$RUN/serve.out" "Thunderbird URL: $URL/" "serve's URL line"
run 0 "$PCB" status --daemon --json
equal "$(jq -r .data.health.serve "$RUN/out")" waiting-for-login "daemon state"
PID=$(jq -r .data.daemon.pid "$RUN/out")
equal "$PID" "$SERVE_PID" "daemon pid"
curl -sS -o /dev/null -D "$RUN/h" -X PROPFIND "$URL/" || fail "PROPFIND without credentials"
has "$RUN/h" "HTTP/1.1 401" "PROPFIND without credentials"
grep -qi '^www-authenticate: Basic realm="pcb"' "$RUN/h" || fail "401 without Basic realm=\"pcb\""

# --------------------------------------------------------------------------
step 6 "pcb login with the 2FA code held by the daemon"
run 10 env -u PCB_TOTP PCB_USERNAME="$FAKE_USER" PCB_PASSWORD="$FAKE_PASS" "$PCB" login --non-interactive
has "$RUN/err" "the account needs a 2FA code" "held login"
has "$RUN/err" "PCB_TOTP=<code> pcb login resume --totp before " "held login hint"
has "$RUN/err" "(the daemon holds the login)" "held login hint"
lacks "$RUN/err" "$FAKE_PASS" "the password is never printed"
run 0 "$PCB" status --json
equal "$(jq -r .data.phase "$RUN/out")" needs-totp "phase while the daemon holds the login"
run 0 "$PCB" status
has "$RUN/out" "login_expires:" "status of a held login"

# --------------------------------------------------------------------------
step 7 "pcb login resume --totp"
run 0 env PCB_TOTP="$TOTP_URI" "$PCB" login resume --totp
has "$RUN/out" "Logged in as $FAKE_USER" "resume"
has "$RUN/out" "CalDAV credentials: pcb client show" "resume creates the default client"
equal "$(fake_calls POST /auth/v4/2fa)" 1 "2FA calls the fake received"

# --------------------------------------------------------------------------
step 8 "the same daemon serves and has synced every calendar"
synced() {
	"$PCB" status --json >"$RUN/st.json" 2>/dev/null &&
		jq -e '.data.health.serve == "serving" and (.data.health.calendars | length) == 4 and ([.data.health.calendars[] | select(.synced_at == null)] | length) == 0 and .data.health.last_pass.ok == true' "$RUN/st.json"
}
until_true 60 "serving with 4 synced calendars" synced
equal "$(jq -r .data.daemon.pid "$RUN/st.json")" "$PID" "daemon pid after the login (no restart)"
equal "$(jq -r .data.phase "$RUN/st.json")" ready "phase"
equal "$(jq -r .data.username "$RUN/st.json")" "$FAKE_USER" "username"
run 0 "$PCB" sync
has "$RUN/out" "pcb serve is running and already syncs: see pcb status" "pcb sync while serving"
run 0 "$PCB" calendar list --json
jq '.data.calendars' "$RUN/out" >"$RUN/calendars.json"
equal "$(jq length "$RUN/calendars.json")" 4 "calendars listed"
equal "$(jq -r '[.[] | select(.default)] | length' "$RUN/calendars.json")" 1 "one default calendar"
equal "$(jq -r '.[] | select(.default) | "\(.type) \(.read_only) \(.keys)"' "$RUN/calendars.json")" "normal false ok" "default calendar"
equal "$(jq -r '.[] | select(.name == "Shared with me (view only)") | .read_only' "$RUN/calendars.json")" true "shared calendar read-only"
run 0 "$PCB" status
has "$RUN/out" "render: undecryptable 0, unverified 0, duplicate masters 0, dropped alarms 0" "render counters"
lacks "$RUN/out" "alert:" "no alert while serving"

# --------------------------------------------------------------------------
step 9 "pcb client show (--reveal)"
run 0 "$PCB" client show --json
equal "$(jq -r .data.password "$RUN/out")" null "password without --reveal"
run 0 "$PCB" client show --json --reveal
equal "$(jq -r .data.username "$RUN/out")" default "client username"
equal "$(jq -r .data.url "$RUN/out")" "$URL/" "client URL"
PASS1=$(jq -r .data.password "$RUN/out")
{ [ -n "$PASS1" ] && [ "$PASS1" != null ]; } || fail "no password with --reveal"
run 0 "$PCB" client show
has "$RUN/out" "Username: default" "client show text"
has "$RUN/out" "Password: $PASS1" "client show text"
(umask 077 && printf 'machine localhost login default password %s\n' "$PASS1" >"$NETRC")

# status METHOD URL [curl args]: the HTTP status, with the netrc.
status() {
	local m=$1 u=$2
	shift 2
	curl -sS --netrc-file "$NETRC" -o "$RUN/body" -D "$RUN/h" -w '%{http_code}' -X "$m" "$@" "$u"
}
propfind0() { status PROPFIND "$1" -H 'Depth: 0'; }

# --------------------------------------------------------------------------
step 10 "CalDAV discovery: well-known, PROPFIND, calendar home, privileges"
equal "$(status GET "$URL/.well-known/caldav")" 308 "GET /.well-known/caldav"
grep -qi '^location: /dav/principals/me/' "$RUN/h" || fail "well-known Location"
equal "$(propfind0 "$URL/")" 207 "PROPFIND /"
equal "$(status PROPFIND "$DAV/" -H 'Depth: 1')" 207 "PROPFIND the calendar home"
equal "$(grep -o '<C:calendar/>' "$RUN/body" | wc -l | tr -d ' ')" 4 "calendars in the home"
calpath() { printf %s "$1" | sha256sum | cut -c1-16; }
DEF_ID=$(jq -r '.[] | select(.default) | .id' "$RUN/calendars.json")
SHARED_ID=$(jq -r '.[] | select(.read_only) | .id' "$RUN/calendars.json")
CAL=$DAV/$(calpath "$DEF_ID")
PRIV='<D:propfind xmlns:D="DAV:"><D:prop><D:current-user-privilege-set/></D:prop></D:propfind>'
equal "$(status PROPFIND "$CAL/" -H 'Depth: 0' -H 'Content-Type: text/xml' --data "$PRIV")" 207 "privileges of the default calendar"
has "$RUN/body" "<D:privilege><D:write/></D:privilege>" "default calendar writable"
equal "$(status PROPFIND "$DAV/$(calpath "$SHARED_ID")/" -H 'Depth: 0' -H 'Content-Type: text/xml' --data "$PRIV")" 207 "privileges of the shared calendar"
lacks "$RUN/body" "<D:write/>" "shared calendar read-only"

# --------------------------------------------------------------------------
step 11 "sync-collection: every seeded object of the default calendar"
sync_collection() { # TOKEN: the REPORT, body in $RUN/body
	status REPORT "$CAL/" -H 'Depth: 1' -H 'Content-Type: application/xml' \
		--data "<?xml version=\"1.0\"?><D:sync-collection xmlns:D=\"DAV:\"><D:sync-token>$1</D:sync-token><D:sync-level>1</D:sync-level><D:prop><D:getetag/></D:prop></D:sync-collection>"
}
equal "$(sync_collection '')" 207 "initial sync-collection"
equal "$(grep -o '<D:response>' "$RUN/body" | wc -l | tr -d ' ')" "$DEF_OBJECTS" "objects in the initial sync-collection"
TOKEN1=$(grep -o '<D:sync-token>[^<]*' "$RUN/body" | sed 's/<D:sync-token>//')
[ -n "$TOKEN1" ] || fail "no sync-token"
say "$DEF_OBJECTS objects, token ${TOKEN1:0:24}..."

# --------------------------------------------------------------------------
step 12 "GET a series with its exception, multiget with attendees and unicode"
SERIES=$CAL/pcb-seed-s343@pcb-fake.invalid.ics
equal "$(status GET "$SERIES")" 200 "GET a seeded series"
tr -d '\r' <"$RUN/body" >"$RUN/series.ics"
has "$RUN/series.ics" "RRULE:FREQ=WEEKLY;INTERVAL=2;COUNT=20" "series rule"
has "$RUN/series.ics" "EXDATE;TZID=Europe/Paris:20251014T120000" "deleted occurrence"
has "$RUN/series.ics" "RECURRENCE-ID;TZID=Europe/Paris:20250930T120000" "edited occurrence"
has "$RUN/series.ics" "SUMMARY:pcb-seed s343.e Every two weeks (edited)" "edited occurrence"
MEETING=/dav/calendars/me/$(calpath "$DEF_ID")/pcb-fake-meeting@pcb-fake.invalid.ics
TEXT=/dav/calendars/me/$(calpath "$DEF_ID")/pcb-seed-s349@pcb-fake.invalid.ics
equal "$(status REPORT "$CAL/" -H 'Depth: 1' -H 'Content-Type: application/xml' \
	--data "<?xml version=\"1.0\"?><C:calendar-multiget xmlns:D=\"DAV:\" xmlns:C=\"urn:ietf:params:xml:ns:caldav\"><D:prop><D:getetag/><C:calendar-data/></D:prop><D:href>$MEETING</D:href><D:href>$TEXT</D:href></C:calendar-multiget>")" 207 "multiget"
equal "$(grep -o '<C:calendar-data>' "$RUN/body" | wc -l | tr -d ' ')" 2 "objects in the multiget"
has "$RUN/body" "ATTENDEE;CN=Lea;ROLE=REQ-PARTICIPANT;PARTSTAT=ACCEPTED:mailto:lea@example.o" "attendee served"
has "$RUN/body" "SUMMARY:pcb-seed s349 Café ☕ 日本語" "unicode summary served"

# --------------------------------------------------------------------------
step 13 "PUT a Thunderbird event (create), read back at once"
EV_UID=$(tr -d '\n' </proc/sys/kernel/random/uuid 2>/dev/null || echo "rehearsal-$$-$RANDOM")
EV=$CAL/$EV_UID.ics
# tb_event SUMMARY GENERATION: the body Thunderbird PUTs for a new event or
# an edit (E08 §6.1: its PRODID, a generated VTIMEZONE, X-MOZ-GENERATION, a
# default alarm), CRLF.
tb_event() {
	sed 's/$/\r/' <<EOF
BEGIN:VCALENDAR
PRODID:-//Mozilla.org/NONSGML Mozilla Calendar V1.1//EN
VERSION:2.0
BEGIN:VTIMEZONE
TZID:Europe/Paris
BEGIN:DAYLIGHT
TZOFFSETFROM:+0100
TZOFFSETTO:+0200
TZNAME:CEST
DTSTART:19700329T020000
RRULE:FREQ=YEARLY;BYDAY=-1SU;BYMONTH=3
END:DAYLIGHT
BEGIN:STANDARD
TZOFFSETFROM:+0200
TZOFFSETTO:+0100
TZNAME:CET
DTSTART:19701025T030000
RRULE:FREQ=YEARLY;BYDAY=-1SU;BYMONTH=10
END:STANDARD
END:VTIMEZONE
BEGIN:VEVENT
CREATED:20261112T090000Z
LAST-MODIFIED:20261112T09${2}000Z
DTSTAMP:20261112T09${2}000Z
UID:$EV_UID
SUMMARY:$1
DTSTART;TZID=Europe/Paris:20270120T100000
DTEND;TZID=Europe/Paris:20270120T110000
LOCATION:Room 1
DESCRIPTION:written by the rehearsal
TRANSP:OPAQUE
X-MOZ-GENERATION:$2
BEGIN:VALARM
ACTION:DISPLAY
TRIGGER;VALUE=DURATION:-PT15M
DESCRIPTION:Default Mozilla Description
END:VALARM
END:VEVENT
END:VCALENDAR
EOF
}
put() { # FILE [curl args]
	local f=$1
	shift
	status PUT "$EV" -H 'Content-Type: text/calendar; charset=utf-8' --data-binary @"$f" "$@"
}
etag() { status GET "$EV" >/dev/null && grep -i '^etag:' "$RUN/h" | sed 's/^[^:]*: *//' | tr -d '\r'; }
write_log_clean() {
	lacks "$RUN/serve.log" "not confirmed" "$1"
	lacks "$RUN/serve.log" "refusing a write" "$1"
	lacks "$RUN/serve.log" "level=ERROR" "$1"
}
SYNC_PATH=/calendar/v1/$DEF_ID/events/sync
WRITES0=$(fake_calls PUT "$SYNC_PATH")
tb_event "pcb-rehearsal created" 1 >"$RUN/create.ics"
equal "$(put "$RUN/create.ics" -H 'If-None-Match: *')" 201 "PUT create"
equal "$(fake_calls PUT "$SYNC_PATH")" $((WRITES0 + 1)) "writes the fake received"
equal "$(status GET "$EV")" 200 "GET right after the create (pcb's read-back stored it)"
tr -d '\r' <"$RUN/body" >"$RUN/got.ics"
has "$RUN/got.ics" "UID:$EV_UID" "read back"
has "$RUN/got.ics" "SUMMARY:pcb-rehearsal created" "read back"
has "$RUN/got.ics" "X-MOZ-GENERATION:1" "read back"
has "$RUN/got.ics" "TRIGGER:-PT15M" "read back alarm"
has "$RUN/got.ics" "LOCATION:Room 1" "read back"
ETAG1=$(etag)
[ -n "$ETAG1" ] || fail "no ETag on GET"
write_log_clean "serve.log after the create"

# --------------------------------------------------------------------------
step 14 "PUT an edit with If-Match; a stale If-Match is 412"
tb_event "pcb-rehearsal renamed" 2 >"$RUN/update.ics"
equal "$(put "$RUN/update.ics" -H "If-Match: $ETAG1")" 204 "PUT update"
equal "$(fake_calls PUT "$SYNC_PATH")" $((WRITES0 + 2)) "writes the fake received"
equal "$(status GET "$EV")" 200 "GET after the update"
tr -d '\r' <"$RUN/body" >"$RUN/got.ics"
has "$RUN/got.ics" "SUMMARY:pcb-rehearsal renamed" "read back the edit"
has "$RUN/got.ics" "X-MOZ-GENERATION:2" "read back the edit"
ETAG2=$(etag)
{ [ -n "$ETAG2" ] && [ "$ETAG2" != "$ETAG1" ]; } || fail "ETag after the edit: '$ETAG2' (before '$ETAG1')"
equal "$(put "$RUN/update.ics" -H "If-Match: $ETAG1")" 412 "PUT with the stale ETag"
equal "$(fake_calls PUT "$SYNC_PATH")" $((WRITES0 + 2)) "a 412 writes nothing"
equal "$(sync_collection "$TOKEN1")" 207 "sync-collection since step 11"
has "$RUN/body" "$EV_UID.ics" "the change since the token"
write_log_clean "serve.log after the edit"

# --------------------------------------------------------------------------
step 15 "DELETE with If-Match, then 404"
equal "$(status DELETE "$EV" -H "If-Match: $ETAG2")" 204 "DELETE"
equal "$(fake_calls PUT "$SYNC_PATH")" $((WRITES0 + 3)) "writes the fake received"
equal "$(status GET "$EV")" 404 "GET after the delete"
equal "$(sync_collection "$TOKEN1")" 207 "sync-collection after the delete"
has "$RUN/body" "$EV_UID.ics</D:href><D:status>HTTP/1.1 404 Not Found</D:status>" "the deletion since the token"
write_log_clean "serve.log after the delete"

# --------------------------------------------------------------------------
step 16 "pcb client rotate default: the old password is refused, no restart"
run 0 "$PCB" client rotate default
cat "$RUN/out" "$RUN/err" >"$RUN/rotate.txt"
has "$RUN/rotate.txt" "Hint: update the password in your CalDAV client" "rotate"
run 0 "$PCB" client show --json --reveal
PASS2=$(jq -r .data.password "$RUN/out")
{ [ -n "$PASS2" ] && [ "$PASS2" != "$PASS1" ]; } || fail "no new password"
has "$RUN/rotate.txt" "$PASS2" "rotate prints the new password"
equal "$(propfind0 "$URL/")" 401 "the old password"
(umask 077 && printf 'machine localhost login default password %s\n' "$PASS2" >"$NETRC")
equal "$(propfind0 "$URL/")" 207 "the new password"
run 0 "$PCB" status --json
equal "$(jq -r .data.daemon.pid "$RUN/out")" "$PID" "daemon pid after the rotate"

# --------------------------------------------------------------------------
step 16b "two access-token expiries in a row: each refreshed, the session stays"
# Proton refuses a refreshed pair's refresh token unless the refresh carries
# that pair's access token (E01, live 2026-11-12), and so does the fake: the
# second refresh is the one a client without it loses.
for n in 1 2; do
	before=$(fake_calls POST /auth/v4/refresh)
	curl -sS --fail -X POST "$CTL/expire-access-tokens" >/dev/null || fail "control: expire-access-tokens"
	run 0 "$PCB" calendar list --json
	equal "$(jq '.data.calendars | length' "$RUN/out")" 4 "calendars listed after expiry $n"
	after=$(fake_calls POST /auth/v4/refresh)
	[ "$after" -gt "$before" ] || fail "expiry $n: no refresh reached the fake ($before, then $after)"
done
run 0 "$PCB" status --json
equal "$(jq -r .data.phase "$RUN/out")" ready "phase after two refreshes"
equal "$(jq -r .data.daemon.pid "$RUN/out")" "$PID" "daemon pid after two refreshes"

# --------------------------------------------------------------------------
step 17 "the fake revokes the session: needs-login, CalDAV 503, no restart"
curl -sS --fail -X POST "$CTL/revoke-sessions" >/dev/null || fail "control: revoke-sessions"
propfind0 "$DAV/" >/dev/null || fail "the authenticated request before the revoke check" # one authenticated request, as the live step sends
waiting() {
	"$PCB" status --json >"$RUN/st.json" 2>/dev/null && jq -e '.data.health.serve == "waiting-for-login"' "$RUN/st.json"
}
until_true 75 "the daemon notices the ended session" waiting
equal "$(jq -r .data.daemon.pid "$RUN/st.json")" "$PID" "daemon pid after the revoke"
run 0 "$PCB" status
has "$RUN/out" "daemon: waiting for a login since" "status after the revoke"
has "$RUN/out" "alert: error needs-login: the Proton session has ended (fix: run pcb login)" "status after the revoke"
equal "$(propfind0 "$DAV/")" 503 "authenticated PROPFIND of the calendar home"
grep -qi '^retry-after: 30' "$RUN/h" || fail "503 without Retry-After: 30"
equal "$(curl -sS -o /dev/null -w '%{http_code}' -X PROPFIND -H 'Depth: 0' "$DAV/")" 401 "PROPFIND without credentials"
has "$RUN/serve.log" "the Proton session has ended" "serve.log after the revoke"

# --------------------------------------------------------------------------
step 18 "re-login from another process: the 60 s guard, then no restart, same client password"
# pcb refuses a login within 60 s of the last one (AGENTS.md), as the live
# run must respect: rehearse the refusal, then wait it out.
rc=0
env PCB_USERNAME="$FAKE_USER" PCB_PASSWORD="$FAKE_PASS" PCB_TOTP="$TOTP_URI" "$PCB" login --non-interactive >"$RUN/out" 2>"$RUN/err" || rc=$?
if [ "$rc" -eq 20 ]; then
	has "$RUN/err" "the previous login attempt was too recent" "a login within 60 s"
	WAIT=$(sed -n 's/^Hint: retry in \([0-9]*\)s$/\1/p' "$RUN/err")
	[ -n "$WAIT" ] || fail "no retry delay in: $(cat "$RUN/err")"
	say "login guard: retry in ${WAIT}s (waiting it out)"
	sleep "$((WAIT + 1))"
	run 0 env PCB_USERNAME="$FAKE_USER" PCB_PASSWORD="$FAKE_PASS" PCB_TOTP="$TOTP_URI" "$PCB" login --non-interactive
else
	equal "$rc" 0 "re-login more than 60 s after the last one"
fi
has "$RUN/out" "Logged in as $FAKE_USER" "re-login"
lacks "$RUN/out" "CalDAV credentials" "re-login keeps the default client"
until_true 60 "serving again" synced
equal "$(jq -r .data.daemon.pid "$RUN/st.json")" "$PID" "daemon pid after the re-login"
equal "$(propfind0 "$DAV/")" 207 "PROPFIND of the calendar home after the re-login"
run 0 "$PCB" client show --json --reveal
equal "$(jq -r .data.password "$RUN/out")" "$PASS2" "default client's password after the re-login"

# --------------------------------------------------------------------------
step 19 "pcb logout revokes the session; the daemon waits again"
REVOKES0=$(fake_calls DELETE /auth/v4)
run 0 "$PCB" logout --json
equal "$(jq -r .data.revoked "$RUN/out")" true "logout revoked"
equal "$(fake_calls DELETE /auth/v4)" $((REVOKES0 + 1)) "session revocations the fake received"
until_true 30 "waiting after the logout" waiting
equal "$(jq -r .data.phase "$RUN/st.json")" logged-out "phase after the logout"
equal "$(jq -r .data.daemon.pid "$RUN/st.json")" "$PID" "daemon pid after the logout"
equal "$(propfind0 "$DAV/")" 503 "CalDAV after the logout"

# --------------------------------------------------------------------------
step 20 "SIGTERM stops pcb serve with exit 0"
kill -TERM "$SERVE_PID"
rc=0
wait "$SERVE_PID" || rc=$?
SERVE_PID=
equal "$rc" 0 "pcb serve exit code"
lacks "$RUN/serve.log" "level=ERROR" "serve.log"
lacks "$RUN/serve.log" "$FAKE_PASS" "serve.log never holds the account password"
lacks "$RUN/serve.log" "$PASS2" "serve.log never holds the CalDAV password"

printf 'PASS: the live session rehearsed against pcb-fake (%s steps)\n' "$STEP"
