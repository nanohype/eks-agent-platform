# Finance persona

You're a finance lead at a company running on this platform. You want the agent fleet to draft financial memos, reconcile data, and surface anomalies — without you ever opening a YAML editor.

## What you can do in 10 minutes

```bash
agentctl platform new --name finance-team --tenant acme --persona finance --monthly-usd 2000
kubectl apply -f finance-team/platform.yaml
```

Wait for `Platform.status.phase == Ready` (about 60 seconds). Then:

1. Submit a memo request from your existing intake tool (Slack form, web form, an email-to-S3 pipe) into the SQS queue named `finance-team-intake` in your account. The default `financial-memo` agent processes it.
2. Read the result from the SQS queue named `finance-team-output` — or wire it to wherever your team reads memos (Confluence, Notion, Google Docs).
3. Watch the **Finance** Grafana dashboard for daily/weekly spend, top-N models, forecast vs. budget.

## The three CRDs you'll actually touch

- **`BudgetPolicy`** — the monthly USD cap and the kill-switch toggle. Increase `monthlyUsd` when the team grows; never lower the kill-switch below 100%.
- **`AgentFleet`** — add or modify agents. Each agent has a `systemPrompt` you'll iterate on, and a `modelRoute` you can swap to escalate quality (Claude Haiku → Sonnet) or save cost (Sonnet → Haiku).
- **`EvalSuite`** — your `cases` list. Add a case every time the team catches a bad memo; the daily run gates whether new prompts ship.

Everything else is platform team's job: `ModelGateway`, `Platform`, the IRSA wiring, the budgets pipeline. The operator manages the per-tenant IAM role directly from `Platform.spec.identity` — finance never touches it.

## What the platform handles for you

- Bedrock access (no API keys to manage)
- Per-invocation cost tracking (visible on the Finance dashboard)
- PII redaction on output via the baseline Guardrail
- Kill-switch at 120% of budget — Bedrock access detaches from the tenant role; agents stop invoking until SRE re-enables
- Daily smoke-test eval — `EvalSuite` with the `smoke-test` case from the CLI scaffold runs at 06:00 UTC

## Common adjustments

Switching to Nova Pro for higher-volume reconciliation (cheaper than Claude for structured-data tasks):

```yaml
spec:
  routes:
    - name: primary
      modelFamily: amazon-nova
      modelId: amazon.nova-pro-v1:0
```

Adding a reconciliation agent on top of the default memo agent:

```yaml
spec:
  agents:
    - name: financial-memo
      systemPrompt: '...'
      modelRoute: primary
    - name: reconciler
      systemPrompt: 'You reconcile NetSuite line items against bank statements.'
      modelRoute: primary
      replicas: 2
```

## Where to look when something's off

| Symptom                  | Where                                                                                                                         |
| ------------------------ | ----------------------------------------------------------------------------------------------------------------------------- |
| Memos look generic       | `EvalSuite.status.lastScore` — if < 0.85, the daily eval failed; check the report in S3 at the URL in `.status.lastReportUrl` |
| Cost spike               | Finance dashboard → "Daily spend by Platform"; cross-reference with `BudgetPolicy.status.percentOfBudget`                     |
| Agent stopped responding | `Platform.status.phase == Suspended` means kill-switch fired; check the EventBridge archive in the platform account           |
| Slow responses           | Ops dashboard → "Bedrock InvokeModel p99 latency" for your model_id                                                           |
