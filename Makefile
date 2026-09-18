SHELL := /bin/bash

MODULE      := github.com/yangshoulai/codex-turn-state-manager
BIN_DIR     := dist
PLUGIN_NAME := codex-turn-state-manager

# CPA loads plugins as C-ABI shared libraries, so CGO is mandatory (see
# docs: "插件技术选型"). Cross-compiling with GOOS alone is NOT supported for
# c-shared output -- build on the target OS/arch.
export CGO_ENABLED := 1

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X $(MODULE)/internal/version.Version=$(VERSION)

.DEFAULT_GOAL := help
.PHONY: help build build-shared build-dev test test-race vet fmt lint tidy clean run db-shell

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
	go build -buildmode=c-shared -tags cshared -trimpath -ldflags "$(LDFLAGS)" \
		-o $(BIN_DIR)/$(PLUGIN_NAME).$$ext ./cmd/plugin; \
	echo "built $(BIN_DIR)/$(PLUGIN_NAME).$$ext"

build-dev: ## Build the standalone development harness
	@mkdir -p $(BIN_DIR)
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(PLUGIN_NAME)-dev ./cmd/plugin

build: build-dev build-shared ## Build both the harness and the shared library

run: ## Run the development harness (mock host + local management API/UI)
	go run ./cmd/plugin -data-dir ./.local -listen 127.0.0.1:8787 -management-key devkey

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
