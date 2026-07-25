# charts/tenant

Opinionated scaffold for a single tenant Platform. Renders:

- `Platform` — tenancy boundary + IRSA + isolation mode
- `BudgetPolicy` — monthly USD cap + kill-switch
- `ModelGateway` — one default route, persona-tuned model family
- `AgentFleet` — at least one agent, with KEDA scaling defaults
- `EvalSuite` — daily smoke test by default

Consumed by `agentctl platform new`. Can also be rendered directly:

```bash
helm template marketing-team ./charts/tenant \
  --set platform.name=marketing-team \
  --set platform.persona=marketing \
  --set platform.tenant=acme \
  --set budget.monthlyUsd=2500
```

## The full Platform vocabulary

Beyond identity and budget, a Platform declares three more things. Each is
optional and off by default, and `charts/tenant/ci/full-values.yaml` is the
worked example with every field set.

- **`datastores`** — the tenant's stateful substrate: relational, keyValue,
  objectStore, queue, cache, stream. A declaration, not a component: the generic
  `tenant-substrate` tofu module provisions the resource from this same list and
  the operator generates the scoped IAM policy that reaches it.

  Two steps, two owners. Declaring a datastore grants the tenant role access to
  the resource's ARN and reports it under `status.datastores`. The resource is
  provisioned when the declaration reaches landing-zone's `tenant-substrate`
  `tenants` input — until it does, the tenant holds a grant on a name that does
  not exist yet.

- **`identity.capabilities`** — managed AWS capabilities outside the datastore
  vocabulary (`ses`, `eventBridgeScheduler`), each driving an operator-generated
  inline policy rather than a hand-written managed policy referenced by ARN.

- **`identity.directSecretReads`** — the application secrets the tenant's pods
  read through the pod role, granted by exact name with no wildcard. Entries are
  prefix-relative to `<platform>/<environment>/`.

- **`attribution.operators`** — per-session human attribution. A non-empty list
  provisions a session role carrying the named human as STS `SourceIdentity` plus
  apiserver impersonation, so an agent's AWS and Kubernetes actions both
  attribute to a person. There is no boolean: the list _is_ the switch.

The chart rejects a declaration the CRD or the tofu module would, at render time
rather than at admission or apply — a name over the composed-resource budget, a
config block that does not match its kind, a `keyValue` store with no partition
key, an unquoted `type: N` (a YAML boolean), `eventBridgeScheduler` with no queue
to send to, a secret read that already carries its own prefix.
`scripts/check-chart-crd-parity.py` asserts both halves: that the chart can emit
every field the CRD defines, and that it still refuses the ones it should.

## Personas

The `platform.persona` field drives downstream defaults across `ModelGateway` (preferred model family), Grafana dashboard panels, and CLI scaffold output. Valid values:

- `sales-ops` — Anthropic Claude family; objection-handling + research agents
- `support` — Meta Llama family; ticket-summarization + KB-article agents
- `finance` — Amazon Nova Pro family; financial-memo + reconciliation agents
- `ops` — Mistral family; on-call summarizer + runbook agent
- `founder` — Claude Sonnet; strategy + memo agent
- `eng` — Claude Sonnet; ADR + code-review agent
- `marketing` — Claude Haiku for volume; campaign-brief + copy agents
- `legal` — Claude Sonnet with mandatory HIPAA + PII guardrails
- `generic` — default; no persona tilt

Choosing a persona at `helm install` (or `agentctl platform new`) time is the only mandatory configuration step.
