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

# The tools this gate reasons with, asserted before any of them is used.
#
# Without this, an absent grep makes `scanned` EMPTY rather than zero, so the
# anti-vacuity floor below evaluates `[ "" -eq 0 ]`, which is an error and not a
# true condition — the floor is skipped, the search finds nothing, and the gate
# reports a clean tree it never read. A check against vacuity that is itself
# deleted by a missing tool is the same defect one level up.
#
# 3, not 1: nothing was checked, which is a different fact from the tree failing.
for _t in grep; do
  command -v "$_t" >/dev/null 2>&1 || {
    echo "$(basename "$0"): $_t is not on PATH; nothing was checked." >&2
    exit 3
  }
done

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
#
# Both spellings have to be caught. The metasyntactic `<env>` is the harmless
# one — a reader retypes it. The dangerous one is a real environment token,
# because it copy-pastes into a shell and returns ParameterNotFound against a
# path that never existed. Matching only the placeholder is a gate that fires
# on the form nobody runs. The terminator class is what keeps the real cluster
# names out of the alternation: `development-platform` continues past `dev`
# with a hyphen, so it does not match.
hits=$(grep -rnE '/eks-agent-platform/(<env>|dev|prod|stage|staging|test|qa|production|development|sandbox|uat)(/|$|\*|`|")' \
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
  echo "SSM contract is cluster-keyed — use the EKS cluster name, not an environment:"
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
# about — activation is payer-level and account-global, and while activating late
# can be repaired by a twelve-month backfill, a resource that ran untagged cannot
# be, because backfill applies an activation status and invents no tag values —
# was documented only in
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

# The README has to name the column and key prefix the export actually delivers, and
# has to keep naming the spellings that do NOT work.
#
# This previously required the README to mention `resourceTags/PlatformId` and
# `iamPrincipal/PlatformId` — pinning the doc to a claim about the data that was
# never checked against the data. Both prefixes are absent from the delivered export
# and read NULL rather than failing, so the gate held the README faithful to a query
# that returned zero for every platform. A doc contract is only as good as the fact
# it encodes.
for key in 'resource_tags' 'user_PlatformId'; do
  if ! grep -q "$key" terraform/components/cost-pipeline/README.md; then
    echo "cost-pipeline/README.md does not mention $key."
    echo "That is the column and key prefix the export delivers (cur-export-schema.txt);"
    echo "a reader who takes the AWS dictionary at face value writes a query that reads"
    echo "NULL for every row and reports zero spend without failing."
    fail=1
  fi
done

# And it has to keep warning about the spellings that silently return nothing.
if ! grep -q 'resourceTags/' terraform/components/cost-pipeline/README.md; then
  echo "cost-pipeline/README.md no longer names resourceTags/ as a spelling that does not work."
  echo "It is the one the AWS documentation leads you to, it is absent from this export,"
  echo "and it fails by returning NULL rather than by erroring. Dropping the warning is"
  echo "how it gets written again."
  fail=1
fi

# The recorded export schema is what every CUR claim in this component is checked
# against, so it has to exist and has to carry its provenance.
if [ ! -f terraform/components/cost-pipeline/cur-export-schema.txt ]; then
  echo "cost-pipeline/cur-export-schema.txt is missing — every CUR assertion in this"
  echo "component is checked against it, and without it they check each other again."
  fail=1
elif ! grep -qi 'PROVENANCE' terraform/components/cost-pipeline/cur-export-schema.txt; then
  echo "cur-export-schema.txt records no provenance. It is only worth more than the AWS"
  echo "dictionary because it was read off the live export; without saying how, it is"
  echo "another restatement."
  fail=1
fi

if ! grep -qi 'backfill' terraform/components/cost-pipeline/README.md; then
  echo "cost-pipeline/README.md does not state which half of this is recoverable."
  echo "Late activation is: a management account can backfill twelve months. A resource"
  echo "that ran untagged is not, because backfill applies an activation status to history"
  echo "and does not invent tag values. Saying only 'not retroactive' points the urgency at"
  echo "the switch when it belongs to the stamp."
  fail=1
fi

# ── the egress reference names the namespaces the operator actually names ────
#
# charts/bedrock-egress publishes a NetworkPolicy template as a readable
# reference. The operator does not read it — it builds the real policies in Go —
# so the two can diverge with nothing failing, and they did: the reference sent
# OTLP egress to a namespace called `observability`, which nothing in the org
# creates, while every policy the operator applies names `monitoring`.
#
# A reference that is wrong is worse than no reference: it is the thing someone
# copies when writing a tenant's own policy, and a rule naming a namespace that
# does not exist allows egress to nowhere — silently, since a NetworkPolicy that
# matches no peer is indistinguishable from one whose peer is simply down.
# `|| true` on both: the script runs under `set -euo pipefail`, so a grep that
# matches nothing would abort here — exiting 1 with NO message, which is the same
# silent-absence failure this gate exists to catch. Let the variable come back
# empty and let the explicit branches below say which side went missing.
collector_ns=$(grep -oE 'collectorNamespace = "[a-z0-9-]+"' \
  operators/internal/controller/cilium_egress.go 2>/dev/null | head -1 \
  | sed 's/.*"\(.*\)"/\1/' || true)
chart_ns=$(grep -oE '^[[:space:]]*observabilityNamespace: [a-z0-9-]+' \
  charts/bedrock-egress/values.yaml 2>/dev/null | head -1 | awk '{print $2}' || true)

if [ -z "$collector_ns" ]; then
  echo "cilium_egress.go declares no collectorNamespace; this gate cannot compare anything"
  fail=1
elif [ -z "$chart_ns" ]; then
  echo "charts/bedrock-egress/values.yaml declares no observabilityNamespace"
  fail=1
elif [ "$collector_ns" != "$chart_ns" ]; then
  echo "the egress reference names namespace '$chart_ns'; the operator applies '$collector_ns'."
  echo "The operator's value is the one that governs — it builds the real policy in Go."
  echo "A reference naming a different namespace is what someone copies into a tenant"
  echo "policy, and a rule naming a namespace nothing creates allows egress to nowhere."
  fail=1
fi

# The same reference, one field further in. The namespace above is templated
# from values.yaml and compared; the OTLP ports beside it are YAML literals that
# nothing reads, and the architecture diagram states the destination a third
# time. All three are the address the operator opens in Go, so all three are
# answerable to the same two constants.
#
# A wrong port here fails the way a wrong namespace does — silently. A rule
# opening 4319 to a collector listening on 4317 is a valid NetworkPolicy, admits
# nothing, and looks exactly like a collector that is simply quiet.
go_ports=$(grep -oE 'otlp(GRPC|HTTP)Port = [0-9]+' \
  operators/internal/controller/cilium_egress.go 2>/dev/null \
  | grep -oE '[0-9]+' | sort -n | tr '\n' ' ' | sed 's/ $//' || true)

# Only the ports inside the rule that names the observability namespace. The
# reference opens 8080 to the gateway a few lines above, and a gate that
# collected every port in the file would compare that against the OTLP pair and
# fail on a correct reference.
ref_ports=$(awk '
  /observabilityNamespace/ { inrule = 1; next }
  inrule && /^[[:space:]]*- to:/ { inrule = 0 }
  inrule && /^[[:space:]]*- port:/ { gsub(/[^0-9]/, "", $0); print }
' charts/bedrock-egress/templates/networkpolicy-template.yaml 2>/dev/null \
  | sort -n | tr '\n' ' ' | sed 's/ $//' || true)

if [ -z "$go_ports" ]; then
  echo "cilium_egress.go declares no otlpGRPCPort/otlpHTTPPort; this gate cannot compare anything"
  fail=1
elif [ -z "$ref_ports" ]; then
  echo "charts/bedrock-egress/templates/networkpolicy-template.yaml opens no port toward the"
  echo "observability namespace. The reference someone copies would allow egress to the"
  echo "collector on no port at all, which denies every export while reading as a rule."
  fail=1
elif [ "$go_ports" != "$ref_ports" ]; then
  echo "the egress reference opens OTLP ports '$ref_ports'; the operator opens '$go_ports'."
  echo "The operator's values govern — it builds the real policy in Go from those constants,"
  echo "and the same two are the port in the OTEL_EXPORTER_OTLP_ENDPOINT it stamps on pods."
  echo "A reference opening a different port is what someone copies into a tenant policy, and"
  echo "a rule opening a port nothing listens on denies every export while looking correct."
  fail=1
fi

# The architecture diagram names the destination in prose, where nothing reads
# it. A diagram naming localhost describes the address an OTel SDK exports to
# when it is given no endpoint at all — the failure the operator stamps
# OTEL_EXPORTER_OTLP_ENDPOINT to prevent — so it is the one wrong answer a
# reader is most likely to act on.
arch_line=$(grep -oE 'agent pod → OTLP \([^)]*\)' ARCHITECTURE.md 2>/dev/null | head -1 || true)
grpc_port=$(grep -oE 'otlpGRPCPort = [0-9]+' \
  operators/internal/controller/cilium_egress.go 2>/dev/null | grep -oE '[0-9]+' || true)

if [ -z "$arch_line" ]; then
  echo "ARCHITECTURE.md no longer states the OTLP destination; this gate cannot compare it"
  fail=1
elif [ -z "$collector_ns" ] || [ -z "$grpc_port" ]; then
  : # already reported above
elif ! printf '%s' "$arch_line" | grep -q "$collector_ns"; then
  echo "ARCHITECTURE.md describes OTLP going to '$arch_line'; the operator stamps the"
  echo "'$collector_ns' namespace. A diagram naming a different destination is what a reader"
  echo "trusts when the series do not arrive."
  fail=1
elif ! printf '%s' "$arch_line" | grep -q "$grpc_port"; then
  echo "ARCHITECTURE.md describes OTLP going to '$arch_line'; the operator stamps port $grpc_port."
  fail=1
fi

# THE OPPOSITE DIRECTION from the floors in no-placeholders.sh, and the reason
# the default cannot be copied between them: here NON-zero is the failing value,
# so an undetermined counter must default to non-zero. Defaulting it to 0 would
# write the defect into the fix — 0 reads as "no findings", which is exactly what
# a counter that was never set most resembles.
fail=${fail:-1}
if [ "$fail" -ne 0 ]; then
  exit 1
fi
echo "✓ doc contracts hold (agent-iam path + cluster-keyed SSM + KMS key names + CUR filterability + cost-tag activation + OTLP destination)"
