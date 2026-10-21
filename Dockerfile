# kapelo-worker with everything a task needs: OpenCode, git, Node.js and
# PostgreSQL for the checks. It runs as uid 1000, or any user given with
# --user.
#
#   docker build -t kapelo-worker .
ARG GO_IMAGE=golang:1.27-bookworm
ARG BASE_IMAGE=node:22-bookworm-slim

FROM ${GO_IMAGE} AS build
WORKDIR /src
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /kapelo-worker ./cmd/kapelo-worker

FROM ${BASE_IMAGE}
ARG OPENCODE_VERSION=1.19.3
# libnss-wrapper lets PostgreSQL run as a --user without a passwd entry.
# safe.directory: a repository mounted from the host belongs to another
# user, and git refuses such a repository unless it is marked safe.
RUN apt-get update \
 && apt-get install -y --no-install-recommends git ca-certificates postgresql-15 libnss-wrapper \
 && rm -rf /var/lib/apt/lists/* \
 && git config --system --add safe.directory '*' \
 && npm install -g --no-audit --no-fund "opencode-ai@${OPENCODE_VERSION}" \
 && npm cache clean --force
ENV PATH=/usr/lib/postgresql/15/bin:$PATH \
    HOME=/tmp \
    NEXT_TELEMETRY_DISABLED=1 \
    OPENCODE_DISABLE_AUTOUPDATE=1
COPY --from=build /kapelo-worker /usr/local/bin/kapelo-worker
WORKDIR /work
USER 1000:1000
ENTRYPOINT ["kapelo-worker"]
