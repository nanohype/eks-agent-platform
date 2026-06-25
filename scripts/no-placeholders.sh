#!/usr/bin/env bash
# Zero-placeholder gate — fail if an unfilled "fill-me" sentinel appears in
# applied deploy configuration (Helm values, kustomize, ArgoCD manifests,
# terragrunt/tofu inputs). Every per-environment value must render from its
# source of truth, never sit in the repo as a placeholder waiting to be
# hand-edited before deploy.
#
# NOT sentinels (intentional public-repo conventions, deliberately unmatched):
#   - example.com domains
#   - the 111111111111 / 222222222222 fake AWS account ids
#   - Azure subscription/tenant GUID placeholders (xxxxxxxx-…)
# Excluded by path: docs (prose, not applied config — *.md isn't scanned),
# examples, test fixtures, vendored copies, and the opt-in mcp-tunnel addon
# (user-supplied Cloudflare IDs, off by default).
set -uo pipefail

SENTINELS='PLACEHOLDER|REPLACE_ME|REPLACEME|CHANGEME|CHANGE_ME|FILL_ME|FILLME|TODO_FILL|TO_BE_FILLED|<FILL|<YOUR_|<ACCOUNT_ID>|<FLEET_ACCOUNT'

hits=$(grep -rnE "$SENTINELS" . \
  --include='*.yaml' --include='*.yml' --include='*.tf' --include='*.hcl' \
  --include='*.tfvars' --include='*.json' \
  --exclude='*.example' \
  --exclude-dir='.git' --exclude-dir='.terraform' --exclude-dir='.terragrunt-cache' \
  --exclude-dir='node_modules' --exclude-dir='examples' --exclude-dir='testdata' \
  --exclude-dir='test' --exclude-dir='mcp-tunnel' --exclude-dir='vendor' \
  2>/dev/null)

if [ -n "$hits" ]; then
  echo "Unfilled placeholder sentinel(s) found in deploy config:"
  echo "$hits"
  echo
  echo "Deploy config must render from its source of truth, not carry a fill-me"
  echo "placeholder. If a path is a legitimate opt-in template, add it to the"
  echo "exclude list in scripts/no-placeholders.sh."
  exit 1
fi
echo "✓ no placeholder sentinels in deploy config"
