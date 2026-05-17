# charts/bedrock-egress

Installs the Bedrock-side config + NetworkPolicy template consumed by the operator.

- **agentgateway-config ConfigMap** — region, endpoint, timeouts, retry mode, OTel endpoint, fallback routes. Loaded by agentgateway pods via `envFrom`.
- **tenant-networkpolicy-template ConfigMap** — the default-deny + selective-allow `NetworkPolicy` the operator clones into every tenant namespace at `Platform` reconcile time.

Why a separate chart from `operator`: this chart is data, not code. It can be upgraded without rolling the operator.
