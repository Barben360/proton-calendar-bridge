# Debian with systemd as PID 1, for scripts/systemd-smoke.sh (M5-P21): the
# pcb binary is copied in at run time.

# debian:13
FROM debian:13@sha256:9c6e381421d0f48d1b8133e573f33c185ee16cbf505eda9c4a8a4791b25ea8a6
# systemd reads this to know it runs in a container.
ENV container=docker
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      systemd systemd-sysv dbus-user-session curl jq passwd ca-certificates \
 && rm -rf /var/lib/apt/lists/*
STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
