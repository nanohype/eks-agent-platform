# syntax=docker/dockerfile:1

# eval-runner — the image the Argo `eval-runner` WorkflowTemplate invokes for
# EvalSuite runs. The workflow shells out to `evaluate` + `score` (this CLI) and
# to `aws` / `jq` / `kubectl` in the same container, so the runtime carries a
# real userland rather than being distroless like the operator.

# ── build stage: bundle the CLI to a single file ────────────────────────────
FROM node:24-bookworm-slim AS builder
WORKDIR /src

# corepack pins pnpm from the root package.json's packageManager field.
RUN corepack enable

# Whole workspace: pnpm --frozen-lockfile validates the lockfile against every
# declared workspace package, so a partial copy would fail. .dockerignore keeps
# node_modules / dist / .git / terraform out of the context.
COPY . .

RUN pnpm install --frozen-lockfile

# Build eval-runner's dependency graph (core → pricing → sdk) so the pricing
# data file lands in its dist, then bundle the CLI from source into one ESM
# file. The @eks-agent/sdk import is types-only and erases at build.
RUN pnpm --filter "...@eks-agent/eval-runner" build \
    && pnpm --filter @eks-agent/eval-runner bundle

# ── runtime stage: slim node + the OS tools the workflow shells to ──────────
FROM node:24-bookworm-slim AS runtime

ARG TARGETARCH
# Pinned tool versions (resolved from upstream at image authoring time).
ARG AWSCLI_VERSION=2.36.1
ARG KUBECTL_VERSION=v1.33.13

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl jq unzip \
    && case "${TARGETARCH:-amd64}" in \
         amd64) awsarch=x86_64; kubearch=amd64 ;; \
         arm64) awsarch=aarch64; kubearch=arm64 ;; \
         *)     awsarch=x86_64; kubearch=amd64 ;; \
       esac \
    # AWS CLI v2 — used by the score step to upload reports to S3.
    && curl -fsSL "https://awscli.amazonaws.com/awscli-exe-linux-${awsarch}-${AWSCLI_VERSION}.zip" -o /tmp/awscli.zip \
    && unzip -q /tmp/awscli.zip -d /tmp \
    && /tmp/aws/install \
    # kubectl — used by the writeback step to patch EvalSuite.status.
    && curl -fsSL "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${kubearch}/kubectl" -o /usr/local/bin/kubectl \
    && chmod +x /usr/local/bin/kubectl \
    # Drop the build-only tools + apt lists to slim the layer.
    && apt-get purge -y --auto-remove curl unzip \
    && rm -rf /var/lib/apt/lists/* /tmp/aws /tmp/awscli.zip

# The bundled CLI and its `evaluate` / `score` entry shims.
COPY --from=builder /src/packages/eval-runner/dist/cli.bundle.js /app/cli.js
RUN printf '#!/bin/sh\nexec node /app/cli.js evaluate "$@"\n' > /usr/local/bin/evaluate \
    && printf '#!/bin/sh\nexec node /app/cli.js score "$@"\n' > /usr/local/bin/score \
    && chmod +x /usr/local/bin/evaluate /usr/local/bin/score

# Unprivileged runtime. The `node` user (uid 1000) ships with the base image.
USER node
WORKDIR /app

# Probes/lifecycle are owned by the Argo workflow pod, not Docker.
HEALTHCHECK NONE

ENTRYPOINT []
CMD ["evaluate"]
