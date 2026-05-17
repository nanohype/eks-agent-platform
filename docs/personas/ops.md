# Ops / SRE persona

```bash
agentctl platform new --name sre --tenant acme --persona ops --monthly-usd 1500
```

Default agent `oncall-summarizer` on Mistral Large summarizes incident timelines into runbook-update candidates.

Add a triage-router agent that classifies incoming alerts:

```yaml
- name: triage
  systemPrompt: |
    Classify the incoming alert into: page-now | page-business-hours | auto-ack | dedupe.
    Output JSON with: classification, confidence (0-1), reasoning.
  modelRoute: primary
```

Watch the **Ops** Grafana dashboard for SQS depth (alert backlog), eval scores (regression on classifier), and p99 Bedrock latency (model degradation).
