# Onboarding — Sales Operations

You get a tenant tuned for **outbound research + lead enrichment**. The default fleet has a `prospector` agent on the `research` route (Sonnet, 60 rpm) and an `enricher` agent on the `enrichment` route (Nova Lite, 120 rpm) — fast and cheap for the bulk lookups, deep and considered for the prospect briefs.

## What this gets you

A dedicated agent fleet that lives next to your existing CRM + outreach tooling. It scales on SQS depth (your existing prospecting queue), so when your team feeds it 200 leads on Monday morning it spins up workers; when the queue drains, it scales back to one.

Budget cap: **2500 USD/mo**. The kill-switch fires at 120% — that's $3000 of Bedrock spend before the operator detaches your tenant role.

## 10-minute quickstart

```bash
agentctl tenant init acme-sales \
  --persona sales-ops \
  --slack '#sales-ops-agents' \
  --schedule '0 6 * * *' \
  | kubectl apply -f -

# Wait for ready
kubectl wait --for=condition=Aggregated tenant/acme-sales --timeout=5m
agentctl tenant get acme-sales
```

Edit the generated `agents[].systemPrompt` to match your ICP language (replace "B2B prospects" with the specific market you sell into).

## Integration shape

```
Salesforce / HubSpot → SQS queue → AgentFleet
                                      ↓
                                 ModelGateway (Bedrock)
                                      ↓
                          Per-prospect brief written to S3
                                      ↓
                                 Salesforce write-back Lambda
```

The fleet's `scaling.queueUrl` points at your SQS queue. The operator's IRSA role grants `sqs:GetQueueAttributes` so KEDA can scale by queue depth. Writing the briefs back is your existing Lambda — out of scope for this platform.

## Common workflows

- **Prospect a list** — drop CSV in the SQS queue, fleet writes per-row briefs to `s3://<eval-reports>/<tenant>/runs/...`.
- **Enrich a contact** — same pattern, lighter prompt, Nova Lite for unit cost.
- **Tune the prompts** — edit `AgentFleet.spec.agents[].systemPrompt`, re-apply, fleet rolls over with the standard agentgateway rolling-update.
- **Catch drift** — the daily 06:00 UTC `EvalSuite` regression-tests two cases (prospector-personas-present, enricher-json-shape). If pass-rate drops below 0.85 the Argo Rollouts AnalysisTemplate gates the next canary.

## Cost expectations

A 200-lead daily prospect run with the default Sonnet prompt costs roughly **30-50 USD**, varying with prompt size. The 2500 USD/mo default budgets for ~1500 leads/month at deep + 50k contact enrichments at light.
