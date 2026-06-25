# ADR 0003 — Threat model + operator IAM blast radius

## Status

Accepted (2026-05-15).

## Context

This platform is a multi-tenant control plane that hands out IAM-backed access to Bedrock. The operator is the _only_ component in the system with `iam:*` permissions, and that scope is what defines the platform's security posture. The trust boundary and assumed adversaries are written down here so reconciler design choices can be evaluated against this contract.

## Decision

This ADR captures (1) the operator's IAM blast radius — what it can do, what it cannot, what happens if it's compromised — and (2) a STRIDE-style threat enumeration with mitigations.

## Operator IAM blast radius

The operator pod assumes the IRSA role provisioned by `terraform/components/agent-iam` (`<env>-<cluster>-eks-agent-platform-operator`). The full permission set is the union of policies attached by **three** Terraform components — `agent-iam` owns the role itself plus the IAM/KMS/S3/Bedrock-introspection grants; `cost-pipeline` attaches the Athena/Glue/CloudWatch policy via `aws_iam_role_policy_attachment.operator_cost`; `kill-switch` grants `events:PutEvents` on the breach bus via the bus's resource policy. The "Provisioned in" column below names the component that grants each row.

| Permission                                                                                                                                                                                                               | Scope                                                      | Provisioned in                                                                                                               | Why                                                               | Risk if compromised                                                                                                                                                                                                                                                  |
| ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | ---------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `iam:CreateRole`, `DeleteRole`, `GetRole`, `ListRolePolicies`, `ListAttachedRolePolicies`, `PutRolePolicy`, `DeleteRolePolicy`, `AttachRolePolicy`, `DetachRolePolicy`, `TagRole`, `UntagRole`, `UpdateAssumeRolePolicy` | `arn:aws:iam::<acct>:role/eks-agent-platform/tenants/*`    | `agent-iam` (`aws_iam_role_policy.operator_iam`)                                                                             | Reconcile per-tenant roles                                        | Operator can mint arbitrary roles under that path, attach any policy. **Cannot** mutate roles outside `/eks-agent-platform/tenants/`, so it can't elevate to admin paths.                                                                                            |
| `iam:CreatePolicy`, `CreatePolicyVersion`, `DeletePolicy`, `DeletePolicyVersion`, `GetPolicy`, `GetPolicyVersion`, `ListPolicyVersions`, `ListEntitiesForPolicy`                                                         | `arn:aws:iam::<acct>:policy/eks-agent-platform/tenants/*`  | `agent-iam` (`aws_iam_role_policy.operator_iam`)                                                                             | Create per-tenant Bedrock-access policies                         | Same blast radius as roles — confined to the IAM path.                                                                                                                                                                                                               |
| `iam:PassRole`                                                                                                                                                                                                           | Same path, `iam:PassedToService` = `bedrock.amazonaws.com` | `agent-iam` (`aws_iam_role_policy.operator_iam`)                                                                             | Hand tenant roles to Bedrock for cross-region inference profiles  | Constrained by the service condition; cannot pass roles to other services.                                                                                                                                                                                           |
| `kms:CreateGrant`, `RevokeGrant`, `ListGrants`, `DescribeKey`                                                                                                                                                            | `cmk-data` ARN, `kms:GrantIsForAWSResource = true`         | `agent-iam` (`aws_iam_role_policy.operator_kms`)                                                                             | Authorize tenant access to S3/Cosmos/etc. backed by cmk-data      | Operator can grant decrypt access to its tenant roles. **Cannot** decrypt content directly. The `kms:GrantIsForAWSResource = true` condition forbids freeform grants.                                                                                                |
| `s3:GetBucketPolicy`, `s3:PutBucketPolicy`                                                                                                                                                                               | model-artifacts bucket ARN                                 | `agent-iam` (`aws_iam_role_policy.operator_artifacts`)                                                                       | Add per-tenant prefix permissions                                 | Operator can rewrite the artifacts bucket policy. Mitigation: bucket already enforces TLS + KMS via existing deny statements; removing those statements is CloudTrail-logged.                                                                                        |
| `bedrock:ListFoundationModels`, `GetFoundationModel`, `ListInferenceProfiles`, `GetInferenceProfile`, `ListGuardrails`, `GetGuardrail`                                                                                   | `*`                                                        | `agent-iam` (`aws_iam_role_policy.operator_bedrock_introspection`)                                                           | Resolve model IDs at reconcile time                               | Read-only introspection. No invocation. No guardrail mutation.                                                                                                                                                                                                       |
| `events:PutEvents`                                                                                                                                                                                                       | `<env>-<cluster>-killswitch` event bus only                | `kill-switch` (`aws_cloudwatch_event_bus_policy.operator_put` — granted by the bus's resource policy, not the operator role) | Fire BudgetBreach events to trigger the kill-switch state machine | Operator can fire a kill-switch falsely (DoS against itself). The kill-switch state machine is idempotent and recovery is human-only with MFA — false-positive firing is annoying, not destructive. Scope is bus-level: cannot publish to any other EventBridge bus. |
| `athena:StartQueryExecution`, `GetQueryExecution`, `GetQueryResults`, `StopQueryExecution`, `GetWorkGroup`                                                                                                               | Cost-pipeline Athena workgroup                             | `cost-pipeline` (`aws_iam_role_policy_attachment.operator_cost` → `aws_iam_policy.operator_cost`)                            | Query CUR for per-Platform spend                                  | Query-only. No data-modification path.                                                                                                                                                                                                                               |
| `glue:GetDatabase`, `GetTable`, `GetTables`, `GetPartitions`                                                                                                                                                             | Cost-pipeline catalog only                                 | `cost-pipeline` (same attachment as above)                                                                                   | Read CUR schema                                                   | Read-only.                                                                                                                                                                                                                                                           |
| `s3:GetObject`, `PutObject`, `ListBucket`                                                                                                                                                                                | Athena results bucket                                      | `cost-pipeline` (same attachment)                                                                                            | Athena output staging                                             | Standard query-result pattern; bucket has 30-day TTL.                                                                                                                                                                                                                |
| `cloudwatch:GetMetricStatistics`, `GetMetricData`, `ListMetrics`                                                                                                                                                         | `*`                                                        | `cost-pipeline` (same attachment)                                                                                            | Bedrock invocation metrics                                        | Read-only.                                                                                                                                                                                                                                                           |

**Operator does not have:**

- `iam:*` outside `/eks-agent-platform/tenants/`
- `kms:Decrypt`, `kms:Encrypt`, or any key-content access
- `bedrock:InvokeModel*` (it never invokes models on behalf of tenants)
- Any `s3:*` outside the two named buckets
- Any cross-account permissions
- Any organization or account-management permissions

**What full operator compromise gets you:**

The attacker can:

1. Create new IAM roles under `/eks-agent-platform/tenants/` and trust any principal (including external accounts) → pivot to Bedrock invocations by creating a role that the attacker controls + invoking through it. **Detection:** CloudTrail records every `CreateRole`, `PutRolePolicy`, `UpdateAssumeRolePolicy` with the operator's role as actor. GuardDuty Anomaly Detection should fire on the unfamiliar trust relationship.
2. Detach the Bedrock-invoke policy from every legitimate tenant role → denial-of-service. **Detection:** SpendReport drops to zero across all tenants simultaneously; ops dashboard alerts.
3. Grant decrypt access on cmk-data to attacker-controlled tenant roles → read tenant S3 prefixes. **Detection:** `kms:CreateGrant` actions with unfamiliar grantee principals in CloudTrail.
4. Mutate the artifacts bucket policy → add an allow statement for attacker principal. **Detection:** S3 bucket policy changes are CloudTrail-logged.
5. Burst `events:PutEvents` to the kill-switch bus across many `Platform`s in parallel → trigger a fleet-wide kill-switch storm that suspends legitimate tenants and forces SSO-elevation recovery for each one. The blast radius is bounded by what the kill-switch state machine does (detach Bedrock-invoke + scale runtimes to 0; both reversible only through the human recovery path), but the recovery cost scales with the number of `Platform` CRs in the cluster. Per-reconciler `MaxConcurrentReconciles` (default `runtime`=5) caps the parallel rate but not the cumulative volume. **Detection:** EventBridge archive on the kill-switch bus shows an unfamiliar burst pattern; CloudTrail records every `PutEvents` with the operator role as actor. **Mitigation:** the SSO recovery path requires MFA + dual-approval, so a single attacker cannot also undo their own DoS — the attack has high signal-to-noise. Future hardening should add a per-bus rate-limit + per-Platform `PutEvents` quota to compress the burst window.

**What full operator compromise does NOT get you:**

- Direct access to tenant data (no `kms:Decrypt`, no `s3:GetObject` on tenant prefixes — that requires the _tenant role_, which the operator can mint but every minting is logged)
- Cross-account access (no cross-account trust on the role)
- Account takeover (no admin paths)
- Bedrock invocation directly (no `InvokeModel` permission; would have to mint a role + assume it, both CloudTrail-logged)

## Cross-component contract: tenant role naming

The kill-switch Step Functions state machine in `terraform/components/kill-switch/main.tf` constructs the IAM role name to detach the Bedrock-invoke policy from using `States.Format` against a build-time pattern. **The operator's PlatformReconciler MUST mint tenant roles matching this pattern**, or the kill-switch silently fails on breach (the `iam:DetachRolePolicy` call against a nonexistent role routes to `RecordFailure` with no alarm path).

Default pattern: `<env>-<platformId>-tenant` (set via `kill-switch.tenant_role_name_pattern`, with `<env>` replaced at Terraform plan time and `{}` replaced at SFN runtime with `$.detail.platformId`).

Concrete example: for `environment=dev` and `Platform.metadata.name=marketing-team`, the kill-switch detaches the baseline policy from role `dev-marketing-team-tenant`. The operator therefore mints that role as `<env>-<Platform.name>-tenant` under IAM path `/eks-agent-platform/tenants/`.

If the pattern changes, both sides must move together — the variable's description block in `kill-switch/variables.tf` is the source of truth, and this ADR section is the cross-component reference.

## Cross-component contract: kill-switch suspension marker

When the kill-switch fires (`BudgetPolicy` breach ≥ 120% with `KillSwitchEnabled: true`), the SFN does **two** things to the tenant role:

1. `iam:DetachRolePolicy` — removes the baseline policy that grants Bedrock invoke. The tenant role can no longer call `bedrock:InvokeModel`.
2. `iam:TagRole` — sets `platform.nanohype.dev/suspended=true` and `platform.nanohype.dev/suspended-reason=<reason>` on the role.

**The operator's `PlatformReconciler.ensureIamRole` MUST read these tags before reattaching the baseline policy.** Without the tag check the operator's `attachBaselineIfMissing` helper would notice the missing baseline and reattach it on next reconcile (default ~60s) — silently undoing the kill-switch.

When the tag is present:

- `Platform.status.phase` becomes `Suspended` with `status.suspendedAt` + `status.suspendedReason` populated,
- the operator skips KMS-grant + bucket-policy reconciliation (no point granting data access while invoke is blocked),
- `AgentFleetReconciler` tears down the fleet's kagent Agents + KEDA ScaledObject so no pods can serve traffic.

Recovery: ops removes the `platform.nanohype.dev/suspended` tag (and the `-reason` tag) from the IAM role via the CLI / console. The operator's next reconcile sees the cleared tag, reattaches the baseline, and the Platform returns to `Ready`. The `SuspendedAt` field flips back to `nil`. No CR mutation is required for recovery.

If the tag keys change, both sides must move together — the SFN definition in `kill-switch/main.tf` and the `suspendedTag` constant in `operators/internal/controller/platform_iam.go` are the source of truth.

## Recovery from operator compromise

1. **Disable the operator's IRSA trust** by removing the operator ServiceAccount from the IAM role's trust policy. Operator pods stop being able to authenticate to AWS within ~1 minute (STS token refresh interval).
2. **Audit CloudTrail for the operator role ARN** over the suspected window. Every `CreateRole`, `PutRolePolicy`, `CreateGrant`, `PutBucketPolicy`, `PutEvents` is recorded.
3. **Revert the operator role** to a fresh provision via `terraform/components/agent-iam` (it's stateless apart from the role itself).
4. **Sweep** any tenant roles created during the window — match against the legitimate `Platform` CRs in the cluster and delete anything orphaned.
5. **Rotate cmk-data** if `CreateGrant` actions during the window granted decrypt to unfamiliar principals.

Recovery is SSO-permission-set-elevation gated. There is no API path for the operator to undo its own compromise.

## STRIDE threat enumeration

Six threat categories, evaluated per architecture component.

### Tenant attacks tenant (cross-tenancy)

| Threat                                                      | STRIDE                     | Mitigation                                                                                                                                                                       |
| ----------------------------------------------------------- | -------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Tenant A reads Tenant B's S3 prefix                         | **I**nformation Disclosure | Tenant roles scope `s3:GetObject` to `tenants/<own-platform-id>/*` via path-prefix policy. KMS grant on cmk-data is also scoped per-tenant via `kms:EncryptionContext` matching. |
| Tenant A invokes Bedrock with Tenant B's model-access ARNs  | **E**levation of Privilege | Per-tenant roles only attach the Bedrock policies generated for their own Platform. Cross-tenant ARNs aren't in the policy.                                                      |
| Tenant A reads Tenant B's CloudWatch logs                   | **I**nformation Disclosure | Per-tenant log groups namespaced by `/eks-agent-platform/<env>/tenants/<platform-id>/*`. Tenant role only has `logs:PutLogEvents` on its own path.                               |
| Tenant A exhausts shared Bedrock quota → Tenant B throttled | **D**enial of Service      | Per-route rate limits in `ModelGateway.spec.routes[].rateLimit` enforced by agentgateway. Bedrock account-level quotas remain a shared concern — call out in the runbook.        |
| Tenant A spams agentgateway → cluster-wide perf hit         | **D**enial of Service      | Per-tenant `NetworkPolicy` egress restricts to agentgateway service; agentgateway has its own KEDA scaling + PDB; tenant `ResourceQuota` caps pod count.                         |

### Tenant attacks operator

| Threat                                                                                         | STRIDE      | Mitigation                                                                                                                                                                     |
| ---------------------------------------------------------------------------------------------- | ----------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| Tenant crafts a malicious `Platform` spec that triggers operator panic / unbounded reconcile   | **D**oS     | Validating webhook rejects malformed specs. Reconciler has explicit error budgets via controller-runtime's backoff. CRD schema enforces enum + min/max constraints.            |
| Tenant adds a label that matches the operator's NetworkPolicy selector → pivot inbound traffic | **E**oP     | Operator NetworkPolicy only accepts ingress from `monitoring` namespace (Prometheus scrape) and the API server (webhook port). No tenant-namespace ingress allowed.            |
| Tenant CR triggers an SSRF in a reconciler (e.g. fetching an unvalidated URL)                  | **E**oP / I | Reconcilers do not fetch external URLs at reconcile time. Eval reports and pricing data are fetched only by explicitly-named bucket/path; new reconcilers must hold this line. |

### External attacks platform

| Threat                                                                  | STRIDE               | Mitigation                                                                                                                                                                                                                                                          |
| ----------------------------------------------------------------------- | -------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| External attacker bypasses agentgateway → reaches a tenant pod directly | **S**poofing / **I** | `NetworkPolicy` default-deny inbound on every tenant namespace. Only agentgateway service can reach tenant pods. Cilium enforces.                                                                                                                                   |
| External attacker reaches the agentgateway listener                     | **I** / **E**oP      | ALB ingress is `scheme: internal` by default; WAF rule set (Common + KnownBadInputs + rate limit) when public. Bedrock Guardrails attached at every route catch injection attempts in payload.                                                                      |
| External attacker steals a leaked tenant credential                     | **S**                | EKS Pod Identity credentials are short-lived, vended per-pod by the Pod Identity agent over the local credential endpoint and not mounted as a long-lived token; the role is assumable only via `pods.eks.amazonaws.com` for the bound (namespace, ServiceAccount). |

### Supply-chain attacks

| Threat                                           | STRIDE        | Mitigation                                                                                                                                                   |
| ------------------------------------------------ | ------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| Compromised operator image pushed to GHCR        | **T**ampering | All operator images signed with cosign (keyless OIDC) + SBOM attestation. Kyverno verify-images policy in `eks-gitops` rejects unsigned images cluster-wide. |
| Compromised npm package in `@eks-agent/sdk` deps | **T**         | Renovate vulnerability alerts always-on, never auto-merge. OSV alerts enabled. Trivy fs scan in CI fails on HIGH/CRITICAL.                                   |
| Compromised Helm chart pulled by ArgoCD          | **T**         | OCI charts published with signed metadata. AppProject `sourceRepos` allowlist is closed-set; only the named OCI registries are reachable.                    |
| Compromised kagent or agentgateway upstream      | **T**         | Pinned chartVersion in ApplicationSet matrix; Renovate proposes bumps but human reviews majors. Cluster-wide image-verification policy applies.              |

### Audit + non-repudiation

| Threat                                   | STRIDE          | Mitigation                                                                                                                                                                                                                                      |
| ---------------------------------------- | --------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Operator denies it created a tenant role | **R**epudiation | CloudTrail records the operator role ARN as `userIdentity.sessionContext.sessionIssuer` for every `CreateRole`. EventBridge archive on the kill-switch bus retains every breach event 365 days. Bedrock invocation log bucket is Object-Lock'd. |
| Tenant denies it invoked Bedrock         | **R**           | Bedrock invocation logging captures input + output + token usage + invoking IAM principal per call. S3 Object Lock prevents tamper for the configured retention.                                                                                |

## Mitigations checklist (operationalized)

The mitigations above are not aspirational. Today, the platform ships:

- [x] Operator IRSA scoped to `/eks-agent-platform/tenants/` path
- [x] No `kms:Decrypt` on operator role
- [x] No `bedrock:InvokeModel*` on operator role
- [x] cmk-data and cmk-logs separated (auditor role only sees logs)
- [x] Tenant baseline policy with `bedrock:InvokeModel` constrained by tag equality
- [x] Bedrock invocation logging to Object-Lock'd S3 bucket
- [x] EventBridge archive 365-day retention on kill-switch bus
- [x] Kill-switch recovery is human-only (SSO permission-set elevation)
- [x] Operator chart NetworkPolicy default-deny + explicit allows
- [x] gitleaks pre-commit + CI secret scanning
- [x] trivy config + fs vuln scan + tflint + gosec + golangci + eslint-security all in CI

Planned hardening:

- [ ] Validating webhook on `Platform`, `BudgetPolicy`, `ModelGateway` (CRD schema is the floor; webhook adds cross-field invariants)
- [ ] cosign keyless signing on operator images via Sigstore/Fulcio (workflow exists; needs first tagged release)
- [ ] Kyverno `verify-images` policy in `eks-gitops` referencing our cosign signer
- [ ] GuardDuty Anomaly Detection rules tuned for "unfamiliar IAM principal as grantee" events

## Consequences

- Anyone reading this ADR can rebuild the operator's trust model from scratch.
- New reconciler logic must justify any new IAM permission against this list. Adding a permission means updating this ADR.
- Compromise recovery is documented; the runbook references this section by anchor.
- The "what compromise does NOT get you" half is the load-bearing claim. Don't let new work weaken it.
