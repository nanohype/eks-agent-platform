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

# ── 3. no doc may name a KMS key no component receives ────────────────────────
# `cmk-data` and `cmk-logs` named a two-key split that landing-zone never
# provisioned: `secrets` minted ONE key, and both `data_kms_key_arn` and
# `logs_kms_key_arn` resolved to it. The names were load-bearing in the worst
# way — SECURITY.md and ARCHITECTURE.md built an auditor-sees-logs-not-data
# posture on a separation that did not exist, and nothing failed, because prose
# has no compiler.
#
# The keys a component can actually receive are named by their VARIABLES
# (data_kms_key_arn, logs_kms_key_arn) or by what landing-zone aliases them —
# `<environment>-platform-secrets`, and `<environment>-platform-logs` where that
# environment sets `separate_logs_key`. Say one of those. An invented nickname
# is a claim about topology that nothing checks.
hits=$(grep -rnE '\bcmk-(data|logs)\b' \
  --exclude-dir='.git' --exclude-dir='node_modules' --exclude-dir='dist' \
  --exclude-dir='vendor' --exclude-dir='.terraform' \
  --exclude-dir='.terragrunt-cache' \
  . 2>/dev/null \
  | grep -v 'scripts/check-doc-contracts.sh' \
  || true)

if [ -n "$hits" ]; then
  echo "cmk-data / cmk-logs name a key no component receives:"
  echo "$hits"
  echo
  echo "landing-zone's secrets component mints ONE CMK (alias/<environment>-platform-secrets)"
  echo "and publishes it as both kms_key_arn and logs_kms_key_arn. An environment that sets"
  echo "separate_logs_key gets a second key, alias/<environment>-platform-logs, and the"
  echo "CloudWatch Logs + Bedrock grants MOVE onto it."
  echo
  echo "Name the variable (data_kms_key_arn / logs_kms_key_arn) or the real alias."
  fail=1
fi

# ── 4. a CUR 2.0 export CAN be filtered ───────────────────────────────────────
# "A Cost and Usage Report has no filter" justified account-scoping in ten places.
# It was true of legacy CUR — aws_cur_report_definition has no filter attribute —
# and false of the Data Export this org actually runs, whose QueryStatement takes
# `WHERE <column> OPERATOR <value>`
# (docs.aws.amazon.com/cur/latest/userguide/dataexports-data-query.html).
#
# The conclusion survived the correction; the reason did not. Account-scoping is
# right because nothing in the export identifies a cluster or a workload
# environment, so a per-environment copy is a duplicate rather than a view. Say
# that, because it is the part that stays true if AWS ever adds a filterable
# column — at which point the old sentence would have been both wrong AND
# load-bearing.
#
# What this gate can and cannot do: it catches the phrase coming back. It cannot
# catch the next unchecked claim about how an AWS service behaves, which is what
# the original defect was. That one is caught by reading the AWS docs before
# writing the sentence, and by nothing else.
hits=$(grep -rniE '(CUR|Cost and Usage Report)[^.]{0,40}has no filter' \
  --exclude-dir='.git' --exclude-dir='node_modules' --exclude-dir='dist' \
  --exclude-dir='vendor' --exclude-dir='.terraform' \
  --exclude-dir='.terragrunt-cache' \
  . 2>/dev/null \
  | grep -v 'scripts/check-doc-contracts.sh' \
  || true)

if [ -n "$hits" ]; then
  echo "a CUR 2.0 export can be filtered — its query statement takes a WHERE clause:"
  echo "$hits"
  echo
  echo "That claim is true of legacy CUR (aws_cur_report_definition) and false of the"
  echo "Data Export this org runs. Account-scoping is still right, for a reason that"
  echo "holds: nothing in the export identifies a cluster or workload environment, so a"
  echo "per-environment copy is a complete duplicate rather than a view of one share."
  fail=1
fi

# ── the cost-allocation-tag check is documented where it lives ──────────────
#
# cost-pipeline declares `check "the_cost_allocation_tags_are_active"`, which
# warns on every plan while either tag key is inactive. The requirement it warns
# about — activation is payer-level, account-global, and NOT retroactive, so
# every hour before it is permanently NULL — was documented only in
# bedrock-account's "Not here" section, a page whose whole job is to say what it
# does not own. An operator who applied cost-pipeline and read its README learned
# nothing about a warning they were about to see, or about a gap they would
# discover a month later on a partial-month report.
#
# So: the component that owns the check owns the explanation, and the two must
# name the same thing. A renamed check with a stale README is the same silent
# drift in miniature.
check_name=$(grep -oE 'check "[a-z_]+"' terraform/components/cost-pipeline/main.tf \
  | head -1 | sed 's/check "//; s/"//')

if [ -z "$check_name" ]; then
  echo "cost-pipeline/main.tf declares no check block; this gate cannot verify what it documents"
  fail=1
elif ! grep -q "$check_name" terraform/components/cost-pipeline/README.md; then
  echo "cost-pipeline declares check \"$check_name\" and its README does not name it."
  echo "The check warns on every plan. A reader who only has the README cannot tell"
  echo "whether the warning is expected or a defect."
  fail=1
fi

for key in 'resourceTags/PlatformId' 'iamPrincipal/PlatformId'; do
  if ! grep -q "$key" terraform/components/cost-pipeline/README.md; then
    echo "cost-pipeline/README.md does not mention $key."
    echo "Both prefixes must be activated in Cost Explorer or the column does not exist;"
    echo "either one alone returns a plausible number that is missing most of the spend."
    fail=1
  fi
done

if ! grep -qi 'not retroactive' terraform/components/cost-pipeline/README.md; then
  echo "cost-pipeline/README.md does not state that tag activation is not retroactive."
  echo "That is the property that makes the cost of missing it unrecoverable: every hour"
  echo "before activation is permanently NULL, and it reads as low spend rather than as a gap."
  fail=1
fi

if [ "$fail" -ne 0 ]; then
  exit 1
fi
echo "✓ doc contracts hold (agent-iam path + cluster-keyed SSM + KMS key names + CUR filterability + cost-tag activation)"
