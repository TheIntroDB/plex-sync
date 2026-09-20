# tidb-plex developer tasks.
#
# Everything here works with stock Go 1.24+ and no CGO, because the SQLite
# driver is pure Go.

BINARY  := tidb-plex
PKG     := github.com/TheIntroDB/plex-integration
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w \
	-X $(PKG)/internal/buildinfo.Version=$(VERSION) \
	-X $(PKG)/internal/buildinfo.Commit=$(COMMIT) \
	-X $(PKG)/internal/buildinfo.Date=$(DATE)

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

.PHONY: cover
cover: ## Run tests and write coverage.html
	go test -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html

.PHONY: lint
lint: ## Run gofmt and go vet
	@test -z "$$(gofmt -l . | grep -v '^$$')" || { echo "not gofmt'd:"; gofmt -l .; exit 1; }
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
