PKG     := github.com/Barben360/proton-calendar-bridge
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
# Where approved crash reports go (a Sentry DSN). Empty, the build has no
# address and keeps and sends no report: dev and source builds, and make
# test and make lint, which never stamp it. Only make build, ui and image
# take it, from the release workflows' secret PCB_REPORT_DSN.
REPORT_DSN ?=
LDFLAGS := -s -w \
	-X $(PKG)/internal/buildinfo.Version=$(VERSION) \
	-X $(PKG)/internal/buildinfo.Commit=$(COMMIT) \
	-X $(PKG)/internal/buildinfo.Date=$(DATE) \
	-X $(PKG)/internal/buildinfo.ReportDSN=$(REPORT_DSN)
# GOOS=windows builds append .exe (make build ui GOOS=windows; pcb-ui only on
# Windows itself: it needs cgo).
GOOS    ?= $(shell go env GOOS)
EXE     := $(if $(filter windows,$(GOOS)),.exe)
OUT     ?= bin/pcb$(EXE)
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
# runners have 2). At 4 locally the suite takes about 250 s
# against 200 s uncapped, for half the CPU time (contention under -race
# doubles the work).
JOBS    ?= $(shell n=$$(nproc 2>/dev/null || echo 4); [ "$$n" -lt 4 ] && echo "$$n" || echo 4)
export GOMAXPROCS := $(JOBS)

.PHONY: build install ui winres msi install-ui test lint image container-smoke systemd-smoke live-preflight live-preflight-test release-test install-test vmtest-test shellcheck bugsink-check actionlint pcb-fake rehearse screenshots

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(OUT) ./cmd/pcb

install: build
	install -D -m 0755 $(OUT) $(DESTDIR)$(PREFIX)/bin/pcb$(EXE)

# pcb-ui, the desktop app (M7): cgo and the GL/X development headers
# (libgl1-mesa-dev libwayland-dev libxkbcommon-dev xorg-dev on Debian), so it is not part of make build.
# On Windows (mingw-w64 gcc): a GUI binary (-H=windowsgui, no console
# window) with its icon and version resource (make winres).
UI_OUT ?= bin/pcb-ui$(EXE)

ui: winres
	CGO_ENABLED=1 go build -trimpath -ldflags "$(LDFLAGS)$(if $(filter windows,$(GOOS)), -H=windowsgui)" -o $(UI_OUT) ./cmd/pcb-ui

# The Windows resource of pcb-ui.exe: the icon (internal/ui/desktop/icons/app.svg
# rendered at 256 px by cmd/pcb-ui/winres) and the version, written by the
# pinned go-winres to cmd/pcb-ui/rsrc_windows_amd64.syso (gitignored), which
# go build links. Windows has four numbers: v1.2.3-4-gabc is 1.2.3.0, a
# version that is not a tag is 0.0.0.0. A no-op on other systems.
WINVER := $(or $(shell echo '$(VERSION)' | sed -nE 's/^v?([0-9]+)\.([0-9]+)\.([0-9]+).*/\1.\2.\3.0/p'),0.0.0.0)

winres:
ifeq ($(GOOS),windows)
	mkdir -p bin
	env -u GOOS -u GOARCH go run ./cmd/pcb-ui/winres -o bin/pcb-ui-256.png
	env -u GOOS -u GOARCH go run github.com/tc-hib/go-winres@v0.3.3 simply --arch amd64 --out cmd/pcb-ui/rsrc \
		--icon bin/pcb-ui-256.png --manifest gui --product-name pcb-ui --file-description "pcb desktop app" \
		--original-filename pcb-ui.exe --product-version $(WINVER) --file-version $(WINVER)
endif

# The Windows installer, bin/pcb_windows_amd64.msi, from bin/pcb.exe and
# bin/pcb-ui.exe (make build ui first), on Windows only: WiX 7.0.1
# (scripts/release/mkmsi.ps1 says how to install it), PowerShell, bash (Git
# for Windows' or MSYS2's). Its version is the tag at HEAD (v1.2.3-rc.1 is
# 1.2.3), else 0.0.0; MSI_TAG=v1.2.3 sets it. ICE checks included.
MSI_TAG ?= $(shell git describe --tags --exact-match HEAD 2>/dev/null)

msi:
ifeq ($(GOOS),windows)
	powershell -NoProfile -ExecutionPolicy Bypass -File scripts/release/mkmsi.ps1 \
		-Version "$$(bash scripts/release/check.sh msi-version '$(MSI_TAG)')" \
		-Pcb bin/pcb.exe -PcbUi bin/pcb-ui.exe -Out bin/pcb_windows_amd64.msi -Validate
else
	@echo "make msi: Windows only (WiX)" >&2; exit 2
endif

