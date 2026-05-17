# Build the operator binary
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS builder

WORKDIR /workspace

# Cache deps separately from source for faster rebuilds
COPY go.mod go.sum* ./
RUN go mod download

COPY api/ api/
COPY cmd/ cmd/
COPY internal/ internal/

# TARGETOS + TARGETARCH are auto-populated by buildx when --platform is set.
# Defaults below let plain `docker build` still work on amd64/linux hosts.
ARG VERSION=dev
ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X main.buildVersion=${VERSION}" \
    -o /workspace/manager ./cmd

# Distroless runtime — non-root, minimal surface
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /
COPY --from=builder /workspace/manager /manager
USER 65532:65532

# Liveness/readiness are owned by the Kubernetes Deployment's HTTP probes
# against /healthz and /readyz on the --health-probe-bind-address port.
# Distroless ships no shell or HTTP client, so a meaningful Docker-level
# HEALTHCHECK is not expressible; opt out explicitly.
HEALTHCHECK NONE

ENTRYPOINT ["/manager"]
