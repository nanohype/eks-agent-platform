# Security Policy

## Reporting a vulnerability

Email rackctl@gmail.com with subject `[security][eks-agent-platform]`. Do not open public issues for security reports.

Acknowledgement target: within 72 hours. Triage target: within 5 business days.

## Security posture

This platform is a tenancy substrate. Its security model assumes:

- The hosting EKS cluster is provisioned by `landing-zone` with CIS EKS baseline enforced.
- ArgoCD is deployed by `landing-zone` (OpenTofu) with SSO-only access.
- `eks-gitops` enforces Pod Security Standards `restricted` and Kyverno verify-images policies.

### Tenant isolation

Two workload-isolation tiers, dialed per Platform by `spec.isolation` and immutable after create. Both share the same host-side containment; the second adds an API boundary. Full model: [`docs/architecture/tenant-isolation-tiers.md`](docs/architecture/tenant-isolation-tiers.md).

- **`namespace` (default)** — namespace-per-Platform with `ResourceQuota` + `LimitRange` + PSS-`restricted` + default-deny `NetworkPolicy`/Cilium egress, an ArgoCD `AppProject` scoped to the Platform's namespace and source repos, and a per-Platform IAM role bound to the `tenant-runtime` ServiceAccount by an EKS Pod Identity association, under a constrained IAM path (`/eks-agent-platform/tenants/`). Tenant workloads share the host API server; isolation is namespace RBAC + network policy.
- **`vcluster`** — the same host-side containment **plus** a per-Platform virtual cluster, so tenant code that holds a Kubernetes API token talks to its own API server, not the host's. This is **API-server-level** isolation — a control the namespace tier's RBAC/network policy do not provide, because they mediate access to the host API rather than removing it from view. It is **not** kernel or node isolation: synced pods run on the same nodes and kernel as every other tenant's, so a container escape is exactly as available as in the namespace tier. Kernel/compute isolation is an orthogonal dial — the tainted `AgentSandbox` node pool, or a dedicated cluster. The operator declares the vcluster as an ArgoCD `Application` (ArgoCD is a hard prerequisite; the tier fails closed if it is absent, never silently downgrading), the host quota/PSS/NetworkPolicy bound the vcluster's control-plane pod, its syncer, and every pod the syncer lands from outside, and the syncer's own ServiceAccount carries **no** Pod Identity association, so a compromised syncer has no AWS reach. Design of record: [ADR 0008](docs/adr/0008-vcluster-isolation-tier.md).

### Identity

- No long-lived credentials anywhere. Tenant pods get credentials from the EKS Pod Identity agent; the operator itself runs with an IRSA role, scoped to the tenant IAM path + Bedrock policy attach/detach. It makes no KMS API call at all — the tenant's key access is an IAM policy the operator writes, not a grant it issues.
- Tool credentials projected into agent pods via External Secrets Operator (already in `eks-gitops`), backed by AWS Secrets Manager. Tools run in the agent's own process, so a tool's credential is scoped to the agent that uses it rather than to a shared tool server.

### Encryption

- **One customer-managed key backs a cluster by default.** landing-zone's `secrets` component mints it and both `data_kms_key_arn` and `logs_kms_key_arn` resolve to it. An environment that wants the log path on its own key sets `separate_logs_key` there: the CloudWatch Logs and Bedrock grants **move** onto a second CMK, and the platform reads the same two variables either way. Shared is the default because the separation is only worth a second key where the log reader and the data reader are different people — where they are the same one, two keys buy nothing and cost rotation, audit and money.
- **Each tenant gets its own CMK for its own data.** `tenant-substrate` mints one per Platform, and the operator grants the tenant role use of exactly that ARN — `GenerateDataKey`, `Decrypt`, `DescribeKey`, resource-scoped to the single key, with no key-management verbs. Granting on a name pattern would reach every tenant's key, so the policy names one ARN and a Platform whose key does not exist yet gets no policy rather than a wildcard.
- **The shared model-artifacts bucket is the exception, and its tenant boundary is IAM, not cryptography.** The operator writes a per-tenant bucket-policy statement scoped to `tenants/<platform>/*`, and that statement is the whole separation: nothing else in the account grants a tenant role object access to that bucket, so a read of another tenant's prefix is an implicit deny at S3 before KMS is ever consulted. The KMS layer contributes no tenant-vs-tenant discrimination there — every tenant role holds the same `kms:Decrypt` on the platform key through the baseline policy, conditioned only on `kms:ViaService = s3`, and S3's SSE-KMS encryption context is `aws:s3:arn`, which with an S3 Bucket Key enabled is the *bucket* ARN and therefore identical for every tenant. Read the prefix policy as the control, not as one of two.
- S3 buckets enforce SSE-KMS, with one exception worth naming rather than discovering: the server-access-log destination buckets are SSE-S3, because S3 does not support SSE-KMS for that delivery path.
- CloudWatch log groups are encrypted with whichever key `logs_kms_key_arn` resolves to.

