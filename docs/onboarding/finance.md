# Onboarding — Finance

You get a tenant tuned for **month-end close + anomaly review**. Single agent (`month-end-helper` on Sonnet, 20 rpm) since finance workloads are batch-bursty around month-end and otherwise quiet.

## What this gets you

A reconciliation assistant that ingests transaction batches, flags >3σ anomalies from the monthly mean, and produces audit-ready commentary. Designed to live in your existing close workflow — not replace your ERP, just summarize/explain.

Budget cap: **1000 USD/mo** (modest — finance volume is human-bounded). SOC2 baseline enforced.

## 10-minute quickstart

```bash
agentctl tenant init acme-finance \
  --persona finance \
  --slack '#finance' \
  --schedule '0 22 1 * *' \
  | kubectl apply -f -

kubectl wait --for=condition=Aggregated tenant/acme-finance --timeout=5m
```

The `--schedule '0 22 1 * *'` runs the eval suite on the 1st of every month at 22:00 UTC — i.e., as month-end close kicks off.

Edit `agents[0].systemPrompt` to match your chart of accounts (the default refers generically to "transaction batches"; you'll want to name specific GL accounts or cost centers).

## Integration shape

```
Stripe / NetSuite / QuickBooks → daily export → S3
                                                  ↓
                              Cron (Argo Workflow) → AgentFleet
                                                          ↓
                                      anomaly report + commentary
                                                          ↓
                                Notion / Confluence page (your existing)
```

The eval-runtime + Argo Workflows substrate is the orchestration; the finance tenant just uses it. You bring the data export + the destination page.

## Common workflows

- **Month-end close** — feed the batch, get the anomaly report + the human-readable commentary explaining each flag.
- **Anomaly explainer** — given a single flagged row, the agent describes what's unusual relative to history.
- **Audit support** — invocation logging is mandatory (SOC2 baseline), so every model call is preserved 365d in S3 + 1y in CloudWatch with KMS encryption. Auditors get a CSV of every reconciliation conversation.
- **Quarterly compliance review** — `kubectl describe tenant acme-finance` shows the full audit chain (which Platform CRs exist, which budgets, which evals are passing).

## Cost expectations

Single-agent + 20 rpm + monthly bursty pattern = **$50-150/month** typical. 1000 USD/mo cap absorbs a year-end close that hammers the agent. The kill-switch fires at 120% of your monthly cap (so 1200 USD for the default), well above expected burn.
