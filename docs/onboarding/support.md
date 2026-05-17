# Onboarding — Customer Support

You get a tenant tuned for **inbound ticket triage + escalation**. Two agents: `triager` on the fast Haiku route (120 rpm — classifies P0..P3 in JSON) and `escalator` on Sonnet (30 rpm — writes the 3-bullet handoff to the human).

## What this gets you

The fleet sits behind your existing ticket intake (Zendesk webhook → SQS, or similar). It auto-routes, sets priority, and drafts the escalation summary — your humans pick up at the "human needs to act" stage instead of starting from scratch on every ticket.

Budget cap: **1500 USD/mo**. SOC2 baseline enforced (invocation logging on, kill-switch on).

## 10-minute quickstart

```bash
agentctl tenant init acme-support \
  --persona support \
  --slack '#support-agents' \
  --schedule '0 6 * * *' \
  | kubectl apply -f -

kubectl wait --for=condition=Aggregated tenant/acme-support --timeout=5m
agentctl tenant get acme-support
```

Edit `agents[0].systemPrompt` (the triager) to match your category taxonomy (replace `[billing, technical, account, feature-request]` with your real categories). The default scheme is intentionally generic.

## Integration shape

```
Zendesk webhook → SQS queue → AgentFleet (triager)
                                  ↓
                            { urgency, category, escalate? }
                                  ↓
                        If escalate: AgentFleet (escalator)
                                          ↓
                                   3-bullet handoff
                                          ↓
                              Slack #support-escalations
```

You bring the Zendesk webhook → SQS Lambda. The fleet processes; downstream Slack posting is your existing channel automation.

## Common workflows

- **P0 detection** — triager produces JSON with `urgency=P0` + `escalate=true`. Your downstream picks this up and pages the on-call.
- **Volume burst absorption** — KEDA scales the triager fleet up to 5 pods on SQS depth so a viral incident doesn't queue.
- **Daily eval regression** — two cases (`triage-p0-detected`, `triage-billing-routed`) catch prompt drift before it hits production tickets. 0.90 pass-threshold (higher than sales-ops because misroutes are more user-visible).
- **Escalator handoffs** — the escalator's 3-bullet summary lands in the handoff doc your humans read first. They start from "what was tried" instead of "what is this".

## Cost expectations

Haiku at 120 rpm = $0.001/1k input + $0.005/1k output. 10k tickets/month at avg 500 input + 200 output tokens ≈ **$15/month** on triage alone. The escalation path (Sonnet) bumps that to ~$200/month at a 20% escalation rate. 1500 USD/mo cap gives you ~5x headroom.
