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

- **`namespace` (default)** — namespace-per-Platform with `ResourceQuota` + `LimitRange` + PSS-`restricted` + default-deny `NetworkPolicy`/Cilium egress, an ArgoCD `AppProject` scoped to the Platform's namespace and source repos, and a per-Platform IRSA role bound to the `tenant-runtime` ServiceAccount via EKS Pod Identity, under a constrained IAM path (`/eks-agent-platform/tenants/`). Tenant workloads share the host API server; isolation is namespace RBAC + network policy.
- **`vcluster`** — the same host-side containment **plus** a per-Platform virtual cluster, so tenant code that holds a Kubernetes API token talks to its own API server, not the host's. This is **API-server-level** isolation — a control the namespace tier's RBAC/network policy do not provide, because they mediate access to the host API rather than removing it from view. It is **not** kernel or node isolation: synced pods run on the same nodes and kernel as every other tenant's, so a container escape is exactly as available as in the namespace tier. Kernel/compute isolation is an orthogonal dial — the tainted `AgentSandbox` node pool, or a dedicated cluster. The operator declares the vcluster as an ArgoCD `Application` (ArgoCD is a hard prerequisite; the tier fails closed if it is absent, never silently downgrading), the host quota/PSS/NetworkPolicy bound the vcluster's control-plane pod, its syncer, and every pod the syncer lands from outside, and the syncer's own ServiceAccount carries **no** Pod Identity association, so a compromised syncer has no AWS reach. Design of record: [ADR 0009](docs/adr/0009-vcluster-isolation-tier.md).

### Identity

- No long-lived credentials anywhere. Pods get tokens via Workload Identity (IRSA). The operator itself runs with an IRSA role scoped to the tenant IAM path + KMS grant + Bedrock policy attach/detach.
- Tool credentials projected into kagent `ToolServer` pods via External Secrets Operator (already in `eks-gitops`), backed by AWS Secrets Manager.

### Encryption

- Two customer-managed keys back every cluster — `cmk-data` and `cmk-logs` — provisioned once by landing-zone, not one pair per Platform. Per-Platform isolation is a scoped KMS grant, not a dedicated key: the operator issues each tenant role a grant on `cmk-data` constrained to `EncryptionContext={PlatformId: <platform>}`, so a tenant role can only decrypt objects written under its own PlatformId. A breach of one tenant role reaches only that tenant's encryption context.
- All S3 buckets enforce SSE-KMS with `cmk-data`, keyed per Platform by that encryption context.
- CloudWatch log groups are encrypted with `cmk-logs`. The auditor role has decrypt on `cmk-logs` only.

### Egress

- VPC endpoints for `bedrock-runtime`, `sts`, `s3`, `secretsmanager`, `logs`, `monitoring`.
- WAF on the public-facing agentgateway listener.
- Bedrock invocation logging written to a tamper-evident S3 bucket with Object Lock (governance mode by default, compliance mode for regulated tenants).

### Supply chain

- All operator images signed with cosign; verify-images policy in `eks-gitops` blocks unsigned images cluster-wide.
- SBOM (SPDX) generated with syft on every tagged release.
- Renovate keeps `@eks-agent/pricing` and dep versions current weekly.

### Kill-switch

A `BudgetPolicy` breach at ≥120% publishes a `BudgetBreach` event that an EventBridge rule routes to a Step Functions state machine; the machine detaches the Bedrock-invoke baseline policy from the tenant role and tags it `platform.nanohype.dev/suspended=true`. The Platform reconciler reads that tag, moves the Platform to `Suspended`, and the fleet reconciler tears its agents down to zero. Publishing the event is not treated as success — the budget reconciler effect-verifies the suspension and, if the platform is still not `Suspended` after a grace window, re-fires the breach (bounded backoff) and raises a `KillSwitchUnrouted` alert, so a broken suspension path can never latch as a false success. Recovery requires SSO permission-set elevation with MFA + approver; there is no API path back without elevation.

## Known limitations

- Bedrock Guardrails are region-gated: the `bedrock` component creates the baseline Guardrail only where the service is available and publishes a null id elsewhere, and a route runs without a guardrail rather than failing when none resolves. Guardrails attach per route through `ModelGateway.spec.routes[].guardrailRef` (falling back to the gateway's `defaultGuardrailRef`, then the account baseline); the gateway reconciler stamps the resolved `{identifier, version}` onto the agentgateway Bedrock backend, which enforces it on input and output.
- DRA is beta in Kubernetes; behavior depends on the `featureGates` enabled in your EKS cluster version.
- The `vcluster` tier adds API-server-level isolation, not compute isolation — synced pods share the host's nodes and kernel. Pair it with the tainted sandbox node pool when node-level separation is required. It also depends on ArgoCD and a vcluster-internal naming algorithm; the operator discovers the syncer-renamed host ServiceAccount by label and cross-checks it against a byte-identical replica of vcluster's algorithm, so an upstream naming change on upgrade fails loud rather than binding Pod Identity to the wrong name.

## Compliance

This platform does not produce a compliance certification on its own. It exposes the controls needed for:

- **SOC 2 Type II** — audit trail via Bedrock invocation logging + EventBridge archive, encrypted at rest with CMK, access-logged via CloudTrail.
- **HIPAA** — requires a BAA with AWS; `Platform.spec.compliance.hipaa = true` enables stricter defaults (Object Lock compliance mode, no cross-region inference, mandatory Guardrails with PII detection enabled).
- **CIS EKS** — baseline enforced upstream by `landing-zone` + `eks-gitops`.
