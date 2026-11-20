# SPDX-License-Identifier: MIT
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test test-docker lint fmt vet spdx licenses check clean

build:
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/aireviewer-mcp ./cmd/aireviewer-mcp

test:
	go test -race ./...

# Runtime sandbox against a real Docker daemon (pulls alpine; ADR 0008).
test-docker:
	AIREVIEWER_DOCKER_TESTS=1 go test -count=1 -run 'TestRuntimeDocker' ./internal/mcpserver/

lint:
	golangci-lint run ./...

fmt:
	gofmt -l -w .

vet:
	go vet ./...

spdx:
	./scripts/check-spdx.sh

licenses:
	go run github.com/google/go-licenses/v2@latest check ./... --disallowed_types=forbidden,restricted

check: vet lint spdx test

clean:
	rm -rf bin dist
