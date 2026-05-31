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

## Fix 2 — agent-plane rewritten for kagent 0.9.4 + agentgateway 1.0.1 (shipped)

The AgentFleet + ModelGateway reconcilers emitted CRs the installed latest
versions rejected, so the agent plane was entirely broken. Rewritten to the current API
and verified end-to-end on kx (ModelGateway + AgentFleet both reach Ready;
Gateway PROGRAMMED; ModelConfig Accepted; Agent carries systemMessage).

The integration: `ModelGateway` → a per-Platform Gateway-API `Gateway`
(gatewayClassName `agentgateway`) + per route an `AgentgatewayBackend`
(`spec.ai.provider.bedrock{model,region,guardrail}`), an `HTTPRoute` at
`/<platform>-<route>`, and an `AgentgatewayPolicy` (`traffic.rateLimit.local`)
when the route sets a limit. `AgentFleet` → a kagent `ModelConfig`
(provider=OpenAI, `openAI.baseUrl` = the route endpoint on the Gateway service)

- a kagent `Agent` bound to it. agentgateway exposes an OpenAI-compatible
  endpoint and proxies to Bedrock, applying guardrail + rate limit, authenticating
  with its own IRSA.

What was wrong and fixed:

- kagent Agent emitted `spec.systemPrompt` → now `spec.declarative.systemMessage`.
- kagent ModelConfig emitted `spec.provider:{object}` → now `provider: OpenAI` +
  `model` + `openAI.baseUrl`.
- agentgateway `Route` (dead `agentgateway.dev/v1alpha1` kind) → Gateway +
  HTTPRoute + AgentgatewayBackend + AgentgatewayPolicy.

Two things the live cluster corrected over the upstream docs:

- **ModelConfig is keyless.** The operator does not write Secrets (tenant
  credentials flow through ExternalSecrets), and agentgateway does the real
  auth, so no `apiKeySecretRef` is set — a keyless ModelConfig is Accepted.
- **Agent storage version is v1alpha2.** kagent serves v1alpha1 (flat) +
  v1alpha2 (storage, `type: Declarative` + `declarative` wrapper). A flat
  v1alpha1 Agent loses its fields on conversion, so the operator emits v1alpha2
  with the declarative wrapper directly.

RBAC: the chart ClusterRole + the kubebuilder markers gained
`gateway.networking.k8s.io/{gateways,httproutes}` +
`agentgateway.dev/{agentgatewaybackends,agentgatewaypolicies}` +
`agents.nanohype.dev/modelgateways` (read, for the route-model resolver). The
Secret rule stays read-only.

**Follow-up:** envtest conformance still doesn't install the real kagent /
agentgateway CRDs (that gap is why the drift went undetected by `make test`).
The rewrite is validated live on kx; adding real-CRD envtest coverage to lock it
against regression is the remaining hardening step.

## Fix 3 — blank-tenant example has no Tenant CR (pending)

`examples/blank-tenant/platform.yaml` sets `spec.tenant: example` but ships no
Tenant CR → `cloudgov platform audit` flags LOW `TENANT_MISSING`. Add a Tenant
CR to the example so it's a complete, conformant reference.

## Related

kx itself does not install the operator — tracked as a fix-forward in the kx
repo (add the operator install to the `ai-platform` slice with the verified
local profile: `--disable-aws`, self-signed issuer, `networkPolicy.engine:
cilium`).
