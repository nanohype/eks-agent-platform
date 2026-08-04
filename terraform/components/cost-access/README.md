# components/cost-access

One cluster's access to the account's cost pipeline — the grant, the workgroup, and the three
handles the operator reads.

Applied **once per cluster**, from `live/<environment>-<clusterBase>/cost-access/`. The pipeline
itself is account-scoped ([`cost-pipeline`](../cost-pipeline/), applied from `live/org/`): the CUR 2.0
export it reads covers the whole account, and no column in that export identifies a cluster, so a
per-environment copy would be a complete duplicate rather than a view of anything. Three things about
reaching it are nonetheless per-cluster, and this component is all three:

- **The operator's grant.** It attaches to *one* cluster's operator role, discovered from that
  cluster's `agent-iam` contract. N clusters need N policies and N attachments.
- **The SSM handles.** Everything the operator reads from SSM is one recursive `GetParametersByPath`
  sweep of `/eks-agent-platform/<cluster>/`, so a parameter published under the account prefix is
  invisible to it. The account's handles are republished here under the keys the operator already
  reads.
- **The Athena workgroup.** A workgroup decides where results land and how they are encrypted, and
  it decides that for every caller in it — `enforce_workgroup_configuration` means a
  client-supplied `ResultConfiguration` is ignored outright. One workgroup for the account would be
  one results prefix for every cluster with no way to vary it. Each cluster gets its own prefix by
  getting its own workgroup; there is no other lever.

**This component creates no cost storage.** No bucket, no database, no table, no retention. The
workgroup is a query-execution context bound to one cluster's identity, pointed at the account's
bucket. If you are editing this to change the pipeline, you are in the wrong component.

## What it does not buy

CUR access remains **account-wide**, and it is in fact broader than "the export": `CurRead` grants
`GetObject` and `ListBucket` on the entire CUR bucket, not on the export's prefix inside it. The
bucket holds only that export today, so the reach is the same in practice — but anything else written
there becomes readable by every cluster's operator, which is worth knowing before putting a second
cost artifact in it.

What this component scopes is where a cluster *writes its own query output*. It is not billing-data
isolation, and reading it as such would overstate the boundary.

## The grant

One IAM policy attached to this cluster's operator role:

| Sid                  | What it permits                                                                                    |
| -------------------- | -------------------------------------------------------------------------------------------------- |
| `AthenaQuery`        | run and read queries in **this cluster's** workgroup only                                          |
| `GlueRead`           | read the account's Glue database and its tables — Athena resolves the schema under the caller      |
| `AthenaResultObjects`| read/write/abort its own results under `results/<cluster_name>/`, including the multipart actions   |
| `AthenaResultsBucket`| bucket-level list on the results bucket, left unconditioned on purpose — the prefixes Athena lists while running a query are not a documented contract, so a condition here risks denying every query to buy a narrowing `AthenaResultObjects` already provides |
| `CurRead`            | read the account CUR **bucket** (account-wide, see above)                                          |
| `CostDataKMS`        | `Decrypt`, `GenerateDataKey`, `DescribeKey` on the account's cost key                              |
| `BedrockMetrics`     | `GetMetricStatistics`, `GetMetricData`, `ListMetrics` on `*` — **not** namespace-conditioned. The operator reads only `agents/Bedrock`; the grant reaches every namespace in the account and region |

`GenerateDataKey` is not optional. The workgroup enforces SSE-KMS results, so `Decrypt` alone leaves
every query failing at the **write** step, which the reconciler records as unreadable spend rather
than as access denied. The budget is then never evaluated and the kill switch cannot fire — but it is
not silent: the BudgetPolicy goes `BudgetReconciled=False` / `ReconcileFailed`, and
`agents_budget_spend_unreadable_total` drives the critical `BudgetSpendUnreadable` alert, which was
added for precisely this shape (a policy whose every tick has failed since it was created, so there
is no `lastReconciled` series for a lag alert to key on).

## The drift check

The account pipeline's cost publisher reads the `PlatformId` tag off invoking roles, and its grant
is scoped to one IAM path. That path is an account-wide constant in landing-zone but is published by
`agent-iam` *per cluster*, so `cost-pipeline` takes it as an input and has nothing to verify it
against. This component can see both sides, and does.

A `lifecycle.precondition` compares the path the account scoped to
(`/eks-agent-platform/org/cost-pipeline/tenant_iam_path`) against the path this cluster's operator
actually mints under (`/eks-agent-platform/<cluster>/agent-iam/tenant_iam_path`), and **fails the
apply** if they disagree. Both are `trimspace`d and normalized to a trailing slash first — the value
is used as a prefix, so `/x/tenants` and `/x/tenants/` mean the same thing to a reader and different
things to IAM, and an SSM value that picked up a newline in CI plumbing is the same path to IAM and
a different string here. A gate that cries wolf on a correctly built cluster is a gate that gets
bypassed.

