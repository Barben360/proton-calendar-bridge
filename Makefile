PKG     := github.com/Barben360/proton-calendar-bridge
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w \
	-X $(PKG)/internal/buildinfo.Version=$(VERSION) \
	-X $(PKG)/internal/buildinfo.Commit=$(COMMIT) \
	-X $(PKG)/internal/buildinfo.Date=$(DATE)
OUT     ?= bin/pcb
# make install puts pcb in $(PREFIX)/bin (M5-P23); pcb install then sets up
# the service.
PREFIX  ?= $(HOME)/.local

.PHONY: build install test lint image container-smoke systemd-smoke live-preflight live-preflight-test

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(OUT) ./cmd/pcb

install: build
	install -D -m 0755 $(OUT) $(DESTDIR)$(PREFIX)/bin/pcb

# PKGS narrows the packages (CI runs three shards in parallel).
PKGS ?= ./...
# The service and cli packages run real SRP logins under -race: on a small
# CI runner they need more than go test's 10 min default.
TEST_TIMEOUT ?= 20m

test:
	go test -race -timeout $(TEST_TIMEOUT) $(PKGS)

# Formatting, vet, then golangci-lint, as CI runs it.
lint:
	@files=$$(gofmt -l .); \
		if [ -n "$$files" ]; then echo "gofmt: run gofmt -w on:"; echo "$$files"; exit 1; fi
	go vet ./...
	golangci-lint run --allow-parallel-runners

# The container image (M5-Rpre7), stamped like make build.
IMAGE ?= pcb:local

image:
	docker build -t $(IMAGE) --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg DATE=$(DATE) .

# Smoke tests in throwaway containers (M5-Rpre12): each prints PASS last.
container-smoke:
	scripts/container-smoke.sh

systemd-smoke: build
	scripts/systemd-smoke.sh $(OUT)

# The live session (spec/HUMAN-CHECKS.md): preflight of a run root, and the
# test of the guard that keeps live commands off the author's real dirs.
# make live-preflight RUN=$$HOME/pcb-live-run/<date> [STEP7=1]
live-preflight:
	@test -n "$(RUN)" || { echo "usage: make live-preflight RUN=\$$HOME/pcb-live-run/<date> [STEP7=1]" >&2; exit 2; }
	scripts/live/preflight.sh "$(RUN)" $(if $(STEP7),--step7)

live-preflight-test:
	scripts/live/guard_test.sh
