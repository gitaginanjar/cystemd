# Makefile for CYSTEMD.
#
#   make              cystemd-server (standalone control plane + web UI)
#                     and cystemdctl (the command line client)
#   make agent        the cystemd agent: the Nomad fork with the control
#                     plane, web UI and systemd driver built in
#   make driver       the systemd driver as an external Nomad plugin
#   make test         unit tests (no agents needed)
#   make generate     regenerate the templ views (ui/views/*_templ.go)
#
# See docs/FORK.md for how the Nomad fork is assembled.

GO ?= go

# Build metadata (the v4 contract): the version is the exact Git tag of
# HEAD (empty when untagged), the commit is the full 40-character hash and
# the build time is ISO 8601 in Asia/Jakarta time with an explicit offset.
GIT_COMMIT := $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
GIT_TAG    := $(shell git describe --tags --exact-match HEAD 2>/dev/null || true)
BUILD_TIME := $(shell TZ=Asia/Jakarta date +%Y-%m-%dT%H:%M:%S%z)

BUILDINFO := github.com/gitaginanjar/cystemd/internal/buildinfo
LDFLAGS := -s -w \
	-X $(BUILDINFO).version=$(GIT_TAG) \
	-X $(BUILDINFO).commit=$(GIT_COMMIT) \
	-X $(BUILDINFO).buildTime=$(BUILD_TIME)
AGENT_VERSION := $(if $(GIT_TAG),$(GIT_TAG),dev)
AGENT_LDFLAGS := $(LDFLAGS) \
	-X main.cystemdVersion=$(AGENT_VERSION) \
	-X main.cystemdCommit=$(shell echo $(GIT_COMMIT) | cut -c1-12)

TEMPL_VERSION := v0.3.1020
BIN := bin

.PHONY: all build server ctl agent fork driver generate check-generate vendor-htmx \
	test test-race test-integration lint fmt clean help

all: server ctl

build: server ctl

## server: build bin/cystemd-server
server:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/cystemd-server ./cmd/cystemd-server

## ctl: build bin/cystemdctl, the command line client (also in the agent: cystemd app ...)
ctl:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/cystemdctl ./cmd/cystemdctl

## fork: assemble the Nomad fork into build/nomad (pinned upstream + overlay + patches)
fork:
	scripts/fork-assemble.sh

## agent: build bin/cystemd, the Nomad fork with CYSTEMD built in
agent: fork
	cd agent && CGO_ENABLED=1 $(GO) build -trimpath -tags release -ldflags "$(AGENT_LDFLAGS)" -o ../$(BIN)/cystemd .

## driver: build the systemd driver as an external plugin
driver: fork
	cd driver && CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w" -o ../$(BIN)/cystemd-driver-systemd ./cmd/cystemd-driver-systemd

## generate: regenerate the templ views
generate:
	$(GO) run github.com/a-h/templ/cmd/templ@$(TEMPL_VERSION) generate -path ui/views

## check-generate: fail when the generated views are out of date
check-generate: generate
	git diff --exit-code -- ui/views

## vendor-htmx: re-vendor htmx (versions and checksums in the script)
vendor-htmx:
	scripts/vendor-htmx.sh

## test: unit tests of every module that needs no running agents
test:
	$(GO) test -short ./...
	cd driver && $(GO) test ./...

## test-race: unit tests with the race detector
test-race:
	$(GO) test -short -race ./...

## test-integration: tests against real Nomad, Consul and Vault binaries on PATH
test-integration:
	$(GO) test -run 'Integration|EndToEnd|Real' -count=1 ./...

## lint: formatting and vet
lint:
	@test -z "$$(gofmt -l cmd internal pkg ui agent driver)" || { gofmt -l cmd internal pkg ui agent driver; exit 1; }
	$(GO) vet ./...

## fmt: format the Go sources
fmt:
	gofmt -w cmd internal pkg ui agent driver

## clean: remove build outputs (keeps the downloaded Go modules)
clean:
	rm -rf $(BIN) build

## help: list the targets
help:
	@sed -n 's/^## //p' $(MAKEFILE_LIST)
