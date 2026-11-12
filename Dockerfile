# pcb as a container (M5-Rpre7, M5-P20): a pure-Go static binary on
# distroless, non-root, all its files in the /data volume. Log in with
# docker exec -it <container> pcb login; until then CalDAV answers 503.

# golang:1.27-trixie
FROM golang:1.27-trixie@sha256:c47c3fda018d13d43a6bf5ebac3e5bba8a23a35782aa4faf531afaf47c64c39f AS build
ENV CGO_ENABLED=0
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=dev
ARG DATE=unknown
RUN make build OUT=/out/pcb VERSION="$VERSION" COMMIT="$COMMIT" DATE="$DATE"
# The volume's layout: a fresh named volume copies it, owner and modes too.
RUN install -d -m 0700 -o 65532 -g 65532 /out/data /out/data/config /out/data/state

# gcr.io/distroless/static-debian13:nonroot
FROM gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329
LABEL org.opencontainers.image.source="https://github.com/Barben360/proton-calendar-bridge" \
      org.opencontainers.image.description="pcb, the Proton Calendar CalDAV bridge" \
      org.opencontainers.image.licenses="MIT"
COPY --from=build /out/pcb /usr/local/bin/pcb
COPY --from=build --chown=65532:65532 /out/data /data
ENV PCB_CONFIG_DIR=/data/config \
    PCB_STATE_DIR=/data/state
USER 65532
VOLUME /data
# Any non-loopback bind serves TLS: a self-signed certificate kept in
# /data/state unless [caldav] tls_cert and tls_key are set.
EXPOSE 1080
# pcb status --daemon exits 20 when no daemon answers on its socket (M5-P5).
HEALTHCHECK --interval=30s --timeout=10s --start-period=20s --retries=3 CMD ["pcb", "status", "--json", "--daemon"]
ENTRYPOINT ["pcb"]
CMD ["serve", "--wait-for-login", "--listen", "0.0.0.0:1080"]
