# Build the operator binary.
#
# Base images are pinned to their multi-arch manifest-list digests, not just a
# floating tag — buildx still selects the right per-platform manifest from the
# index, but the exact bytes are fixed. Refresh a digest with
# `docker buildx imagetools inspect <image>:<tag>` and bump the tag comment
# alongside it.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine@sha256:eb61c3873c97416fff0116553fff65d67a3549f72e11d9758849a689cdd8b523 AS builder

WORKDIR /workspace

# Cache deps separately from source for faster rebuilds
COPY go.mod go.sum* ./

# The golang images set GOTOOLCHAIN=local, so go.mod's `toolchain` line is not
# honoured here: the binary is compiled by whatever Go the image above ships,
# however far go.mod has moved. A security bump to the toolchain directive
# therefore does not reach the shipped image unless the digest moves with it.
# Fail the build when the image is older than the directive, instead of
# shipping the older compiler without a word.
RUN want="$(sed -n 's/^toolchain go//p' go.mod)"; have="$(go env GOVERSION)"; have="${have#go}"; \
    num() { echo "$1" | awk -F. '{ printf "%d%04d%04d", $1, $2, $3 }'; }; \
    if [ -n "$want" ] && [ "$(num "$have")" -lt "$(num "$want")" ]; then \
      echo "builder image ships go${have}, but go.mod declares toolchain go${want} and GOTOOLCHAIN=local builds with the image's go: move the golang digest above to an image with go${want} or newer" >&2; \
      exit 1; \
    fi

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
