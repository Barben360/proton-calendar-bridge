#!/usr/bin/env bash
# Container smoke test (M5-Rpre7, M5-Rpre12): the image comes up with no
# login, healthy, waiting for one; CalDAV answers 401 without credentials and
# 503 with them; it runs as 65532 and holds no secrets; and it comes back
# healthy after a SIGKILL. No Proton call is made: nobody logs in.
#
# Needs docker, curl and jq. Builds a throwaway image (make image), or tests
# PCB_SMOKE_IMAGE when set (CI tests the image it pushes; it is kept). The
# container, its volume and a throwaway image are removed on any exit. The
# last line is PASS.
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
for tool in docker curl jq; do
	command -v "$tool" >/dev/null || { echo "container-smoke: $tool is required" >&2; exit 1; }
done

id="pcb-smoke-$$-$(date +%s)"
container=$id
volume=$id
port=18443
url="https://127.0.0.1:$port/dav/calendars/me/"
image=${PCB_SMOKE_IMAGE:-}
built=""

cleanup() {
	local status=$?
	if [ "$status" -ne 0 ] && docker container inspect "$container" >/dev/null 2>&1; then
		echo "--- last container logs ---" >&2
		docker logs --tail 40 "$container" >&2 2>&1 || true
	fi
	docker rm -f "$container" >/dev/null 2>&1 || true
	docker volume rm -f "$volume" >/dev/null 2>&1 || true
	if [ -n "$built" ]; then docker image rm -f "$built" >/dev/null 2>&1 || true; fi
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

fail() {
	echo "FAIL: $*" >&2
	exit 1
}
step() { echo "--- $*"; }

# started_ns is when the container last started, in nanoseconds.
started_ns() {
	date -d "$(docker container inspect -f '{{.State.StartedAt}}' "$container")" +%s%N
}

# wait_healthy waits up to 60 s for a healthy verdict from a check run after
# the container's last start (a verdict from before a restart does not count).
wait_healthy() {
	local since state status last deadline=$((SECONDS + 60))
	since=$(started_ns)
	while :; do
		state=$(docker container inspect -f '{{json .State}}' "$container")
		if [ "$(jq -r .Running <<<"$state")" != true ]; then
			fail "the container stopped (exit code $(jq -r .ExitCode <<<"$state")) instead of becoming healthy"
		fi
		status=$(jq -r '.Health.Status // "none"' <<<"$state")
		if [ "$status" = healthy ]; then
			last=$(jq -r '.Health.Log[-1].Start' <<<"$state")
			if [ "$(date -d "$last" +%s%N)" -gt "$since" ]; then
				return 0
			fi
		fi
		if [ "$SECONDS" -ge "$deadline" ]; then
			fail "not healthy within 60 s (health: $status; last check: $(jq -c '.Health.Log[-1] // {}' <<<"$state"))"
		fi
		sleep 1
	done
}

step "1. build"
if [ -z "$image" ]; then
	built="pcb:$id"
	make -C "$root" --no-print-directory image IMAGE="$built"
	image=$built
fi

step "2. run $image with a fresh volume on 127.0.0.1:$port"
docker volume create "$volume" >/dev/null
docker run -d --name "$container" -v "$volume:/data" -p "127.0.0.1:$port:1080" "$image" >/dev/null

step "3. healthy within 60 s"
wait_healthy

step "4. pcb status --json --daemon: logged out, needs-login, a stamped version"
st=$(docker exec "$container" pcb status --json --daemon) || fail "pcb status --json --daemon failed: $st"
jq -e '.ok == true
	and .data.phase == "logged-out"
	and any(.data.alerts[]; .code == "needs-login")
	and (.data.daemon.version | type == "string" and . != "" and . != "dev")' <<<"$st" >/dev/null ||
	fail "unexpected status: $st"

step "5. pcb client add smoke --json gives a password"
# The password stays in this shell: never printed, never in an argv.
pw=$(docker exec "$container" pcb client add smoke --json | jq -r '.data.password // empty')
[ -n "$pw" ] || fail "pcb client add smoke --json gave no password"

step "6. CalDAV over TLS: 503 Retry-After: 30 with a client, 401 without"
# PROPFIND, as Thunderbird asks: a GET on a collection is 405 in any state.
got=$(printf 'user = "smoke:%s"\n' "$pw" |
	curl -sk -K - -X PROPFIND -H 'Depth: 0' -o /dev/null -w '%{http_code} %header{retry-after}' "$url") || true
[ "$got" = "503 30" ] || fail "with a client: want '503 30' (status, Retry-After), got '$got'"
got=$(curl -sk -X PROPFIND -H 'Depth: 0' -o /dev/null -w '%{http_code}' "$url") || true
[ "$got" = 401 ] || fail "without credentials: want 401, got '$got'"

step "7. the image runs as 65532"
user=$(docker image inspect -f '{{.Config.User}}' "$image")
[ "$user" = 65532 ] || fail "Config.User is '$user', want 65532"

step "8. /data: config and state 0700 65532, no secrets.sealed"
listing=$(docker cp "$container:/data" - | tar --numeric-owner -tvf -)
for d in data/config/ data/state/; do
	grep -Eq "^drwx------ 65532/65532 .* $d\$" <<<"$listing" || fail "$d is not 0700 65532/65532 in: $listing"
done
grep -q ' data/state/clients.json$' <<<"$listing" || fail "the listing misses the client just added: $listing"
if grep -q 'secrets.sealed' <<<"$listing"; then fail "a secrets.sealed exists: $listing"; fi

step "9. docker kill, docker start: healthy again within 60 s"
docker kill "$container" >/dev/null
[ "$(docker container inspect -f '{{.State.Running}}' "$container")" = false ] || fail "the container still runs after docker kill"
docker start "$container" >/dev/null
wait_healthy

echo PASS
