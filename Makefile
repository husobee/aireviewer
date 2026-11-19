# SPDX-License-Identifier: MIT
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test lint fmt vet spdx licenses check clean

build:
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/aireviewer-mcp ./cmd/aireviewer-mcp

test:
	go test -race ./...

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
