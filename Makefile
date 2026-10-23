# Version is derived from the git tag; `git tag vX.Y.Z` is the single source of truth.
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

.PHONY: install build test version

install:
	go install -ldflags "$(LDFLAGS)" ./cmd/because

build:
	go build -ldflags "$(LDFLAGS)" -o because ./cmd/because

test:
	go test ./...

version:
	@echo $(VERSION)
