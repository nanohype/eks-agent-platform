# syntax=docker/dockerfile:1

# Self-hosted sandbox worker for Managed Agents.
#
# Runs `ant beta:worker`, which claims sessions from a `self_hosted`
# environment's work queue and executes the agent's tool calls inside this
# container — agent code and its filesystem stay inside the cluster. The
# workload that schedules it supplies `ANTHROPIC_ENVIRONMENT_ID` and
# `ANTHROPIC_ENVIRONMENT_KEY`; they are not baked into the image.
#
# Pinned to the multi-arch manifest-list digest, not just the tag — buildx still
# picks the right per-platform manifest, but the bytes are fixed. Refresh with
# `docker buildx imagetools inspect debian:bookworm-slim`.
FROM debian:bookworm-slim@sha256:7b140f374b289a7c2befc338f42ebe6441b7ea838a042bbd5acbfca6ec875818

# The agent's tool calls run in this container, so it carries a real
# userland — bash at /bin/bash (required by `ant`), git, and TLS roots
# for egress to api.anthropic.com.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl git \
    && rm -rf /var/lib/apt/lists/*

# The Anthropic `ant` CLI — the worker process itself. The release tarball is
# pinned to a per-architecture SHA-256 (from the upstream
# ant_${ANT_VERSION}_checksums.txt) and verified before extraction, so a
# tampered, swapped, or truncated download fails the build instead of shipping
# an unverified binary into the sandbox. Bump both the version and the two
# digests together when moving to a new `ant` release.
ARG ANT_VERSION=1.9.1
ARG TARGETARCH=amd64
ARG ANT_SHA256_amd64=221abeda39d65e202f8c07a85c94744023d2bddef7b0906301715b73d78a1cfc
ARG ANT_SHA256_arm64=5cd4bda9ea9c9621a7403d27e256d37b18bfd95a85ba97435b717e2fd03462c7
RUN set -eux; \
    case "${TARGETARCH}" in \
      amd64) ant_sha256="${ANT_SHA256_amd64}" ;; \
      arm64) ant_sha256="${ANT_SHA256_arm64}" ;; \
      *) echo "unsupported TARGETARCH: ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    curl -fsSL -o /tmp/ant.tar.gz \
      "https://github.com/anthropics/anthropic-cli/releases/download/v${ANT_VERSION}/ant_${ANT_VERSION}_linux_${TARGETARCH}.tar.gz"; \
    echo "${ant_sha256}  /tmp/ant.tar.gz" | sha256sum -c -; \
    tar -xz -C /usr/local/bin -f /tmp/ant.tar.gz ant; \
    rm -f /tmp/ant.tar.gz; \
    ant --version

# Unprivileged user; /workspace is the tool-execution working directory.
RUN useradd --create-home --uid 1000 --shell /bin/bash worker \
    && mkdir -p /workspace \
    && chown worker:worker /workspace
USER worker
WORKDIR /workspace

# Probes are owned by the Kubernetes workload, not Docker.
HEALTHCHECK NONE

ENTRYPOINT ["ant"]
CMD ["beta:worker", "poll"]
