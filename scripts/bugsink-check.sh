#!/usr/bin/env bash
# Check that Bugsink accepts, groups and keeps the event shape pcb sends as a
# crash report (spec/designs/2026-11-19-crash-reports-design.md, 5.4): runs a
# throwaway bugsink/bugsink container on a random loopback port, creates a
# project, posts testdata/report/event.json as an envelope twice (another
# event_id, same fingerprint) and once more with another fingerprint, then
# reads the result back with bugsink-manage. Prints PASS or the first
# mismatch (exit 1). Not part of the gate (`make bugsink-check`).
#
#   BUGSINK_IMAGE=bugsink/bugsink:2.6.3   image to test
#
# Needs bash, docker, curl, jq, python3 (to build the throwaway secret).
set -euo pipefail

REPO=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
EVENT="$REPO/testdata/report/event.json"
IMAGE=${BUGSINK_IMAGE:-bugsink/bugsink:2.6.3}
NAME="pcb-bugsink-check-$$"

cleanup() { docker rm -f "$NAME" >/dev/null 2>&1 || true; }
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

# Throwaway values: nothing here is a real secret and nothing outlives the container.
SECRET=$(python3 -c 'import secrets; print(secrets.token_urlsafe(64))')
ADMIN_PASSWORD=$(python3 -c 'import secrets; print(secrets.token_urlsafe(16))')

docker run -d --name "$NAME" -p 127.0.0.1::8000 \
	-e SECRET_KEY="$SECRET" \
	-e CREATE_SUPERUSER="check@example.test:$ADMIN_PASSWORD" \
	-e PHONEHOME=False \
	"$IMAGE" >/dev/null
PORT=$(docker port "$NAME" 8000/tcp | head -n1 | sed 's/.*://')
BASE="http://127.0.0.1:$PORT"

echo "waiting for Bugsink on $BASE"
ready=
for _ in $(seq 1 90); do
	if curl -fsS -o /dev/null "$BASE/health/ready" 2>/dev/null; then ready=1; break; fi
	[ "$(docker inspect -f '{{.State.Running}}' "$NAME")" = true ] || { docker logs "$NAME" >&2; fail "container stopped"; }
	sleep 1
done
[ -n "$ready" ] || { docker logs "$NAME" >&2; fail "Bugsink not ready after 90 s"; }

# Create a project and read its DSN (the key and id) through Django.
DSN=$(docker exec "$NAME" bugsink-manage shell -c \
	"from projects.models import Project; print('DSN=' + Project.objects.create(name='check').dsn)" | sed -n 's/^DSN=//p')
[ -n "$DSN" ] || fail "no DSN"
KEY=${DSN#http://}; KEY=${KEY%%@*}
PROJECT=${DSN##*/}

post() { # post <event_id> <fingerprint-json>
	local body
	body=$(jq -c --arg id "$1" --argjson fp "$2" '.event_id = $id | .fingerprint = $fp' "$EVENT")
	printf '{"event_id":"%s"}\n{"type":"event"}\n%s\n' "$1" "$body" |
		curl -fsS -o /dev/null -X POST \
			-H "X-Sentry-Auth: Sentry sentry_version=7, sentry_key=$KEY, sentry_client=pcb" \
			--data-binary @- "$BASE/api/$PROJECT/envelope/" || fail "envelope $1 refused"
}

FP_A=$(jq -c .fingerprint "$EVENT")
FP_B='["go.error","*errors.errorString","otherFunc"]'
post 00000000000040008000000000000001 "$FP_A"
post 00000000000040008000000000000002 "$FP_A"
post 00000000000040008000000000000003 "$FP_B"

# Ingestion is digested by a background worker: wait for the three events.
count=0
for _ in $(seq 1 30); do
	count=$(docker exec "$NAME" bugsink-manage shell -c \
		"from events.models import Event; print('N=%d' % Event.objects.count())" | sed -n 's/^N=//p')
	[ "$count" = 3 ] && break
	sleep 1
done
[ "$count" = 3 ] || fail "expected 3 events digested, got $count"

# Read back what Bugsink kept. Prints one "FAIL ..." line per mismatch, else "OK".
RESULT=$(docker exec "$NAME" bugsink-manage shell -c '
import json
from events.models import Event
from issues.models import Issue
bad = []
issues = list(Issue.objects.order_by("first_seen", "id"))
if len(issues) != 2:
    bad.append("expected 2 issues, got %d" % len(issues))
else:
    counts = sorted(Event.objects.filter(issue=i).count() for i in issues)
    if counts != [1, 2]:
        bad.append("expected issues with 1 and 2 events, got %s" % counts)
    first = Event.objects.get(event_id="00000000000040008000000000000001")
    second = Event.objects.get(event_id="00000000000040008000000000000002")
    if first.issue_id != second.issue_id:
        bad.append("same fingerprint did not group")
    if Event.objects.get(event_id="00000000000040008000000000000003").issue_id == first.issue_id:
        bad.append("another fingerprint was folded into the same issue")
    d = json.loads(first.data)
    if first.release != "pcb@1.2.3" or d.get("release") != "pcb@1.2.3":
        bad.append("release lost: %r" % first.release)
    want_tags = {"binary": "pcb", "command": "pcb serve", "install": "container", "source": "crash"}
    if d.get("tags") != want_tags:
        bad.append("tags lost: %r" % d.get("tags"))
    stored = {t.value.key.key: t.value.value for t in first.tags.all()}
    if any(stored.get(k) != v for k, v in want_tags.items()):
        bad.append("tags not indexed: %r" % stored)
    ex = d["exception"]["values"][0]
    frames = ex["stacktrace"]["frames"]
    if [f["in_app"] for f in frames] != [False, True, True]:
        bad.append("in_app lost: %r" % [f.get("in_app") for f in frames])
    if ex.get("mechanism", {}).get("handled") is not False:
        bad.append("handled:false lost: %r" % ex.get("mechanism"))
    if d.get("extra", {}).get("count") != 3:
        bad.append("extra.count lost: %r" % d.get("extra"))
    if d.get("fingerprint") != ["go.crash", "runtime error", "serveLoop", "handleRequest"]:
        bad.append("fingerprint lost: %r" % d.get("fingerprint"))
for b in bad:
    print("FAIL " + b)
print("OK" if not bad else "")
' | grep -E '^(FAIL |OK$)' || true)

if [ "$RESULT" != OK ]; then
	echo "$RESULT" >&2
	fail "Bugsink did not keep the event shape"
fi
echo PASS