# The icon is the window's app icon, installed as the desktop entry's Icon.
install-ui: ui
	install -D -m 0755 $(UI_OUT) $(DESTDIR)$(PREFIX)/bin/pcb-ui
	# The absolute Exec: launchers often lack $(PREFIX)/bin on their PATH.
	install -d $(DESTDIR)$(PREFIX)/share/applications
	sed 's|^Exec=pcb-ui|Exec=$(PREFIX)/bin/pcb-ui|' packaging/pcb-ui.desktop > $(DESTDIR)$(PREFIX)/share/applications/pcb-ui.desktop
	chmod 0644 $(DESTDIR)$(PREFIX)/share/applications/pcb-ui.desktop
	install -D -m 0644 internal/ui/desktop/icons/app.svg $(DESTDIR)$(PREFIX)/share/icons/hicolor/scalable/apps/pcb-ui.svg

# The fake Proton for rehearsals (plan R2): developer only, never in the
# image nor in make install.
FAKE_OUT ?= bin/pcb-fake

pcb-fake:
	CGO_ENABLED=0 go build -trimpath -o $(FAKE_OUT) ./cmd/pcb-fake

# PKGS narrows the packages (CI runs three shards in parallel).
PKGS ?= ./...
# The service and cli packages run real SRP logins under -race: on a small
# CI runner they need more than go test's 10 min default.
TEST_TIMEOUT ?= 20m

# The tests never see the caller's session bus: one that forgot its
# package's guard cannot reach the developer's desktop keyring, and the bus
# tests start their own private bus (internal/dbustest), so no
# dbus-run-session is needed (or wanted: it activates the system's
# gnome-keyring-daemon on the caller's home).
test:
	env -u DBUS_SESSION_BUS_ADDRESS go test -race -timeout $(TEST_TIMEOUT) $(PKGS)

# Formatting, vet, then golangci-lint, as CI runs it. golangci-lint sizes
# its workers by the machine's cores, not by GOMAXPROCS.
lint:
	@files=$$(gofmt -l .); \
		if [ -n "$$files" ]; then echo "gofmt: run gofmt -w on:"; echo "$$files"; exit 1; fi
	go vet ./...
	golangci-lint run --concurrency $(JOBS) --allow-parallel-runners

# The container image (M5-Rpre7), stamped like make build. IMAGE_FLAGS adds
# docker build flags (.github/workflows/image-build.yml pushes by digest).
IMAGE ?= pcb:local
IMAGE_FLAGS ?=

image:
	docker build -t $(IMAGE) --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg DATE=$(DATE) $(if $(REPORT_DSN),--build-arg REPORT_DSN) $(IMAGE_FLAGS) .

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

vmtest-test:
	bash scripts/win/vmtest_test.sh

# Does Bugsink accept, group and keep the crash report shape (not in the gate;
# needs docker): scripts/bugsink-check.sh.
bugsink-check:
	scripts/bugsink-check.sh

# shellcheck of the live-session scripts, pinned by digest so CI and a
# workstation run the same one (the runner's own v0.11.0 crashes on
# preflight.sh: "Non-exhaustive patterns in function checkCmd").
SHELLCHECK_IMAGE ?= koalaman/shellcheck:v0.10.0@sha256:2097951f02e735b613f4a34de20c40f937a6c8f18ecb170612c88c34517221fb
shellcheck:
	docker run --rm --network none -v "$(CURDIR):/m:ro" -w /m/scripts/live $(SHELLCHECK_IMAGE) \
		-x -P SCRIPTDIR common.sh guard.sh run.sh preflight.sh guard_test.sh rehearse.sh
	docker run --rm --network none -v "$(CURDIR):/m:ro" -w /m $(SHELLCHECK_IMAGE) -x \
		scripts/install.sh scripts/install_test.sh \
		scripts/release/check.sh scripts/release/check_test.sh scripts/release/macos-smoke.sh \
		scripts/release/package.sh scripts/release/package_test.sh scripts/release/licensertf_test.sh \
		scripts/win/vmtest.sh scripts/win/vmtest_test.sh scripts/bugsink-check.sh

# The workflows, linted by actionlint (v1.7.12, 2026-03-30), pinned by digest.
ACTIONLINT_IMAGE ?= rhysd/actionlint:1.7.12@sha256:b1934ee5f1c509618f2508e6eb47ee0d3520686341fec936f3b79331f9315667
actionlint:
	docker run --rm --network none -v "$(CURDIR):/m:ro" -w /m $(ACTIONLINT_IMAGE) -color

# The release scripts (scripts/release): the tag rules, the archives and
# the MSI's licence RTF (a drift from LICENSE fails); each prints PASS last.
release-test:
	bash scripts/release/check_test.sh
	bash scripts/release/package_test.sh
	bash scripts/release/licensertf_test.sh

# install.sh against a fake release of stub binaries (POSIX sh, as the
# curl | sh users run it); prints PASS last.
install-test:
	sh scripts/install_test.sh

# The live session rehearsed offline against pcb-fake (plan R2): builds pcb
# and pcb-fake into a temporary run root and prints PASS last.
rehearse:
	scripts/live/rehearse.sh

# The README's pcb-ui pictures, light and dark, rendered by Fyne's test driver
# on made-up accounts (internal/ui/desktop/screenshots_test.go).
screenshots:
	PCB_SCREENSHOTS=$(CURDIR)/docs/assets/ui go test ./internal/ui/desktop -run '^TestScreenshots$$' -count=1
