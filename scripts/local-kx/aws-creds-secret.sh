#!/usr/bin/env bash
# aws-creds-secret.sh — mount AWS creds onto the kx agentgateway
# pod so Bedrock invocations actually work end-to-end.
#
# This is a local-dev shortcut. In production, agentgateway uses IRSA;
# kind has no OIDC provider, so we fall back to static AccessKey/Secret
# via a Secret + envFrom. Tenant isolation is degraded (single shared
# cred) — acceptable for local validation, not for any shared cluster.
set -euo pipefail

EXPECTED_CTX="kind-kx"
NS="agentgateway"
SECRET="agentgateway-aws"
DEPLOY="agentgateway"
REGION="${AWS_REGION:-us-west-2}"

red()    { printf '\033[31m%s\033[0m\n' "$*"; }
green()  { printf '\033[32m%s\033[0m\n' "$*"; }
yellow() { printf '\033[33m%s\033[0m\n' "$*"; }

ctx=$(kubectl config current-context 2>/dev/null || true)
if [[ "$ctx" != "$EXPECTED_CTX" ]]; then
  red "kubectl context is '$ctx', expected '$EXPECTED_CTX'."
  exit 1
fi

if ! kubectl get ns "$NS" >/dev/null 2>&1; then
  red "namespace '$NS' not found. enable kx's ai-platform slice first:"
  yellow "  cd ../kx && task stack:ai-platform:enable"
  exit 1
fi

# Resolve credentials. Prefer env vars (already-resolved by the user)
# over aws-cli export (which may prompt for SSO refresh).
if [[ -n "${AWS_ACCESS_KEY_ID:-}" && -n "${AWS_SECRET_ACCESS_KEY:-}" ]]; then
  echo "using AWS_ACCESS_KEY_ID + AWS_SECRET_ACCESS_KEY from env."
  AK="$AWS_ACCESS_KEY_ID"
  SK="$AWS_SECRET_ACCESS_KEY"
  ST="${AWS_SESSION_TOKEN:-}"
elif command -v aws >/dev/null 2>&1; then
  PROFILE="${AWS_PROFILE:-default}"
  echo "exporting creds from aws profile '$PROFILE'..."
  # 'aws configure export-credentials' was added in awscli v2.10; it
  # resolves SSO + role-chained creds to static. --format env emits
  # AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=... AWS_SESSION_TOKEN=...
  eval "$(aws configure export-credentials --profile "$PROFILE" --format env 2>/dev/null || true)"
  if [[ -z "${AWS_ACCESS_KEY_ID:-}" ]]; then
    red "aws configure export-credentials returned no creds for profile '$PROFILE'."
    yellow "  check 'aws sts get-caller-identity --profile $PROFILE'"
    exit 1
  fi
  AK="$AWS_ACCESS_KEY_ID"
  SK="$AWS_SECRET_ACCESS_KEY"
  ST="${AWS_SESSION_TOKEN:-}"
else
  red "no AWS creds available — set AWS_ACCESS_KEY_ID + AWS_SECRET_ACCESS_KEY, or install awscli."
  exit 1
fi

# Build the Secret (server-side apply so re-runs work).
echo "creating/updating secret $NS/$SECRET..."
kubectl create secret generic "$SECRET" \
  --namespace "$NS" \
  --from-literal=AWS_ACCESS_KEY_ID="$AK" \
  --from-literal=AWS_SECRET_ACCESS_KEY="$SK" \
  --from-literal=AWS_SESSION_TOKEN="$ST" \
  --from-literal=AWS_REGION="$REGION" \
  --dry-run=client -o yaml | kubectl apply -f -

# Patch the agentgateway Deployment to load envFrom the Secret.
# Strategic merge merges the env / envFrom lists rather than replacing.
echo "patching deployment $NS/$DEPLOY to load envFrom $SECRET..."
kubectl patch deployment "$DEPLOY" --namespace "$NS" --type strategic --patch "$(cat <<EOF
spec:
  template:
    spec:
      containers:
        - name: $DEPLOY
          envFrom:
            - secretRef:
                name: $SECRET
EOF
)"

kubectl rollout restart deployment/"$DEPLOY" --namespace "$NS"
kubectl rollout status deployment/"$DEPLOY" --namespace "$NS" --timeout=2m

green "── agentgateway has AWS creds ──"
echo
echo "smoke-test the Bedrock loop from inside the tenant namespace:"
cat <<'EOF'

  kubectl run -n tenants-blank curl --rm -it --image=curlimages/curl --restart=Never -- \
    curl -sX POST http://agentgateway.agentgateway.svc.cluster.local:8080/v1/messages \
         -H 'content-type: application/json' \
         -d '{"route":"blank-primary","messages":[{"role":"user","content":"ping"}],"max_tokens":16}'

EOF
echo "expected: an Anthropic message envelope with a Bedrock-generated response."
echo
yellow "the shared cred has whatever your laptop has — usually a lot. don't use this cluster for anything sensitive."
