# charts/bedrock-egress

Installs the Bedrock-side config + NetworkPolicy template consumed by the operator.

- Gateway configuration — region, model routes, rate limits, guardrails — lives on the `ModelGateway` CR, not in this chart.
- **tenant-networkpolicy-template ConfigMap** — the default-deny + selective-allow `NetworkPolicy` the operator clones into every tenant namespace at `Platform` reconcile time.

Why a separate chart from `operator`: this chart is data, not code. It can be upgraded without rolling the operator.
