# Runtime validation fixes — operator against the real org substrate

Findings from validating the operator on a live cluster (kx: cilium CNI +
kagent 0.9.4 + agentgateway 1.0.1) — the things `make test` (envtest, no real
kagent/agentgateway CRDs, no cilium) cannot catch.

## What's solid

The platform control plane reconciles end-to-end. Applying a Platform creates
the full tenant boundary: namespace `tenants-<platform>` with PSS=restricted +
ownership labels, `tenant-default` ResourceQuota + LimitRange, `tenant-egress`
NetworkPolicy, `tenant-runtime` SA, and the ArgoCD AppProject. `cloudgov
platform audit` reads it correctly (k8s-side checks pass; IRSA flagged only
because `--disable-aws` mints no role).

## Fix 1 — operator chart NetworkPolicy is cilium-incompatible (shipped)

`charts/operator/templates/networkpolicy.yaml` allowed egress to the
kube-apiserver via an `ipBlock` of the service CIDR. Under cilium (the CNI on
every cluster this operator runs on — kx and EKS) the API ClusterIP resolves to
the reserved `kube-apiserver` identity, which CIDR rules — even `0.0.0.0/0` — do
not match. The operator could not reach the apiserver to register its Tenant
field-index and CrashLooped on `dial tcp 10.96.0.1:443: i/o timeout`. Setting
the correct CIDR did not help; only disabling the policy did.

Fix: add `networkPolicy.engine`. `cilium` (default) emits a CiliumNetworkPolicy
that allows apiserver egress via `toEntities: kube-apiserver`, AWS APIs via
`toEntities: world` on 443, and DNS to kube-dns; webhook ingress uses
`fromEntities: kube-apiserver`. `kubernetes` keeps the vanilla NetworkPolicy as
a portable fallback for non-cilium clusters. Verified on kx: fresh operator pods
come up Running 1/1 with the CNP enforcing, no apiserver timeout.

## Fix 2 — agent-plane targets stale kagent/agentgateway APIs (pending)

The AgentFleet + ModelGateway reconcilers emit CRs the installed latest versions
reject. All three need rewriting to the current API, plus conformance coverage
that installs the real kagent + agentgateway CRDs.

- **kagent Agent** (`agentfleet_reconcile.go`): emits `spec.systemPrompt`;
  kagent 0.9.4 Agent uses `spec.systemMessage`. The unknown field is pruned →
  an Agent with no system prompt.
- **kagent ModelConfig** (`agentfleet_reconcile.go`): emits
  `spec.provider: {type, baseURL, route}`; kagent 0.9.4 wants `spec.provider` as
  a string enum (`Anthropic|OpenAI|…`) + required `spec.model` + a provider
  block (`openAI.baseUrl`, etc.). Hard-rejected → AgentFleet reconcile errors,
  fleet stuck Pending.
- **agentgateway Route** (`modelgateway_reconcile.go`): emits
  `agentgateway.dev/v1alpha1` kind `Route`; agentgateway 1.0.1 has no such CRD
  (ships `AgentgatewayBackend` + Gateway API `HTTPRoute`/`Gateway`). NoKindMatch
  → treated as "agentgateway not installed" → ModelGateway stuck Pending.

Target the latest pinned versions (kagent 0.9.4, agentgateway 1.0.1 — what kx
installs). The integration intent is unchanged: Agent → ModelConfig → in-cluster
agentgateway → Bedrock with guardrail + rate-limit.

## Fix 3 — blank-tenant example has no Tenant CR (pending)

`examples/blank-tenant/platform.yaml` sets `spec.tenant: example` but ships no
Tenant CR → `cloudgov platform audit` flags LOW `TENANT_MISSING`. Add a Tenant
CR to the example so it's a complete, conformant reference.

## Related

kx itself does not install the operator — tracked as a fix-forward in the kx
repo (add the operator install to the `ai-platform` slice with the verified
local profile: `--disable-aws`, self-signed issuer, `networkPolicy.engine:
cilium`).
