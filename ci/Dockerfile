# SPDX-License-Identifier: MIT
#
# CI image for aireviewer headless mode (SPEC §12, ADR 0011): Claude Code,
# the aireviewer plugin and a prebuilt aireviewer-mcp. Build from the
# repository root:
#
#   docker build -f ci/Dockerfile -t aireviewer-ci:<version> .
#
# Pin CLAUDE_CODE_VERSION to a tested release for reproducible CI.

FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY skills/_shared ./skills/_shared
RUN CGO_ENABLED=1 go build -trimpath -ldflags '-s -w' -o /out/aireviewer-mcp ./cmd/aireviewer-mcp

FROM node:22-bookworm-slim
ARG CLAUDE_CODE_VERSION=latest
RUN apt-get update \
 && apt-get install -y --no-install-recommends git ca-certificates curl \
 && rm -rf /var/lib/apt/lists/* \
 && npm install -g "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" \
 && npm cache clean --force
# The plugin without Go sources: scripts/mcp.sh runs the prebuilt binary.
COPY .claude-plugin /opt/aireviewer/.claude-plugin
COPY agents /opt/aireviewer/agents
COPY skills /opt/aireviewer/skills
COPY scripts/mcp.sh /opt/aireviewer/scripts/mcp.sh
COPY LICENSE /opt/aireviewer/LICENSE
COPY --from=build /out/aireviewer-mcp /opt/aireviewer/bin/aireviewer-mcp
COPY ci/run.sh /usr/local/bin/aireviewer-ci
RUN chmod 0755 /usr/local/bin/aireviewer-ci /opt/aireviewer/scripts/mcp.sh
# Headless mode: the server posts nothing except the opted-in augmentation
# summary, and runs no PR code.
ENV AIREVIEWER_HEADLESS=1
USER node
WORKDIR /home/node
ENTRYPOINT []
