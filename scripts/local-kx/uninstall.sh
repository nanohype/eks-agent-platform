#!/usr/bin/env bash
# uninstall.sh — tear down the kx install.
#
# Removes the blank tenant CRs, the operator chart, the tenant
# workload namespace, the operator namespace, and the bedrock-mode
# AWS creds Secret. Leaves kx's upstream slices (ai-platform,
# autoscaling, argo-platform) alone — they belong to kx.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
EXPECTED_CTX="kind-kx"

red()   { printf '\033[31m%s\033[0m\n' "$*"; }
green() { printf '\033[32m%s\033[0m\n' "$*"; }

ctx=$(kubectl config current-context 2>/dev/null || true)
if [[ "$ctx" != "$EXPECTED_CTX" ]]; then
  red "kubectl context is '$ctx', expected '$EXPECTED_CTX'."
  exit 1
fi

echo "deleting blank-tenant CRs..."
kubectl delete -f "$REPO_ROOT/examples/blank-tenant/platform.yaml" --ignore-not-found

echo "uninstalling operator chart..."
helm uninstall operator --namespace eks-agent-platform --ignore-not-found 2>/dev/null || true

echo "deleting namespaces..."
kubectl delete namespace eks-agent-platform --ignore-not-found
kubectl delete namespace tenants-blank --ignore-not-found

echo "deleting bedrock-mode AWS creds Secret..."
kubectl delete secret agentgateway-aws --namespace agentgateway --ignore-not-found

green "── uninstall complete ──"
echo "kx's upstream slices (ai-platform, autoscaling, argo-platform) left in place."
echo "to remove those: cd ../kx && task stack:<slice>:disable"
