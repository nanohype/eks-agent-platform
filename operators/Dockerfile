# Build the operator binary.
#
# Base images are pinned to their multi-arch manifest-list digests, not just a
# floating tag — buildx still selects the right per-platform manifest from the
# index, but the exact bytes are fixed. Refresh a digest with
# `docker buildx imagetools inspect <image>:<tag>` and bump the tag comment
# alongside it.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS builder

WORKDIR /workspace

# Cache deps separately from source for faster rebuilds
COPY go.mod go.sum* ./
RUN go mod download

COPY api/ api/
COPY cmd/ cmd/
COPY internal/ internal/

# TARGETOS + TARGETARCH are auto-populated PER-PLATFORM by buildx — declared
# WITHOUT defaults on purpose: a default value shadows buildx's per-platform value
# and silently cross-builds the wrong arch (an amd64 binary inside the arm64 image
# -> "exec format error" on Graviton nodes). Plain `docker build` without
# --platform leaves them empty, so Go builds for the native host; pass an explicit
# --build-arg TARGETARCH=… to cross-build a specific arch by hand.
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X main.buildVersion=${VERSION}" \
    -o /workspace/manager ./cmd

# The metrics-shim binary — KEDA's work-queue bridge for SandboxPool
# autoscaling. One image, two binaries; the shim Deployment overrides
# the container command.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" \
    -o /workspace/metrics-shim ./cmd/metrics-shim

# Distroless runtime — non-root, minimal surface (digest-pinned; see note above)
FROM gcr.io/distroless/static-debian12:nonroot@sha256:aef9602f8710ec12bde19d593fed1f76c708531bb7aba205110f1029786ead7b
WORKDIR /
COPY --from=builder /workspace/manager /manager
COPY --from=builder /workspace/metrics-shim /metrics-shim
USER 65532:65532

# Liveness/readiness are owned by the Kubernetes Deployment's HTTP probes
# against /healthz and /readyz on the --health-probe-bind-address port.
# Distroless ships no shell or HTTP client, so a meaningful Docker-level
# HEALTHCHECK is not expressible; opt out explicitly.
HEALTHCHECK NONE

ENTRYPOINT ["/manager"]
