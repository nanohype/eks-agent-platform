# components/bedrock-account

The account's Bedrock model-invocation record — every model call, written once, to one place.

Applied **once per account per region**, from `live/org/bedrock-account/`. Not a preference:
`aws_bedrock_model_invocation_logging_configuration` has no name and no identifier, so AWS holds
exactly one of it per account per region. A per-environment copy would not be three configurations;
it would be three roots fighting over one, where the last apply silently repoints the account's
logging at its own bucket and **any** environment's teardown deletes it for all of them. Both
applies are green. Nothing goes red. The invocation record is what every budget decision reads, so
that is the org's own failure class sitting on the flagship feature — which is why this component
exists to be the single owner.

It takes no `cluster_name`, and `environment` is validated to `org`.

## Pieces

- **Invocations bucket** — the logging destination, named for the account and region because S3's
  namespace is global while Bedrock logging is per-region. Object Lock enabled at create (immutable
  afterwards), versioned, SSE-KMS on the key passed as `logs_kms_key_arn` with a bucket key, and a
  lifecycle that transitions to STANDARD_IA at 90 days, GLACIER at 365, and expires one day past the
  lock retention. That expiry is what bounds the bucket once the lock lapses — transitions move the
  cost down a tier and never end it. Noncurrent versions expire after a single day: versioning is
  here because Object Lock requires it, not to keep history.
- **Access-logs bucket** — receives server-access logs from the invocations bucket, its only writer.
  Encrypted **SSE-S3 (AES256), not the CMK** — S3 server-access logging to a bucket destination does
  not support SSE-KMS, so this is a constraint rather than a choice, and it is the one exception to
  "log storage is on a customer-managed key" in this component. It lives here rather than next door
  because a per-environment owner would either strand a bucket with no producer or make the
  account-scoped log write into one environment's sink, which is the same defect one layer down.
- **CloudWatch log group** — `/aws/bedrock/<prefix>/invocations`, KMS-encrypted, the CloudWatch half
  of the same single configuration.
- **The logging role** — assumed by `bedrock.amazonaws.com`, conditioned on `aws:SourceAccount`.
  Bedrock takes a role for the **CloudWatch** destination only: `role_arn` lives inside
  `cloudwatch_config` and `s3_config` has no equivalent. So what this role actually authorizes is
  `CreateLogStream` + `PutLogEvents` on the log group. It also carries write-side S3 and KMS grants,
  which serve the large-data spill path this configuration does not enable — ordinary S3 delivery is
  authorized by the bucket policy instead, which is why the configuration `depends_on` it.
- **The logging configuration itself** — with an explicit `depends_on` the bucket policy, because
  Bedrock validates that policy synchronously at create and tofu will otherwise race the two.

## Applying this adopts whatever the account already has

`PutModelInvocationLoggingConfiguration` takes no identifier. It overwrites whatever the account was
already configured with and reports success either way — so on an account that already has invocation
logging, the first apply takes ownership of a resource it did not create, records it in state as its
own, and will later delete it. The previous destination stops receiving records and nothing goes red
anywhere.

Terraform cannot gate this. The pinned provider ships data sources for models, inference profiles and
model-access agreements, but none for the logging configuration, so a `precondition` here would have
nothing to read — it would be decoration. The check lives one layer up instead, where the account is
readable: the installer's preflight calls `GetModelInvocationLoggingConfiguration` and refuses the run
when the account carries a configuration that is not this one's.

**The contract before first apply** is that the account has either no invocation-logging configuration
or one already pointing at `<prefix>-invocations`. Applying over a third party's is a silent takeover,
and what it displaces cannot be recovered from here.

## Object Lock is the retention decision, and it is the one to read carefully

There is deliberately **no `force_destroy_buckets` lever** here. Only two components in this tree
hold S3 buckets at all — this one and [`cost-pipeline`](../cost-pipeline/) — and both are
account-scoped, so neither has a workload environment whose teardown posture could decide the
question. cost-pipeline settles it with an explicit flag; this component settles it with the lock
mode, because a lock is a retention statement and a teardown flag must not be able to talk over one.
So what governs deletion here is the mode:

| `object_lock_mode`      | `force_destroy` | What a `destroy` of this root does                                                                                                        |
| ----------------------- | --------------- | ----------------------------------------------------------------------------------------------------------------------------------------- |
| `GOVERNANCE` (default)  | `true`          | Succeeds **if** the caller holds `s3:BypassGovernanceRetention`, and takes the whole account's invocation history with it — every environment's, not one environment's. Neither repo grants that action by name, but `AdministratorAccess` implies it, and landing-zone attaches that to the break-glass role in every workload environment and to the `Admin` SSO permission set. Read this row as "an administrator can delete the record", not as a barrier. |
| `COMPLIANCE`            | `false`         | Blocked outright. No principal, including the account root, can shorten the retention — the bucket cannot be destroyed until every object's retain-until date has passed. |

Both buckets take that `force_destroy` value, and under `COMPLIANCE` they resist deletion for
different reasons. The invocations bucket is held by the lock, which no permission clears. The
access-logs bucket carries no lock at all — it simply refuses a non-empty delete, which an operator
can resolve by emptying it. Only the invocation record is genuinely immutable; the table is about
that bucket.

