# SPDX-License-Identifier: MIT
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test test-docker evaluate lint fmt vet spdx licenses check clean

build:
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/aireviewer-mcp ./cmd/aireviewer-mcp

test:
	go test -race ./...

# Runtime sandbox against a real Docker daemon (pulls alpine; ADR 0008).
test-docker:
	AIREVIEWER_DOCKER_TESTS=1 go test -count=1 -run 'TestRuntimeDocker' ./internal/mcpserver/

# End-to-end evaluation against evaluation/golden (calls the model; set MAX_COST).
MAX_COST ?= 10
evaluate: build
	bin/aireviewer-mcp evaluate run -plugin-dir . -max-cost $(MAX_COST) -out evaluation/results/$$(date +%Y%m%d-%H%M%S).json

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
