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

.PHONY: build install test lint

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(OUT) ./cmd/pcb

install: build
	install -D -m 0755 $(OUT) $(DESTDIR)$(PREFIX)/bin/pcb

# PKGS narrows the packages (CI runs two shards in parallel).
PKGS ?= ./...

test:
	go test -race $(PKGS)

# Formatting, vet, then golangci-lint, as CI runs it.
lint:
	@files=$$(gofmt -l .); \
		if [ -n "$$files" ]; then echo "gofmt: run gofmt -w on:"; echo "$$files"; exit 1; fi
	go vet ./...
	golangci-lint run --allow-parallel-runners