It fails rather than warns because the consequence is a wrong number, not an error: if the paths
drift, every invocation from this cluster attributes to `unknown` and every budget here reads low.
The failure names the fix — set cost-pipeline's `tenant_iam_path` to match agent-iam's
`tenant_role_path` and re-apply `live/org/cost-pipeline` before this leaf.

**It asks once; it does not watch.** A `lifecycle.precondition` is evaluated only when this leaf
runs. Drift introduced afterwards — someone re-applies `live/org/cost-pipeline` with a different
path — is caught by nothing until a human re-applies `cost-access`. The gate protects the apply, not
the steady state, and the failure it guards produces no error of its own, so nothing else will
surface it in between.

## Inputs

| Variable       | Description                                                                                              |
| -------------- | ---------------------------------------------------------------------------------------------------------- |
| `environment`  | `development`, `staging` or `production` — validated. The account pipeline uses `org`; this is its per-cluster counterpart |
| `cluster_name` | full EKS cluster name (`<environment>-<clusterBase>`); keys both the `agent-iam` contract read and the SSM subtree written |
| `region`       | composes the Glue catalog and table ARNs, and binds the operator's KMS grant to S3 in this region        |
| `tags`         | common tags                                                                                              |

There is deliberately **no `data_kms_key_arn`**. The operator needs `Decrypt` and `GenerateDataKey`
on the key the account pipeline encrypts with, but which key that is is not this component's to
decide — it is read from `/eks-agent-platform/org/cost-pipeline/data_kms_key_arn`. As an input it
was a second place the same value was set with nothing comparing the two, and a grant scoped to a
key the workgroup does not use fails on the write.

## Reads

From this cluster's `agent-iam` contract: `operator_role_name`, `tenant_iam_path`.

From the account pipeline at `/eks-agent-platform/org/cost-pipeline/`: `cur_bucket_arn`,
`athena_results_bucket`, `athena_results_bucket_arn`, `athena_database`, `athena_database_arn`,
`cur_table_name`, `tenant_iam_path`, `data_kms_key_arn`.

The account publishes more than that — the estimate table, the reconciliation view, and the account's
own Athena workgroup. Those are the account's own query surface, read by whoever queries the account,
and are not part of the operator's configuration, so this component does not carry them across. The
workgroup especially must not be: republishing it would hand the operator a workgroup its policy does
not cover.

## Publishes

Exactly three keys, under `/eks-agent-platform/<cluster_name>/cost-pipeline/`:

- `athena_workgroup` — **this cluster's** workgroup, not the account's
- `athena_database` — passed through from the account
- `cur_table_name` — passed through from the account

Three because three is what the operator's `Config.assign` decodes. A republished key with no decode
case is not harmless: it reads as configuration the operator honours, so the next person to change
the pipeline changes it here too and watches for an effect that cannot arrive — and it is a
per-cluster resource that has to be created, tagged and destroyed for nobody.

The workgroup key and the `AthenaQuery` grant ship together on purpose. Publishing the account
workgroup while the policy permits only this one is AccessDenied on every tick; creating this one
while the handle still names the account's hands the operator a workgroup its policy does not cover.
Either half alone produces stale budgets and a kill switch that never fires, with nothing red.

## Consumed by

The operator, and only the operator. Two things about how it consumes them decide whether an apply
here is visible:

- **It reads once, at startup.** `operatorconfig.Load` runs a single sweep from `main.go`; there is
  no watch and no refresh. Changing `athena_workgroup` — the one thing this component exists to make
  per-cluster — does not reach a running operator until the pod restarts.
- **A missing leaf does not stop it.** `Config.Validate` requires the operator role, the tenant IAM
  path, the tenant baseline policy, the permissions boundary and the artifacts bucket. The cost keys
  are deliberately not in that set: they "degrade per-reconciler instead." So a cluster where this
  component never applied runs an operator that starts clean, reports healthy, and has no budget
  path at all. Nothing in the operator's startup will tell you.

That is the reason the workgroup key and the `AthenaQuery` grant ship together, one layer up from
the reason given above: neither half announces its own absence.

## Outputs

`operator_cost_policy_arn`, `athena_database`.

## Apply order

[`cost-pipeline`](../cost-pipeline/) (from `live/org/`) before this, and landing-zone's `agent-iam`
before both. Expressed through SSM rather than terragrunt `dependency` blocks: terragrunt resolves
dependencies at **parse** time, so every per-cluster leaf would fail `init` — not `apply` — whenever
the account state was absent, and no `TF_VAR` gets you out of that.

**Teardown is the exact reverse, and it is not forgiving.** The eight account reads above are
unconditional `data.aws_ssm_parameter` blocks with no fallback, so once `live/org/cost-pipeline` is
destroyed, every cluster's `cost-access` can no longer plan even its own destroy — the data sources
fail to resolve and the only way out is `state rm`. Destroy every `cost-access` leaf first, then the
account root.

Worth knowing before running one on a live cluster: destroying this leaf detaches a policy from a
role landing-zone owns, deletes the workgroup the running operator is configured to query, and
removes the three SSM keys — while the operator keeps running against all three.