### Egress

- VPC endpoints for `bedrock-runtime`, `sts`, `s3`, `secretsmanager`, `logs`, `monitoring`.
- WAF on the public-facing model gateway listener.
- Bedrock invocation logging written to a tamper-evident S3 bucket with Object Lock (governance mode by default, compliance mode for regulated tenants).

### Supply chain

- All operator images signed with cosign; verify-images policy in `eks-gitops` blocks unsigned images cluster-wide.
- SBOM (SPDX) generated with syft on every tagged release.
- Renovate keeps `@eks-agent/pricing` and dep versions current weekly.

### Kill-switch

A `BudgetPolicy` breach at ≥120% publishes a `BudgetBreach` event that an EventBridge rule routes to a Step Functions state machine; the machine detaches the Bedrock-invoke baseline policy from the tenant role and tags it `platform.nanohype.dev/suspended=true`. The Platform reconciler reads that tag, moves the Platform to `Suspended`, and the fleet reconciler tears its agents down to zero. Publishing the event is not treated as success — the budget reconciler effect-verifies the suspension and, if the platform is still not `Suspended` after a grace window, re-fires the breach (bounded backoff) and raises a `KillSwitchUnrouted` alert, so a broken suspension path can never latch as a false success. Recovery requires SSO permission-set elevation with MFA + approver; there is no API path back without elevation.

## Known limitations

- Bedrock Guardrails are region-gated: the `bedrock` component creates the baseline Guardrail only where the service is available and publishes a null id elsewhere, and a route runs without a guardrail rather than failing when none resolves. Guardrails attach per route through `ModelGateway.spec.routes[].guardrailRef` (falling back to the gateway's `defaultGuardrailRef`, then the account baseline); the gateway reconciler stamps the resolved `{identifier, version}` onto the route's request headers, which Bedrock enforces on input and output. The mutation uses `set` rather than `add`, so a caller that sends its own guardrail headers has them overwritten rather than honoured.
- There is no auditor role. landing-zone declares an `Auditor` IAM Identity Center permission set, but it carries no account assignments, so it materializes as no IAM role in any account; its `SecurityAudit` policy grants KMS metadata reads (`Describe*`/`Get*`/`List*`) and no `kms:Decrypt`. The posture where a principal reads operational logs but not platform data needs both the key separation (available per environment via landing-zone's `separate_logs_key`, off by default) and a principal assigned to use it. ADR 0003 tracks it.
- The `vcluster` tier adds API-server-level isolation, not compute isolation — synced pods share the host's nodes and kernel. Pair it with the tainted sandbox node pool when node-level separation is required. It also depends on ArgoCD and a vcluster-internal naming algorithm; the operator discovers the syncer-renamed host ServiceAccount by label and cross-checks it against a byte-identical replica of vcluster's algorithm, so an upstream naming change on upgrade fails loud rather than binding Pod Identity to the wrong name.

## Compliance

This platform does not produce a compliance certification on its own. It exposes the controls needed for:

- **SOC 2 Type II** — audit trail via Bedrock invocation logging + EventBridge archive, encrypted at rest with CMK, access-logged via CloudTrail.
- **HIPAA** — requires a BAA with AWS. The controls a HIPAA workload leans on are the same substrate every Platform gets: CMK encryption at rest, per-tenant isolation, invocation logging, and Guardrails where a route references one.

`Platform.spec.compliance` declares which of these regimes a Platform is in scope for. It is a declaration, not a switch — the operator provisions nothing differently from it. `cloudgov platform audit` reads it and checks the rest of the declaration lines up: a `soc2` Platform must have `killSwitchEnabled` on its BudgetPolicy, and a Platform must declare at least what its owning Tenant declares. Anything stricter than that is a control you configure explicitly.
- **CIS EKS** — baseline enforced upstream by `landing-zone` + `eks-gitops`.
