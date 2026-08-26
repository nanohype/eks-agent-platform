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

# The tools this gate reasons with, asserted before any of them is used.
#
# Without this, an absent grep makes `scanned` EMPTY rather than zero, so the
# anti-vacuity floor below evaluates `[ "" -eq 0 ]`, which is an error and not a
# true condition — the floor is skipped, the search finds nothing, and the gate
# reports a clean tree it never read. A check against vacuity that is itself
# deleted by a missing tool is the same defect one level up.
#
# 3, not 1: nothing was checked, which is a different fact from the tree failing.
for _t in grep wc tr; do
  command -v "$_t" >/dev/null 2>&1 || {
    echo "$(basename "$0"): $_t is not on PATH; nothing was checked." >&2
    exit 3
  }
done

SENTINELS='PLACEHOLDER|REPLACE_ME|REPLACEME|CHANGEME|CHANGE_ME|FILL_ME|FILLME|TODO_FILL|TO_BE_FILLED|<FILL|<YOUR_|<ACCOUNT_ID>|<FLEET_ACCOUNT'

SCAN_ARGS=(
  --include='*.yaml' --include='*.yml' --include='*.tf' --include='*.hcl'
  --include='*.tfvars' --include='*.json'
  --exclude='*.example'
  --exclude-dir='.git' --exclude-dir='.terraform' --exclude-dir='.terragrunt-cache'
  --exclude-dir='node_modules' --exclude-dir='examples' --exclude-dir='testdata'
  --exclude-dir='test' --exclude-dir='mcp-tunnel' --exclude-dir='vendor'
)

# Count the corpus before searching it. Finding no sentinel and searching no
# files produce the same silence, so without this the gate reports success from
# the wrong working directory, or if the include patterns stop matching the
# extensions deploy config is written in. An absence check needs a floor, or it
# is satisfied by having looked nowhere.
scanned=$(grep -rl '' . "${SCAN_ARGS[@]}" 2>/dev/null | wc -l | tr -d ' ')
if [ "$scanned" -eq 0 ]; then
  echo "No deploy-config files matched under $PWD." >&2
  echo "Run this from the repository root. A pass here would mean the scan found" >&2
  echo "nothing to read, not that the config is clean." >&2
  exit 2
fi

# THE UNCONDITIONAL FLOOR, and deliberately not a count. Run against a tree
# holding only the gate scripts, this check found two deploy-config files that
# were ITS OWN and reported success. A count floor is satisfied by the gate's own
# directory however high the number is set; requiring the corpus to contain
# something OUTSIDE scripts/ cannot be met that way.
outside=$(grep -rl '' . "${SCAN_ARGS[@]}" 2>/dev/null | grep -cv '^\./scripts/')
if [ "$outside" -eq 0 ]; then
  echo "Every one of the $scanned file(s) scanned lives under scripts/." >&2
  echo "A tree holding only the gates satisfies a count floor with the gates' own" >&2
  echo "files, so a pass here would say nothing about the repository." >&2
  exit 2
fi

hits=$(grep -rnE "$SENTINELS" . "${SCAN_ARGS[@]}" 2>/dev/null)

if [ -n "$hits" ]; then
  echo "Unfilled placeholder sentinel(s) found in deploy config:"
  echo "$hits"
  echo
  echo "Deploy config must render from its source of truth, not carry a fill-me"
  echo "placeholder. If a path is a legitimate opt-in template, add it to the"
  echo "exclude list in scripts/no-placeholders.sh."
  exit 1
fi
echo "✓ no placeholder sentinels in $scanned deploy-config files"
