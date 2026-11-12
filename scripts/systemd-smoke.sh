#!/usr/bin/env bash
# systemd smoke test (M5-Rpre12, M5-P21): pcb install --user and --system in
# a Debian container with systemd as PID 1, each unit up and waiting for a
# login, then uninstalled. No Proton call is made: nobody logs in.
#
# Usage: scripts/systemd-smoke.sh [PCB_BINARY] (default bin/pcb; make
# systemd-smoke builds it). Needs docker and a privileged container. The
# container and its image are removed on any exit. The last line is PASS.
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
bin=${1:-$root/bin/pcb}
command -v docker >/dev/null || { echo "systemd-smoke: docker is required" >&2; exit 1; }
[ -x "$bin" ] || { echo "systemd-smoke: no pcb binary at $bin (run make build)" >&2; exit 1; }

id="pcb-systemd-smoke-$$-$(date +%s)"
container=$id
image="pcb-systemd-smoke:$id"
pcb=/opt/pcb/bin/pcb           # the binary under test; --user runs it
system_pcb=/usr/local/bin/pcb  # where --system copies it (M5-P16)
user_unit=/home/tester/.config/systemd/user/pcb.service
system_unit=/etc/systemd/system/pcb.service
uid=""

cleanup() {
	local status=$?
	if [ "$status" -ne 0 ] && docker container inspect "$container" >/dev/null 2>&1; then
		echo "--- last journal lines ---" >&2
		docker exec "$container" journalctl --no-pager -n 60 >&2 2>&1 || true
	fi
	docker rm -f "$container" >/dev/null 2>&1 || true
	docker image rm -f "$image" >/dev/null 2>&1 || true
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
in_box() { docker exec "$container" "$@"; }
as_tester() {
	docker exec "$container" runuser -u tester -- \
		env HOME=/home/tester XDG_RUNTIME_DIR="/run/user/$uid" "$@"
}
absent() { if in_box test -e "$1"; then fail "$1 exists: $2"; fi; }

# verify runs a systemd-analyze verify command line: it passes when it
# exits 0 and warns about nothing (an unknown directive only warns).
verify() {
	local out
	out=$("$@" 2>&1) || fail "$*: $out"
	[ -z "$out" ] || fail "$* warned: $out"
}

# wait_status runs a pcb status --json --daemon command line until a daemon
# answers (up to 30 s), then checks it needs a login.
wait_status() {
	local out deadline=$((SECONDS + 30))
	until out=$("$@" 2>/dev/null); do
		[ "$SECONDS" -lt "$deadline" ] || fail "no daemon answered within 30 s: $out"
		sleep 1
	done
	jq -e '.ok == true and any(.data.alerts[]; .code == "needs-login")' <<<"$out" >/dev/null ||
		fail "unexpected status: $out"
}

step "build the Debian systemd image and start it"
docker build -q -t "$image" "$root/testdata/systemd" >/dev/null
docker run -d --name "$container" --privileged --cgroupns=host \
	--tmpfs /run --tmpfs /run/lock -v /sys/fs/cgroup:/sys/fs/cgroup:rw \
	"$image" /sbin/init >/dev/null
# is-system-running --wait answers once the boot is done; until the system
# bus is up it answers nothing.
deadline=$((SECONDS + 120))
while :; do
	state=$(timeout 120 docker exec "$container" systemctl is-system-running --wait 2>/dev/null) || true
	case $state in
	running) break ;;
	degraded)
		echo "systemd is degraded:"
		in_box systemctl --failed --no-pager || true
		break
		;;
	"") [ "$SECONDS" -lt "$deadline" ] || fail "systemd did not come up within 120 s" ;;
	*) fail "systemd did not come up: '$state'" ;;
	esac
	[ "$(docker container inspect -f '{{.State.Running}}' "$container")" = true ] || fail "the container stopped: $(docker logs --tail 20 "$container" 2>&1)"
	sleep 1
