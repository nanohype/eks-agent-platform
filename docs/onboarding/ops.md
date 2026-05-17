# Onboarding — Operations / Platform

You get a tenant tuned for **incident response + postmortem drafting**. Single agent (`incident-summarizer` on Sonnet, 30 rpm). The fleet scales 1-3 pods on demand.

## What this gets you

A status-update + postmortem assistant that ingests CloudWatch alarms + Slack thread + runbook references and produces:

- a single-paragraph status update for #incidents (lead with impact),
- a blameless postmortem draft once the incident is resolved.

Designed for on-call humans — saves you the cognitive overhead of writing the status update at 03:00 when you should be focused on the fix.

Budget cap: **1500 USD/mo**. SOC2 baseline enforced.

## 10-minute quickstart

```bash
agentctl tenant init acme-ops \
  --persona ops \
  --slack '#oncall' \
  --schedule '0 7 * * *' \
  | kubectl apply -f -

kubectl wait --for=condition=Aggregated tenant/acme-ops --timeout=5m
```

## Integration shape

```
PagerDuty webhook → SQS → AgentFleet (incident-summarizer)
                              ↓
                        status update text
                              ↓
                         Slack #incidents

After incident resolved (manual):
  Slack /agentctl postmortem <incident-id> → fleet drafts → Notion
```

The PagerDuty → SQS Lambda is yours. The postmortem trigger is a manual Slack slash-command in the simplest case.

## Common workflows

- **Status update generation** — every alarm batch produces a one-paragraph #incidents update.
- **Postmortem drafting** — collect the incident timeline, paste in, get a blameless draft with sections (impact, timeline UTC, root cause, contributing factors, what went well, what didn't, action items).
- **Runbook synthesis** — given the on-call wiki + the actual recovery steps from the last 5 incidents, regenerate the runbook with what actually worked.
- **Eval regression** — daily check `incident-summary-has-impact` confirms the agent still leads with impact (not with timeline or technical jargon).

## Cost expectations

~10 incidents/month at 5 status updates each = 50 invocations. Sonnet at modest prompt sizes ≈ **$50/month** typical. Bursty patterns (a single big incident) might spike to $200. 1500 USD/mo cap covers a really bad month + the agentctl experimentation overhead.

## Why this matters when paged

The agent isn't the responder — you are. But it removes the "I have to write the customer-facing update right now" tax from your cognitive load during the incident. You fix the thing; the agent writes about it.
