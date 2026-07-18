# Engineering persona

You're a platform/SRE/backend engineer at a company running on this platform. You want a fleet of agents that draft ADRs, review code, generate diagrams, and shoulder the boilerplate parts of design.

## What you can do in 10 minutes

```bash
agentctl platform new --name eng-team --tenant acme --persona eng --monthly-usd 1500
kubectl apply -f eng-team/platform.yaml
```

Default agent is `adr-drafter`. Add agents as needed:

```yaml
spec:
  agents:
    - name: adr-drafter
      systemPrompt: |
        You draft Architectural Decision Records. Show trade-offs explicitly.
        Use the format: Status, Context, Decision, Consequences.
      modelRoute: primary
    - name: code-reviewer
      systemPrompt: |
        You review code diffs for security issues, race conditions, and missing
        error handling. Output a single comment per issue with file:line citations.
      modelRoute: primary
    - name: diagram-gen
      systemPrompt: 'You generate Mermaid or PlantUML diagrams from architecture descriptions.'
      modelRoute: primary
```

## CRDs you'll touch

- **`AgentFleet`** — most of your iteration is here. Prompts, agent additions, scaling tweaks.
- **`ModelGateway.spec.routes`** — add a `code-review` route on a different model if you want a second opinion on diffs.
- **`EvalSuite`** — add a case every time an agent produces a bad review or a wrong ADR. Eval-on-PR is your safety net.

## What the platform handles

- Bedrock invocation via IRSA, no keys
- Per-pod cost attribution (visible on Ops dashboard, joinable on `agents.platform`)
- Cross-region inference profile failover when set on the `ModelRoute`

## Notes

- Agent prompts live in the CR but should be version-controlled. The conventional pattern is a `prompts/` directory in your team's app repo that renders into the CR via Helm or Kustomize. Don't edit prompts in the live cluster.
- Eval gating ships on Argo Rollouts AnalysisTemplate; any change to `AgentFleet.spec.agents[*].systemPrompt` triggers a rollout that's gated by the suite's mean score.
