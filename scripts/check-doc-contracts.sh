#!/usr/bin/env bash
# Doc-contract gate — fail if on-call prose points at paths the code does not
# have. Two contracts, both verified against the live trees:
#
#   1. agent-iam lives in landing-zone (components/aws/agent-iam). This repo
#      has no terraform/components/agent-iam. A runbook that cd's there during
#      a compromise is a dead end.
#   2. Cluster-scoped SSM is keyed on the EKS cluster name:
#        /eks-agent-platform/<cluster>/…
#      not the bare environment token. model-import is the one landing-zone
#      component that keys on environment; its docs may say so explicitly.
#      Everything else (agent-iam, kill-switch, bedrock, …) is cluster-keyed.
#
# Sibling to scripts/no-placeholders.sh — same "prose is a contract" idea.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

fail=0

# ── 1. agent-iam is not in this repo ──────────────────────────────────────────
# Match the old path as a literal string. Allow the gate script itself and any
# mention that is clearly saying "do not use this path".
hits=$(grep -rnE 'terraform/components/agent-iam' \
  --include='*.md' --include='*.yaml' --include='*.yml' --include='*.ts' \
  --include='*.mts' --include='*.go' --include='*.tf' --include='*.hcl' \
  --include='*.txt' --include='*.tpl' \
  --exclude-dir='.git' --exclude-dir='node_modules' --exclude-dir='dist' \
  --exclude-dir='vendor' --exclude-dir='.terraform' \
  --exclude-dir='.terragrunt-cache' \
  . 2>/dev/null || true)

if [ -n "$hits" ]; then
  echo "agent-iam is owned by landing-zone — terraform/components/agent-iam does not exist here:"
  echo "$hits"
  echo
  echo "Point at landing-zone/components/aws/agent-iam (or its live/ leaf) instead."
  fail=1
fi

# ── 2. SSM paths use <cluster>, not bare <env> ────────────────────────────────
# Allow model-import docs (environment-keyed by design in landing-zone) and this
# script. Everything else that pastes /eks-agent-platform/<env>/ is lying to
# the on-call engineer.
hits=$(grep -rnE '/eks-agent-platform/<env>(/|$|\*)' \
  --include='*.md' --include='*.yaml' --include='*.yml' --include='*.ts' \
  --include='*.mts' --include='*.go' --include='*.tf' --include='*.hcl' \
  --include='*.txt' --include='*.tpl' \
  --exclude-dir='.git' --exclude-dir='node_modules' --exclude-dir='dist' \
  --exclude-dir='vendor' --exclude-dir='.terraform' \
  --exclude-dir='.terragrunt-cache' \
  . 2>/dev/null \
  | grep -v 'scripts/check-doc-contracts.sh' \
  | grep -v 'model-import' \
  | grep -v 'hero.excalidraw' \
  || true)

if [ -n "$hits" ]; then
  echo "SSM contract is cluster-keyed — use /eks-agent-platform/<cluster>/… not <env>:"
  echo "$hits"
  echo
  echo "Operator + agent-iam + kill-switch + bedrock + … all key on cluster name."
  echo "model-import is the documented environment-keyed exception."
  fail=1
fi

if [ "$fail" -ne 0 ]; then
  exit 1
fi
echo "✓ doc contracts hold (agent-iam path + cluster-keyed SSM)"
