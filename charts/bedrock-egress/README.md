# charts/bedrock-egress

Installs the Bedrock-side config + NetworkPolicy template consumed by the operator.

- Gateway configuration — region, model routes, rate limits, guardrails — lives on the `ModelGateway` CR, not in this chart.
- **tenant-networkpolicy-template ConfigMap** — a READABLE REFERENCE of the default-deny + selective-allow `NetworkPolicy` shape a tenant namespace gets. The operator does not read this ConfigMap: it builds the real policies in Go (`cilium_egress.go`, `platform_reconcile.go`) and applies those. Nothing consumes this chart at runtime, so treat it as documentation that happens to be valid YAML — and keep its namespaces in step with the operator's, which `scripts/check-doc-contracts.sh` enforces.

Why a separate chart from `operator`: this chart is data, not code. It can be upgraded without rolling the operator.
