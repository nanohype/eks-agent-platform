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
