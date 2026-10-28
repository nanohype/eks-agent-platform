# syntax=docker/dockerfile:1

# eval-runner — the image the Argo `eval-runner` WorkflowTemplate invokes for
# EvalSuite runs. The workflow shells out to `evaluate` + `score` (this CLI) and
# to `aws` / `jq` / `kubectl` in the same container, so the runtime carries a
# real userland rather than being distroless like the operator.
#
# Both stages pin the multi-arch manifest-list digest, not just the tag — buildx
# still picks the right per-platform manifest, but the bytes are fixed. Refresh
# with `docker buildx imagetools inspect node:24-trixie-slim`, take a digest
# past the release-age floor Renovate holds updates to, and move both stages
# together.

# ── build stage: bundle the CLI to a single file ────────────────────────────
# Runs on the build host's platform. Its only output is one JavaScript file,
# the same for every target, so emulating this stage per target platform would
# only make an arm64 build under QEMU slower.
FROM --platform=$BUILDPLATFORM node:24-trixie-slim@sha256:43b9ba1142b8dcf33fa8f922ca1be85714adc444e0586c7cf7726daaeabc1bea AS builder
WORKDIR /src

# corepack pins pnpm from the root package.json's packageManager field.
RUN corepack enable

# Whole workspace: pnpm --frozen-lockfile validates the lockfile against every
# declared workspace package, so a partial copy would fail.
# packages/eval-runner/Dockerfile.dockerignore keeps node_modules / dist / .git /
# terraform out of the context.
COPY . .

RUN pnpm install --frozen-lockfile

# `@eks-agent/eval-runner...` selects eval-runner and every workspace package it
# depends on (core, pricing), and pnpm runs each one's own build script in
# dependency order, pricing's copy of its data file into dist among them. The
# bundle inlines core and pricing from those dist entry points.
RUN pnpm --filter "@eks-agent/eval-runner..." build \
    && pnpm --filter @eks-agent/eval-runner bundle

# ── runtime stage: slim node + the OS tools the workflow shells to ──────────
FROM node:24-trixie-slim@sha256:43b9ba1142b8dcf33fa8f922ca1be85714adc444e0586c7cf7726daaeabc1bea AS runtime

# Populated per platform by buildx, and declared without a default: a default
# shadows the per-platform value and cross-builds the wrong architecture.
ARG TARGETARCH

# The AWS CLI and kubectl downloads are each pinned to a per-architecture
# SHA-256 and verified before use, so a tampered, swapped or truncated download
# fails the build instead of shipping. Renovate bumps the two versions and
# leaves the digests alone: a bump lands with digests naming the old release,
# sha256sum fails the build, and that failure is the prompt to refresh them
# from the published sources —
#   kubectl: https://dl.k8s.io/release/<version>/bin/linux/<arch>/kubectl.sha256
#   AWS CLI: https://awscli.amazonaws.com/awscli-exe-linux-<x86_64|aarch64>-<version>.zip,
#            hashed only after its .sig verifies against the AWS CLI team's
#            PGP key from the AWS CLI install guide (fingerprint
#            FB5D B77F D5C1 18B8 0511 ADA8 A631 0ACC 4672 475C).
#
# Which release each pin names. kubectl stays on the minor whose ±1
# version-skew window spans the EKS versions the platform's clusters run, at
# that minor's newest patch past the release-age floor Renovate holds updates
# to. A renovate.json packageRule holds KUBECTL_VERSION to that minor, so
# moving to another minor follows a change of the cluster version target. The
# AWS CLI is the newest 2.x release past the same floor.
ARG AWSCLI_VERSION=2.38.5
ARG AWSCLI_SHA256_amd64=5def7d1b7e55d892f9a381bb2cb2081a685712464be0d36d5d706af0ac48d0d3
ARG AWSCLI_SHA256_arm64=07711a936e9d271045ab937d2c2c5390c25f419e4ba9e25adb9e996031e3f6be
ARG KUBECTL_VERSION=v1.36.5
ARG KUBECTL_SHA256_amd64=33bc88a24c3b09cf55bfd59e6ca977881b61fc3f25ab65bc80c65b430248648e
ARG KUBECTL_SHA256_arm64=88fc1aca8fd0c1b44fe379f67c39bd6858bdfc50241dc55775f84d0f5b31da3a

RUN set -eux; \
    case "${TARGETARCH}" in \
      amd64) awsarch=x86_64;  aws_sha256="${AWSCLI_SHA256_amd64}"; kubectl_sha256="${KUBECTL_SHA256_amd64}" ;; \
      arm64) awsarch=aarch64; aws_sha256="${AWSCLI_SHA256_arm64}"; kubectl_sha256="${KUBECTL_SHA256_arm64}" ;; \
      *) echo "unsupported TARGETARCH: '${TARGETARCH}'" >&2; exit 1 ;; \
    esac; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates curl jq unzip; \
    # AWS CLI v2 — the score step uploads reports to S3.
    curl -fsSL --connect-timeout 30 --max-time 600 -o /tmp/awscli.zip \
      "https://awscli.amazonaws.com/awscli-exe-linux-${awsarch}-${AWSCLI_VERSION}.zip"; \
    echo "${aws_sha256}  /tmp/awscli.zip" | sha256sum -c -; \
    unzip -q /tmp/awscli.zip -d /tmp; \
    /tmp/aws/install; \
    # kubectl — the writeback step patches EvalSuite.status.
    curl -fsSL --connect-timeout 30 --max-time 600 -o /usr/local/bin/kubectl \
      "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl"; \
    echo "${kubectl_sha256}  /usr/local/bin/kubectl" | sha256sum -c -; \
    chmod +x /usr/local/bin/kubectl; \
    # Drop the build-only tools + apt lists to slim the layer.
    apt-get purge -y --auto-remove curl unzip; \
    rm -rf /var/lib/apt/lists/* /tmp/aws /tmp/awscli.zip

# No package manager at runtime: the workflow runs `node` on one bundled file
# and installs nothing. npm goes because its vendored dependency tree is what
# image scanners report against this image; corepack and yarn go because
# nothing at runtime uses them. The RUN ends by asserting the result, so a base
# image that ships a package manager somewhere this list does not reach fails
# the build rather than shipping it, and so does one whose node did not survive.
RUN set -eu; \
    rm -rf /usr/local/lib/node_modules /opt/yarn-v* \
      /usr/local/bin/npm /usr/local/bin/npx /usr/local/bin/corepack \
      /usr/local/bin/yarn /usr/local/bin/yarnpkg; \
    for pm in npm npx corepack yarn yarnpkg; do \
      if command -v "$pm" >/dev/null 2>&1; then \
        echo "package manager still on PATH: $pm ($(command -v "$pm"))" >&2; exit 1; \
      fi; \
    done; \
    node --version

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
