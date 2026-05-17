#!/usr/bin/env bash
# install.sh — land eks-agent-platform on the kx (kind) cluster.
#
# Default mode: --disable-aws + blank-tenant smoke test. Validates that
# the operator's CR emission paths work against kx's real upstream
# CRDs (kagent / agentgateway / KEDA / Argo Workflows).
#
# --with-bedrock: also runs aws-creds-secret.sh which mounts a Secret
# with static AWS creds onto the agentgateway pod so it can actually
# call Bedrock.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
EXPECTED_CTX="kind-kx"
WITH_BEDROCK=0

while [[ $# -gt 0 ]]; do
  case $1 in
    --with-bedrock) WITH_BEDROCK=1; shift ;;
    -h|--help)
      cat <<EOF
Usage: $0 [--with-bedrock]

Installs the eks-agent-platform operator + blank-tenant smoke test
into the kind-kx cluster.

  --with-bedrock   Also mount AWS creds onto the agentgateway pod so
                   Bedrock invocations actually work end-to-end. Reads
                   creds from \$AWS_PROFILE (default 'default') via
                   'aws configure export-credentials', or directly from
                   \$AWS_ACCESS_KEY_ID + \$AWS_SECRET_ACCESS_KEY if set.

Prereq: kx slices enabled —
  cd ../kx
  task stack:ai-platform:enable     # kagent + agentgateway
  task stack:autoscaling:enable     # KEDA
  task stack:argo-platform:enable   # argo-workflows + argo-rollouts
EOF
      exit 0
      ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done

red()    { printf '\033[31m%s\033[0m\n' "$*"; }
green()  { printf '\033[32m%s\033[0m\n' "$*"; }
yellow() { printf '\033[33m%s\033[0m\n' "$*"; }

# 1. context guard
ctx=$(kubectl config current-context 2>/dev/null || true)
if [[ "$ctx" != "$EXPECTED_CTX" ]]; then
  red "kubectl context is '$ctx', expected '$EXPECTED_CTX'."
  red "switch with: kubectl config use-context $EXPECTED_CTX"
  exit 1
fi

# 2. upstream CRD presence check
echo "checking kx prerequisites..."
declare -A crd_to_slice=(
  ["agents.kagent.dev"]="task -d ../kx stack:ai-platform:enable     # kagent"
  ["routes.agentgateway.dev"]="task -d ../kx stack:ai-platform:enable     # agentgateway"
  ["scaledobjects.keda.sh"]="task -d ../kx stack:autoscaling:enable   # KEDA"
  ["workflows.argoproj.io"]="task -d ../kx stack:argo-platform:enable  # argo-workflows"
)
missing=0
for crd in "${!crd_to_slice[@]}"; do
  if ! kubectl get crd "$crd" >/dev/null 2>&1; then
    red "missing CRD: $crd"
    yellow "  enable with: ${crd_to_slice[$crd]}"
    missing=1
  fi
done
[[ $missing -eq 1 ]] && exit 1
green "all upstream CRDs present."

# 3. install operator chart
echo "installing operator chart..."
helm upgrade --install operator "$REPO_ROOT/charts/operator" \
  --namespace eks-agent-platform \
  --create-namespace \
  --values "$SCRIPT_DIR/values-local.yaml" \
  --wait --timeout 2m

# 4. wait for the deployment
kubectl wait --for=condition=Available deployment/operator \
  --namespace eks-agent-platform --timeout=2m

# 5. apply the blank-tenant smoke test
echo "applying blank-tenant smoke test..."
kubectl apply -f "$REPO_ROOT/examples/blank-tenant/platform.yaml"

# 6. wait for the Platform CR to reach Ready
kubectl wait --for=condition=NamespaceReady platform/blank \
  --namespace eks-agent-platform --timeout=2m || \
  yellow "Platform didn't reach NamespaceReady within 2m — check 'kubectl describe platform blank -n eks-agent-platform' for the failing step."

# 7. summary
echo
green "── install complete ──"
echo "operator:                  $(kubectl -n eks-agent-platform get deploy operator -o jsonpath='{.status.readyReplicas}')/$(kubectl -n eks-agent-platform get deploy operator -o jsonpath='{.spec.replicas}') ready"
echo "platform 'blank':          $(kubectl get platform blank -n eks-agent-platform -o jsonpath='{.status.phase}' 2>/dev/null || echo Pending)"
echo "tenant namespace:          $(kubectl get ns tenants-blank --no-headers 2>/dev/null | awk '{print $1, $2}' || echo not-yet-created)"
echo "agentgateway routes:       $(kubectl get -n agentgateway routes.agentgateway.dev -l 'eks-agent-platform/platform=blank' --no-headers 2>/dev/null | wc -l | tr -d ' ')"
echo "kagent agents:             $(kubectl get -n tenants-blank agents.kagent.dev --no-headers 2>/dev/null | wc -l | tr -d ' ')"
echo "keda scaledobjects:        $(kubectl get -n tenants-blank scaledobjects.keda.sh --no-headers 2>/dev/null | wc -l | tr -d ' ')"
echo

if [[ $WITH_BEDROCK -eq 1 ]]; then
  echo "── enabling Bedrock mode ──"
  exec "$SCRIPT_DIR/aws-creds-secret.sh"
else
  yellow "Bedrock invocations are NOT wired (operator is --disable-aws)."
  yellow "for end-to-end Bedrock: re-run with --with-bedrock"
  echo
  echo "next steps:"
  echo "  agentctl tenant list                       # cluster rollup"
  echo "  agentctl tenant get blank                  # one-tenant view"
  echo "  kubectl logs -n eks-agent-platform -l app.kubernetes.io/name=operator --tail=50"
  echo "  ./scripts/local-kx/uninstall.sh            # tear down"
fi
