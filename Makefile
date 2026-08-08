SDK_DIR   ?= .sdk
SCHEMA_DIR = internal/tree/schemas
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS    = -s -w -X github.com/KudcraftsHQ/meta-ads-cli/internal/dispatch.Version=$(VERSION)

.PHONY: build test vet fmt lint tree sdk clean install snapshot

build:
	CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' -o bin/meta-ads ./cmd/meta-ads

install:
	CGO_ENABLED=0 go install -ldflags '$(LDFLAGS)' ./cmd/meta-ads

test:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

lint: fmt vet test

# Download the Facebook SDK. Only needed when regenerating the command tree.
sdk:
	python3 tools/fetch_sdk.py --out $(SDK_DIR)

# Regenerate the command tree from the SDK and rebuild.
#
# The generated schemas are committed, so this only runs when Meta ships a new
# API version -- nobody needs Python to build or contribute otherwise.
tree: sdk
	python3 tools/gen_command_tree.py --sdk $(SDK_DIR) --out $(SCHEMA_DIR)

snapshot:
	goreleaser release --snapshot --clean

clean:
	rm -rf bin dist $(SDK_DIR)
