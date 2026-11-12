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
# Every target runs go on JOBS CPUs (make test JOBS=8 for more). Uncapped,
# go test -race ./... runs a package per core, each binary on every core
# with up to that many parallel tests: on a 24-core machine the load passed
# 130 and logins starved past their 30 s deadline (spurious failures).
# GOMAXPROCS sets go's -p (packages at once), -parallel (tests at once per
# package) and the threads of each test binary and child. JOBS is 4, or the
# machine's CPUs when fewer: more than the CPUs starves the same way (CI's
# private-repo runners have 2). At 4 locally the suite takes about 250 s
# against 200 s uncapped, for half the CPU time (contention under -race
# doubles the work).
JOBS    ?= $(shell n=$$(nproc 2>/dev/null || echo 4); [ "$$n" -lt 4 ] && echo "$$n" || echo 4)
export GOMAXPROCS := $(JOBS)

.PHONY: build install test lint image container-smoke systemd-smoke

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

# Formatting, vet, then golangci-lint, as CI runs it. golangci-lint sizes
# its workers by the machine's cores, not by GOMAXPROCS.
lint:
	@files=$$(gofmt -l .); \
		if [ -n "$$files" ]; then echo "gofmt: run gofmt -w on:"; echo "$$files"; exit 1; fi
	go vet ./...
	golangci-lint run --concurrency $(JOBS) --allow-parallel-runners

# The container image (M5-Rpre7), stamped like make build.
IMAGE ?= pcb:local

image:
	docker build -t $(IMAGE) --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg DATE=$(DATE) .

# Smoke tests in throwaway containers (M5-Rpre12): each prints PASS last.
container-smoke:
	scripts/container-smoke.sh

systemd-smoke: build
	scripts/systemd-smoke.sh $(OUT)
