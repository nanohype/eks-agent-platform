# syntax=docker/dockerfile:1

# Self-hosted sandbox worker for Managed Agents.
#
# Runs `ant beta:worker`, which claims sessions from a `self_hosted`
# environment's work queue and executes the agent's tool calls inside this
# container — agent code and its filesystem stay inside the cluster. The
# workload that schedules it supplies `ANTHROPIC_ENVIRONMENT_ID` and
# `ANTHROPIC_ENVIRONMENT_KEY`; they are not baked into the image.
FROM debian:bookworm-slim

# The agent's tool calls run in this container, so it carries a real
# userland — bash at /bin/bash (required by `ant`), git, and TLS roots
# for egress to api.anthropic.com.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl git \
    && rm -rf /var/lib/apt/lists/*

# The Anthropic `ant` CLI — the worker process itself.
ARG ANT_VERSION=1.9.1
ARG TARGETARCH=amd64
RUN curl -fsSL \
      "https://github.com/anthropics/anthropic-cli/releases/download/v${ANT_VERSION}/ant_${ANT_VERSION}_linux_${TARGETARCH}.tar.gz" \
      | tar -xz -C /usr/local/bin ant \
    && ant --version

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
