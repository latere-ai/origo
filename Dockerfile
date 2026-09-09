# syntax=docker/dockerfile:1
#
# Developer image. It compiles the binary inside the image, so a clone with
# only a container runtime installed produces the same artifact the pipeline
# ships. The release pipeline uses Dockerfile.ci instead, which copies the
# binary the verify run already built and tested.
#
# The runtime stage is shared with Dockerfile.ci between the markers below
# and the two must stay byte for byte the same, so the developer image and
# the released image differ only in how the binary arrived.

# >>> shared runtime base <<<
# origod runs git as a subprocess, so the runtime is a minimal Debian with
# git rather than the static distroless base the Latere template uses.
# trixie ships git 2.47, above the 2.40 floor origod check enforces (spec
# 018, spec 017). The base is pinned by digest: a tag can be moved by its
# owner, which turns the base into a mutable input.
ARG RUNTIME_BASE=docker.io/library/debian:trixie-slim@sha256:d7e12182ce18b85b93007c1dedf31f2d29e01ccf3182cc4017c709b6259bc132
# >>> end shared runtime base <<<

ARG BUILDER_BASE=docker.io/library/golang:1.27-bookworm@sha256:648f440f42a0958804efb24df176f806f9d353b41f1c0627f666428e40310f6b

FROM --platform=${BUILDPLATFORM} ${BUILDER_BASE} AS build

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=none
ARG SOURCE_DATE_EPOCH=0

# GOTOOLCHAIN=local pins the compiler to the one in the base image so the
# build downloads nothing it did not declare.
ENV CGO_ENABLED=0 \
    GOFLAGS=-mod=readonly \
    GOTOOLCHAIN=local

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

# -trimpath and -buildid= make two builds of one commit produce identical
# bytes together with the fixed build time.
RUN BUILD_DATE=$(date -u -d "@${SOURCE_DATE_EPOCH}" +%Y-%m-%dT%H:%M:%SZ) && \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
        -trimpath -buildvcs=false \
        -ldflags "-s -w -buildid= \
            -X github.com/latere-ai/origo/internal/version.Version=${VERSION} \
            -X github.com/latere-ai/origo/internal/version.Commit=${COMMIT} \
            -X github.com/latere-ai/origo/internal/version.Date=${BUILD_DATE}" \
        -o /out/origod ./cmd/origod

FROM ${RUNTIME_BASE} AS runtime

# The binary sits on PATH under its own name, so `origod check` inside a
# pod is the command the install document tells an operator to run and
# the init container's `args: [check]` reads the same way.
COPY --from=build --chown=65532:65532 /out/origod /usr/local/bin/origod

# >>> shared runtime stage <<<
# git is the one binary origod needs beside itself. ca-certificates lets the
# node reach an object store over TLS. The package lists are removed so the
# layer holds the binaries and nothing that would let a shell in the
# container install more.
RUN apt-get update \
    && apt-get install -y --no-install-recommends git ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /var/lib/origo \
    && chown 65532:65532 /var/lib/origo
# 65532 matches the nonroot user of the distroless images, written
# numerically because a Kubernetes runAsNonRoot check cannot resolve a name.
USER 65532:65532
WORKDIR /
VOLUME ["/var/lib/origo"]
EXPOSE 8080 8081 7946/udp
ENTRYPOINT ["/usr/local/bin/origod"]
# >>> end shared runtime stage <<<
