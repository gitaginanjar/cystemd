# Makefile for cystemd

BINARY_NAME := cystemd
INSTALL_DIR := ${PWD}
BIN_DIR := $(INSTALL_DIR)
CONFIG_FILE := $(INSTALL_DIR)/config.yml
ENV_FILE := $(INSTALL_DIR)/.env

# Go toolchain. Resolved from PATH by default; override with GO=<path>
# or export GO=<path> before invoking make.
GO ?= go

# Full 40-character Git commit hash, never the short hash.
# The --version output advertises the binary's provenance; the full hash
# makes it unambiguous which revision a customer is actually running.
GIT_COMMIT := $(shell git rev-parse HEAD 2>/dev/null || echo unknown)

# Build timestamp in 24-hour Asia/Jakarta time, ISO 8601 with an explicit
# numeric offset, for example: 2026-05-18T19:49:22+0700.
BUILD_TIME := $(shell TZ=Asia/Jakarta date +%Y-%m-%dT%H:%M:%S%z)

# Populate the version only when HEAD points exactly to a Git tag.
# If the current commit has no tag, the version is an empty string.
GIT_TAG := $(shell git describe --tags --exact-match HEAD 2>/dev/null || true)

.PHONY: all build install clean run

# Default target
all: build

# Build the Go binary
build:
	@echo "Building $(BINARY_NAME)..."
	mkdir -p $(BIN_DIR)
	$(GO) version
	$(GO) mod tidy
	rm -f $(BIN_DIR)/$(BINARY_NAME)
	CGO_ENABLED=0 $(GO) build -ldflags "-X cystemd/internal/service.version=$(GIT_TAG) -X cystemd/internal/service.commit=$(GIT_COMMIT) -X cystemd/internal/service.buildTime=$(BUILD_TIME)" -o $(BIN_DIR)/$(BINARY_NAME) ./main.go

# Install the binary and configuration into INSTALL_DIR
install: build
	@echo "Installing to $(INSTALL_DIR)..."
	mkdir -p $(BIN_DIR)
	cp $(BIN_DIR)/$(BINARY_NAME) $(BIN_DIR)/
	@echo "Binary installed at $(BIN_DIR)/$(BINARY_NAME)"
	@echo "Config expected at $(CONFIG_FILE)"
	@echo "Env file expected at $(ENV_FILE)"

# Clean build artifacts
clean:
	@echo "Cleaning build artifacts..."
	rm -f $(BIN_DIR)/$(BINARY_NAME)

# Run the API server directly
run:
	@echo "Running $(BINARY_NAME)..."
	$(BIN_DIR)/$(BINARY_NAME)