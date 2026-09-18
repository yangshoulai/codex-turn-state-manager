SHELL := /bin/bash

MODULE      := github.com/yangshoulai/codex-turn-state-manager
BIN_DIR     := dist
PLUGIN_NAME := codex-turn-state-manager

# CPA loads plugins as C-ABI shared libraries, so CGO is mandatory (see
# docs: "插件技术选型"). Cross-compiling with GOOS alone is NOT supported for
# c-shared output -- build on the target OS/arch.
export CGO_ENABLED := 1

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
# The host rejects a plugin version beginning with "v"
# (pluginhost.validPluginVersion), and the store builds the artifact name from
# the bare version, so a release tag's prefix is stripped for the build. Without
# this, tagging v0.1.0 would register the plugin as "v0.1.0" and be refused.
#
# override, not a plain assignment: a command-line VERSION wins over the
# makefile, which would let `make ... VERSION=v0.1.0` through with the prefix
# intact and name the archive differently from the registered version.
override VERSION := $(patsubst v%,%,$(VERSION))
LDFLAGS := -X $(MODULE)/internal/version.Version=$(VERSION)

# Windows only: Go hands the external linker a generated .def file, and the
# binutils ld shipped on the Windows runners (2.46) fails to parse it whenever
# the output name embeds the version -- "name-v0.1.0.dll" gives
# "export_file.def:1: syntax error", while "name.dll" links fine. lld reads the
# same file without complaint, so the Windows build routes through it.
#
# Verified by isolation on a Windows runner: dot-in-name fails, long names
# without one pass, and the same command succeeds with -fuse-ld=lld. Remove this
# once binutils accepts what Go emits.
ifeq ($(shell go env GOOS),windows)
C_SHARED_LDFLAGS := -extldflags "-fuse-ld=lld"
endif

.DEFAULT_GOAL := help
.PHONY: help build build-shared build-dev build-linux release-archive test test-race vet fmt lint tidy clean run db-shell cpa-docker-up cpa-docker-down cpa-docker-logs

help: ## Show available targets
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

build-shared: ## Build the C-ABI plugin shared library for the host OS
	@mkdir -p $(BIN_DIR)
	@case "$$(go env GOOS)" in \
		darwin) ext=dylib ;; \
		windows) ext=dll ;; \
		*) ext=so ;; \
	esac; \
	go build -buildmode=c-shared -tags cshared -trimpath \
		-ldflags '$(LDFLAGS) $(C_SHARED_LDFLAGS)' \
		-o $(BIN_DIR)/$(PLUGIN_NAME).$$ext ./cmd/plugin; \
	echo "built $(BIN_DIR)/$(PLUGIN_NAME).$$ext"

build-dev: ## Build the standalone development harness
	@mkdir -p $(BIN_DIR)
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(PLUGIN_NAME)-dev ./cmd/plugin

build: build-dev build-shared ## Build both the harness and the shared library

# --- Plugin store release ---------------------------------------------------
#
# Produces the archive the plugin store installs, for the machine running make.
# The matrix in .github/workflows/release.yml calls this on each target runner:
# c-shared cannot be cross-compiled, so there is one native build per platform.

RELEASE_DIR := dist/release

release-archive: ## Build and package the store archive for the host platform
	@mkdir -p $(BIN_DIR)
	@case "$$(go env GOOS)" in \
		darwin) ext=dylib ;; \
		windows) ext=dll ;; \
		*) ext=so ;; \
	esac; \
	lib=$(BIN_DIR)/$(PLUGIN_NAME)-v$(VERSION).$$ext; \
	go build -buildmode=c-shared -tags cshared -trimpath \
		-ldflags '$(LDFLAGS) $(C_SHARED_LDFLAGS)' \
		-o $$lib ./cmd/plugin && \
	go run ./cmd/release-archive \
		-lib $$lib \
		-version "$(VERSION)" \
		-goos "$$(go env GOOS)" \
		-goarch "$$(go env GOARCH)" \
		-out $(RELEASE_DIR)

run: ## Run the development harness (mock host + local management API/UI)
	@# $(ARGS) goes last so it can override the defaults above; Go's flag
	@# package lets the later occurrence win.
	go run ./cmd/plugin -data-dir ./.local -listen 127.0.0.1:8787 -management-key devkey $(ARGS)

# --- Real-CPA test loop -----------------------------------------------------
#
# c-shared output cannot be cross-compiled, so the library is built inside a
# Linux container matching the deployment target. See dev/cpa-docker/README.md.

PLUGIN_SO := dev/cpa-docker/plugins/$(PLUGIN_NAME).so
DOCKER_PLATFORM ?= linux/arm64

build-linux: ## Build the C-ABI plugin for linux ($(DOCKER_PLATFORM))
	@mkdir -p dev/cpa-docker/plugins
	docker run --rm --platform $(DOCKER_PLATFORM) \
		-v "$(CURDIR)":/src -w /src \
		-v "$$HOME/go/pkg/mod":/go/pkg/mod \
		-e GOFLAGS=-mod=mod -e CGO_ENABLED=1 \
		golang:1.26-bookworm \
		go build -buildmode=c-shared -tags cshared -trimpath -ldflags "$(LDFLAGS)" \
			-o $(PLUGIN_SO) ./cmd/plugin
	@echo "built $(PLUGIN_SO) ($(VERSION))"

cpa-docker-up: build-linux ## Build for linux and start a throwaway CPA with the plugin loaded
	docker rm -f cpa-plugin-test >/dev/null 2>&1 || true
	cd dev/cpa-docker && docker run -d --name cpa-plugin-test --platform $(DOCKER_PLATFORM) \
		-p 18317:8317 \
		-v "$$PWD/config.yaml":/CLIProxyAPI/config.yaml:ro \
		-v "$$PWD/auths":/root/.cli-proxy-api \
		-v "$$PWD/logs":/CLIProxyAPI/logs \
		-v "$$PWD/plugins":/CLIProxyAPI/plugins \
		-v "$$PWD/plugin-data":/CLIProxyAPI/plugin-data \
		eceasy/cli-proxy-api:latest
	@sleep 6
	@echo "CPA on http://127.0.0.1:18317 (management key: local-dev-key)"

cpa-docker-logs: ## Follow the throwaway CPA log
	docker logs -f cpa-plugin-test

cpa-docker-down: ## Stop and remove the throwaway CPA
	docker rm -f cpa-plugin-test

test: ## Run unit tests
	go test ./...

test-race: ## Run unit tests with the race detector
	go test -race ./...

vet: ## Run go vet
	go vet ./...

fmt: ## Format the tree
	gofmt -s -w .

tidy: ## Tidy go.mod/go.sum
	go mod tidy

clean: ## Remove build artifacts
	rm -rf $(BIN_DIR)

db-shell: ## Open a SQLite shell against the dev database (requires sqlite3)
	sqlite3 ./.local/state.db