done
in_box install -d -m 0755 /opt/pcb/bin
docker cp "$bin" "$container:$pcb"
in_box chmod 0755 "$pcb"
in_box useradd --create-home --shell /bin/bash tester
uid=$(in_box id -u tester)
in_box loginctl enable-linger tester
in_box systemctl start "user@$uid.service"

echo "== as tester: pcb install --user"

step "u1. install --user --plan leaves no unit"
plan=$(as_tester "$pcb" install --user --plan --json)
jq -e --arg unit "$user_unit" '.data.plan == true and any(.data.steps[]; .path == $unit and .action == "create")' <<<"$plan" >/dev/null ||
	fail "the plan does not create $user_unit: $plan"
absent "$user_unit" "after --plan"

step "u2. install --user"
as_tester "$pcb" install --user

step "u3. enabled and active"
got=$(as_tester systemctl --user is-enabled pcb) || true
[ "$got" = enabled ] || fail "systemctl --user is-enabled pcb: '$got'"
got=$(as_tester systemctl --user is-active pcb) || true
[ "$got" = active ] || fail "systemctl --user is-active pcb: '$got'"

step "u4. pcb service status --json: active"
out=$(as_tester "$pcb" service status --json)
jq -e '.data.active_state == "active"' <<<"$out" >/dev/null || fail "unexpected service status: $out"

step "u5. pcb status --json --daemon: needs-login"
wait_status as_tester "$pcb" status --json --daemon

step "u6. CalDAV with a client: 503"
pw=$(as_tester "$pcb" client add smoke --json | jq -r '.data.password // empty')
[ -n "$pw" ] || fail "pcb client add smoke --json gave no password"
# The password goes through stdin, never an argv.
got=$(printf 'user = "smoke:%s"\n' "$pw" | docker exec -i "$container" \
	curl -s -K - -X PROPFIND -H 'Depth: 0' -o /dev/null -w '%{http_code}' http://127.0.0.1:1080/dav/calendars/me/) || true
[ "$got" = 503 ] || fail "want 503 with a client, got '$got'"

step "u7. systemd-analyze --user verify"
verify as_tester systemd-analyze --user verify "$user_unit"

step "u8. a second install --user keeps every file"
again=$(as_tester "$pcb" install --user --json)
jq -e '[.data.steps[] | select(.kind == "file")] | length > 0 and all(.action == "keep")' <<<"$again" >/dev/null ||
	fail "the second install changed a file: $again"

step "u9. uninstall --user"
as_tester "$pcb" uninstall --user
absent "$user_unit" "after uninstall --user"
got=$(as_tester systemctl --user is-active pcb) || true
[ "$got" != active ] || fail "pcb still runs after uninstall --user"

echo "== as root: pcb install --system"

step "s1. install --system --plan leaves no unit"
in_box "$pcb" install --system --plan >/dev/null
absent "$system_unit" "after --plan"

step "s2. install --system"
in_box "$pcb" install --system

step "s3. active"
got=$(in_box systemctl is-active pcb) || true
[ "$got" = active ] || fail "systemctl is-active pcb: '$got'"

step "s4. pcb status --json --daemon as pcb: needs-login"
wait_status in_box runuser -u pcb -- env PCB_CONFIG_DIR=/etc/pcb PCB_STATE_DIR=/var/lib/pcb "$system_pcb" status --json --daemon

step "s5. systemd-analyze verify, then security (printed, not asserted)"
verify in_box systemd-analyze verify "$system_unit"
in_box systemd-analyze security --no-pager pcb.service || true

step "s6. uninstall --system --delete-data"
in_box "$pcb" uninstall --system --delete-data --yes --non-interactive
absent "$system_unit" "after uninstall --system"
absent /var/lib/pcb "after uninstall --system --delete-data"
if in_box id pcb >/dev/null 2>&1; then fail "the pcb user still exists after uninstall --system --delete-data"; fi

echo PASS
