BINARY    := llmock
BUILD_DIR := bin

GO := go

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w \
           -X main.Version=$(VERSION) \
           -X main.Commit=$(COMMIT) \
           -X main.Date=$(DATE)

.PHONY: all build test fmt vet tidy check release snapshot clean

all: check build

build:
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY) ./cmd/llmock

test:
	$(GO) test ./...

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

tidy:
	$(GO) mod tidy

check: fmt vet test

release:
	@which goreleaser > /dev/null 2>&1 || { echo "goreleaser not found. Install: brew install goreleaser"; exit 1; }
	goreleaser release --clean

snapshot:
	@which goreleaser > /dev/null 2>&1 || { echo "goreleaser not found. Install: brew install goreleaser"; exit 1; }
	goreleaser release --snapshot --clean

clean:
	rm -rf $(BUILD_DIR) dist/
