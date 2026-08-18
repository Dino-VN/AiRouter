# aihub — a single-binary, multi-tenant proxy for Codex and Antigravity accounts.
#
# The web UI is compiled by Bun/Vite into internal/webui/dist and embedded into
# the executable, so a release is one file: copy it to another machine, give it a
# PostgreSQL URL, and run it. Neither Go nor Node is needed there.
#
# Usual sequence:
#   make deps      # install UI dependencies (once)
#   make build     # UI + binary  ->  bin/aihub
#   make run       # build and start it against $AIHUB_DATABASE_URL

SHELL := /bin/bash

VERSION ?= 0.1.0
BINARY  := bin/aihub
UI_DIR  := web
DIST    := internal/webui/dist

GO      ?= go
BUN     ?= bun
GOFLAGS := -trimpath
LDFLAGS := -s -w -X main.version=$(VERSION)

# Cross-compilation targets for `make release`.
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64

.DEFAULT_GOAL := help

## help: list the available targets
.PHONY: help
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /' | sort

## deps: install Go modules and UI dependencies
.PHONY: deps
deps:
	$(GO) mod download
	cd $(UI_DIR) && $(BUN) install

## ui: build the web UI into the embed directory
.PHONY: ui
ui:
	cd $(UI_DIR) && $(BUN) run build
	@test -f $(DIST)/index.html || { echo "UI build produced no $(DIST)/index.html"; exit 1; }
	@echo "web ui built into $(DIST)"

## ui-dev: run the Vite dev server (proxies /api to a local aihub)
.PHONY: ui-dev
ui-dev:
	cd $(UI_DIR) && $(BUN) run dev

## build: build the UI and then the binary
.PHONY: build
build: ui
	$(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/aihub
	@echo "built $(BINARY) $(VERSION)"

## build-go: build the binary only, reusing whatever UI is already embedded
.PHONY: build-go
build-go:
	$(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/aihub

## release: cross-compile a stripped binary per platform into dist/
.PHONY: release
release: ui
	@mkdir -p dist
	@for platform in $(PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; \
		out=dist/aihub-$(VERSION)-$$os-$$arch; \
		if [ "$$os" = "windows" ]; then out=$$out.exe; fi; \
		echo "building $$out"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
			$(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $$out ./cmd/aihub || exit 1; \
	done
	@echo "release binaries are in dist/"

## run: build and start the server
.PHONY: run
run: build
	./$(BINARY)

## dev: start the server without rebuilding the UI (use with `make ui-dev`)
.PHONY: dev
dev:
	$(GO) run ./cmd/aihub

## migrate: apply database migrations and exit
.PHONY: migrate
migrate:
	$(GO) run ./cmd/aihub -migrate

# Both formatters below take the same paths on purpose. Pointed at ".", gofumpt
# walks web/node_modules too and would rewrite a vendored Go file that happens
# to ship inside one of the UI dependencies.
## fmt: format the Go sources
.PHONY: fmt
fmt:
	$(GO) run mvdan.cc/gofumpt@latest -l -w ./cmd ./internal 2>/dev/null || gofmt -l -w ./cmd ./internal

## vet: run go vet
.PHONY: vet
vet:
	$(GO) vet ./...

## test: run the Go test suite
.PHONY: test
test:
	$(GO) test ./... -count=1

## check: format, vet and test
.PHONY: check
check: fmt vet test

## tidy: tidy go.mod
.PHONY: tidy
tidy:
	$(GO) mod tidy

## docker: build the container image
.PHONY: docker
docker:
	docker build -t aihub:$(VERSION) -t aihub:latest .

## clean: remove build output (the embedded UI placeholder is left alone)
.PHONY: clean
clean:
	rm -rf bin dist $(UI_DIR)/dist $(UI_DIR)/node_modules/.vite
