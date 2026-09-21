# plex-sync developer tasks.
#
# Everything here works with stock Go 1.24+ and no CGO, because the SQLite
# driver is pure Go.

BINARY  := plex-sync
PKG     := github.com/TheIntroDB/plex-sync
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w \
	-X $(PKG)/internal/buildinfo.Version=$(VERSION) \
	-X $(PKG)/internal/buildinfo.Commit=$(COMMIT) \
	-X $(PKG)/internal/buildinfo.Date=$(DATE)

# The formatter, pinned. gofumpt is gofmt plus the rules gofmt leaves to taste,
# and it is what the source in this repository is written in: 'make fmt-check'
# has to agree with what a contributor's editor produces, so both read this
# version rather than whatever happens to be on PATH.
GOFUMPT_VERSION := v0.12.0
GOFUMPT         ?= $(shell command -v gofumpt 2>/dev/null || echo $(shell go env GOPATH)/bin/gofumpt)

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build the binary into ./bin
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) .

.PHONY: install
install: ## Install into GOPATH/bin
	go install -trimpath -ldflags "$(LDFLAGS)" .

.PHONY: test
test: ## Run all tests
	go test ./...

.PHONY: test-race
test-race: ## Run all tests with the race detector
	go test -race ./...

.PHONY: test-live
test-live: ## Run the tests that need a copy of a real Plex database
	./scripts/live-e2e.sh

.PHONY: cover
cover: ## Run tests and write coverage.html
	go test -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html

.PHONY: fmt
fmt: ## Format all Go source with the pinned gofumpt
	@command -v $(GOFUMPT) >/dev/null 2>&1 || { echo "$(GOFUMPT) is not installed; run 'make tools'"; exit 1; }
	$(GOFUMPT) -w .

.PHONY: fmt-check
fmt-check: ## Fail if any Go source is not formatted
	@command -v $(GOFUMPT) >/dev/null 2>&1 || { echo "$(GOFUMPT) is not installed; run 'make tools'"; exit 1; }
	@unformatted="$$($(GOFUMPT) -l . | grep -v '^$$')"; \
	if [ -n "$$unformatted" ]; then \
		echo "::error::these files are not formatted:"; \
		echo "$$unformatted"; \
		echo; \
		echo "run 'make fmt' to fix them"; \
		exit 1; \
	fi
	@echo "formatting ok ($(GOFUMPT_VERSION))"

.PHONY: tools
tools: ## Install the pinned developer tools
	go install mvdan.cc/gofumpt@$(GOFUMPT_VERSION)

.PHONY: lint
lint: fmt-check ## Check formatting and run go vet
	go vet ./...

.PHONY: tidy
tidy: ## Tidy the module
	go mod tidy

.PHONY: snapshot
snapshot: ## Build all release targets without publishing
	goreleaser release --snapshot --clean

.PHONY: run
run: build ## Build and start the terminal interface
	./bin/$(BINARY)
