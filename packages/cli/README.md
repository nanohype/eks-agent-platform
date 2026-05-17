# @eks-agent/cli — `agentctl`

CLI for declaring and managing eks-agent-platform tenants.

```bash
agentctl platform new \
  --name marketing-team \
  --tenant acme \
  --persona marketing \
  --monthly-usd 2500
```

Renders a directory containing a multi-document YAML with `Platform` + `BudgetPolicy` + `ModelGateway` + `AgentFleet` + `EvalSuite`. Apply with `kubectl apply -f marketing-team/platform.yaml`.

## Persona presets

| Persona     | Default model                | Default agent     |
| ----------- | ---------------------------- | ----------------- |
| `sales-ops` | Claude 3.5 Sonnet            | objection-handler |
| `support`   | Llama 3.1 70B                | ticket-summarizer |
| `finance`   | Nova Pro                     | financial-memo    |
| `marketing` | Claude 3.5 Haiku             | campaign-brief    |
| `ops`       | Mistral Large                | oncall-summarizer |
| `founder`   | Claude 3.5 Sonnet            | strategy-memo     |
| `eng`       | Claude 3.5 Sonnet            | adr-drafter       |
| `legal`     | Claude 3.5 Sonnet (HIPAA on) | policy-reviewer   |
| `generic`   | Claude 3.5 Sonnet            | assistant         |

Overrides are post-scaffold YAML edits; the CLI is for the happy path. For something more bespoke, install `charts/tenant` directly with custom values.

## Future commands

`agentctl agent new`, `agentctl eval run`, `agentctl platform suspend|resume`, and `agentctl budget show` are planned. The scaffolding is in `src/commands/`. `agentctl tenant list` and `agentctl tenant get` are already implemented.
