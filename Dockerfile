# syntax=docker/dockerfile:1
#
# ghcr.io/divmora/zenith: the agent plus the toolchains it most often needs
# to build and test a repository (Go, Node.js with npm/pnpm/yarn, Python
# with pytest, make, gcc). For other stacks, build FROM this image and add
# yours.
#
#   docker run --rm -v "$PWD:/workspace" -e ZENITH_MODEL -e ANTHROPIC_API_KEY \
#     ghcr.io/divmora/zenith run --task "..."
#
# Network isolation, CPU and memory limits are the job of whatever starts
# the container (see SECURITY.md).

ARG GO_VERSION=1.27
ARG NODE_VERSION=24

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-bookworm AS builder
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=0.0.0-dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w -X github.com/divmora/zenith/internal/version.Version=${VERSION}" \
    -o /out/zenith ./cmd/zenith

FROM node:${NODE_VERSION}-bookworm-slim AS node

# The Go image already has git, curl, gcc, make and the Go toolchain.
FROM golang:${GO_VERSION}-bookworm
ARG VERSION=0.0.0-dev
LABEL org.opencontainers.image.title="zenith" \
      org.opencontainers.image.description="Autonomous coding agent: plans, edits, tests and commits on a branch, with any LLM provider" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.source="https://github.com/divmora/zenith" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.vendor="DIVMORA Technologies"

COPY --from=node /usr/local/bin/node /usr/local/bin/node
COPY --from=node /usr/local/lib/node_modules /usr/local/lib/node_modules
RUN ln -s ../lib/node_modules/npm/bin/npm-cli.js /usr/local/bin/npm \
    && ln -s ../lib/node_modules/npm/bin/npx-cli.js /usr/local/bin/npx \
    && ln -s ../lib/node_modules/corepack/dist/corepack.js /usr/local/bin/corepack \
    && corepack enable --install-directory /usr/local/bin pnpm yarn \
    && apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates jq less python3 python3-pip python3-pytest python3-venv ripgrep unzip \
    && rm -rf /var/lib/apt/lists/* \
    && git config --system --add safe.directory '*' \
    && useradd --uid 10001 --home-dir /home/zenith --create-home --shell /bin/bash zenith \
    && mkdir -p /workspace && chown zenith:zenith /workspace

COPY --from=builder /out/zenith /usr/local/bin/zenith

# Caches live in the user's home, so a volume there keeps them between runs.
ENV HOME=/home/zenith \
    GOPATH=/home/zenith/go \
    GOCACHE=/home/zenith/.cache/go-build \
    COREPACK_HOME=/home/zenith/.cache/corepack \
    NPM_CONFIG_CACHE=/home/zenith/.npm \
    PATH=/home/zenith/go/bin:/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    COREPACK_ENABLE_DOWNLOAD_PROMPT=0
USER 10001
# Fetch pnpm and Yarn 1 now: the sandbox may have no network at run time.
# A project that pins another version (packageManager) still needs network.
# The home stays writable for any uid, so `docker run --user "$(id -u)"`
# works on a mounted checkout.
RUN corepack install --global pnpm@latest yarn@1 \
    && pnpm --version >/dev/null && yarn --version >/dev/null \
    && mkdir -p "$GOPATH" "$GOCACHE" "$NPM_CONFIG_CACHE" \
    && chmod -R a+rwX /home/zenith
WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/zenith"]
CMD ["help"]
