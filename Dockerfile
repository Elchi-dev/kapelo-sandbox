# Two images from one build:
#
#   sandbox (default)  the Kapelo sandbox: Node.js, git, Python, PostgreSQL,
#                      ripgrep and build tools; data in /data
#   worker             the standalone worker: OpenCode, git, Node.js and
#                      PostgreSQL for the checks
#
#   docker build -t kapelo-sandbox .
#   docker build --target worker -t kapelo-worker .
ARG GO_IMAGE=golang:1.27-bookworm
ARG SANDBOX_BASE=node:22-bookworm
ARG WORKER_BASE=node:22-bookworm-slim

FROM ${GO_IMAGE} AS build
WORKDIR /src
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /kapelo-sandbox ./cmd/kapelo-sandbox

FROM ${WORKER_BASE} AS worker
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
COPY --from=build /kapelo-sandbox /usr/local/bin/kapelo-sandbox
WORKDIR /work
USER 1000:1000
ENTRYPOINT ["kapelo-sandbox", "worker"]

FROM ${SANDBOX_BASE} AS sandbox
RUN apt-get update \
 && apt-get install -y --no-install-recommends postgresql-15 libnss-wrapper ripgrep jq zip unzip \
 && rm -rf /var/lib/apt/lists/* \
 && mkdir -p /data && chown 1000:1000 /data
ENV PATH=/usr/lib/postgresql/15/bin:$PATH \
    KAPELO_DATA=/data \
    NEXT_TELEMETRY_DISABLED=1
COPY --from=build /kapelo-sandbox /usr/local/bin/kapelo-sandbox
VOLUME /data
WORKDIR /data
USER 1000:1000
HEALTHCHECK --interval=1m --timeout=10s CMD ["kapelo-sandbox", "status"]
ENTRYPOINT ["kapelo-sandbox"]
