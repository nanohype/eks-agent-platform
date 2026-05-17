# Sales-ops persona

You run revenue operations and you want agents handling objection prep, deal research, account briefs.

```bash
agentctl platform new --name revenue --tenant acme --persona sales-ops --monthly-usd 3000
```

Default agent is `objection-handler` on Claude 3.5 Sonnet. Add `deal-researcher` and `account-brief` for full coverage. Outputs land in your SQS queue; pipe to Salesforce/HubSpot custom objects via your existing integration layer (no eng work required — your existing tooling already speaks SQS).

## What you'll touch

- `AgentFleet.spec.agents[*].systemPrompt` — your IP lives here. Iterate.
- `EvalSuite.spec.cases` — every objection your team flags as "the agent got this wrong" becomes a case. The daily run keeps quality from drifting.
- `BudgetPolicy.spec.monthlyUsd` — set realistic; the kill-switch is your friend, not an obstacle.

## What you won't touch

The IRSA role, the Bedrock policies, the cross-region failover wiring, the WAF rules. Platform team owns those.