**"Blocked" does not mean "nothing happened."** Terraform destroys dependents before the things they
depend on, and nothing here carries `prevent_destroy`. The logging configuration references the log
group, the role and the bucket, so a `destroy` that is going to fail on the bucket has already
deleted the account's invocation-logging configuration, its log group and its delivery role by the
time it reaches it. What is left is an account logging nothing — for every environment, and for any
other workload sharing it — a `cost-pipeline` subscription pointing at a log group that no longer
exists, and an orphaned bucket with the rest of the state gone. The same holds under `GOVERNANCE`
when the caller lacks the bypass permission. A failed destroy here is more destructive than no
attempt, so if the record must be kept, do not start one.

**The two lock variables are first-apply decisions, not levers.** S3 writes lock information into
each object version's metadata at PUT time, and an existing version stays locked according to the
configuration it was written with. Changing `object_lock_mode` afterwards does not retroactively
lock what is already there, and lowering `object_lock_retention_days` releases nothing. That second
variable also feeds two places — the lock default and the lifecycle expiry
(`object_lock_retention_days + 1`) — which agree only while it never changes: lower it and the
expiry lands on a date the existing locks will not permit, raise it and objects written earlier
expire sooner than this page claims.

`COMPLIANCE` is not a stricter default to reach for; it is a commitment with no exit. Choose it when
being unable to delete the record for `object_lock_retention_days` is the point.

## Inputs

| Variable                     | Description                                                                                     |
| ---------------------------- | ------------------------------------------------------------------------------------------------- |
| `environment`                | always `org` — validated, because a workload token would mean several roots claiming one configuration |
| `logs_kms_key_arn`           | **Required, no default** — the only value this component cannot supply itself. Passed by the orchestrator as a `TF_VAR`, not set in the terragrunt leaf. The landing-zone CMK that encrypts the invocations bucket and the log group (the access-logs bucket is AES256; see Pieces). It resolves to the same key the tree passes as `data_kms_key_arn` unless that environment sets landing-zone's `separate_logs_key`, which moves the log path onto its own CMK |
| `log_retention_days`         | CloudWatch retention on the invocation log group (default 365)                                  |
| `object_lock_mode`           | `GOVERNANCE` (default) or `COMPLIANCE` — see above                                              |
| `object_lock_retention_days` | WORM retention on each object (default 365); bucket expiry is this plus one day                 |
| `access_logs_retention_days` | retention on S3 server-access logs (default 365)                                                |
| `tags`                       | common tags                                                                                     |

There is no `cluster_name` and no region variable. The region comes from the provider, and a cluster
token would be a claim this component cannot keep.

## Outputs

`invocation_bucket_arn`, `invocation_bucket_name`, `invocation_log_group_name`,
`invocation_log_group_arn`, `bedrock_logging_role_arn`, `access_logs_bucket_name`.

Exactly one is published to SSM, at `/eks-agent-platform/org/bedrock-account/invocation_log_group`:
the log group's **name**.

## Consumed by

- [`cost-pipeline`](../cost-pipeline/) attaches its invocation-cost publisher to that log group. It
  is account-scoped itself, so it reads this contract directly rather than through a per-cluster
  republish. It needs the log group's *name* and composes the ARN it grants on from it, which is why
  the ARNs above are module outputs and not published parameters — a published ARN nobody reads is a
  contract with one side.

The **operator is not a consumer**. It has no CloudWatch Logs client and never addresses the
invocation bucket: the spend it acts on arrives as a CloudWatch metric the cost publisher emits and
as CUR rows it reads through Athena. Its configuration sweep is rooted at
`/eks-agent-platform/<cluster>/`, so it cannot see anything under `org/` in any case.

The pairing with the publisher is not optional, and the reason is worth stating: a log group accepts
five subscription filters, so per-environment publishers would all attach cleanly and each would
process every record into the account-global `agents/Bedrock` namespace the budget reconciler reads
with `Stat=Sum`. N copies means N times the real number, from N green applies.

## Not here

- **The baseline Guardrail** — [`bedrock`](../bedrock/), per cluster. A guardrail is a named
  resource, so an account holds many and each cluster gets its own. That asymmetry is the whole
  reason there are two Bedrock components.
- **The `PlatformId` cost-allocation tag activation** — account-global, but part of the cost
  pipeline, and it cannot happen in the apply that first stamps the key (AWS takes up to 24h to list
  a newly observed one). Holding it here would gate the account's only Bedrock audit trail behind a
  billing clock. It lives in [`cost-pipeline`](../cost-pipeline/) as an assertion.
- **Per-tenant Bedrock access policies** — created by the operator at reconcile time, bound to each
  tenant's IAM role, with model-ARN scoping.

## Apply order

`bedrock-account` before [`cost-pipeline`](../cost-pipeline/), because the publisher subscribes to a
log group this component creates. The dependency is expressed through SSM rather than a terragrunt
`dependency` block: terragrunt resolves dependencies at **parse** time, so a missing account state
would fail `init` rather than `apply`, and no `TF_VAR` gets you out of that.
