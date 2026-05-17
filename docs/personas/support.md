# Support persona

You run customer support. You want ticket summaries, KB-article drafts, and escalation routing.

```bash
agentctl platform new --name support-team --tenant acme --persona support --monthly-usd 4000
```

Default model family is Meta Llama 3.1 70B — cost-efficient for the volume support pulls. Default agent `ticket-summarizer` reads from your tickets queue and writes summary + suggested next-step to the output queue.

Add a `kb-drafter` agent for synthesizing recurring tickets into KB articles:

```yaml
- name: kb-drafter
  systemPrompt: |
    You synthesize 5+ similar support tickets into a single KB article.
    Output Markdown with sections: Symptom, Root cause, Resolution, Verification.
  modelRoute: primary
```

Pipe the output to your KB system (Notion, Confluence, Zendesk Guide) via your existing integration tooling.

## Volume-aware tuning

Support sees spiky volume. The default `scaling.max=5` will not be enough during incident windows. Bump to 20+ and let KEDA scale on SQS depth:

```yaml
spec:
  scaling:
    enabled: true
    min: 2
    max: 30
    queueDepthTrigger: 5
```

Cost stays in check via `BudgetPolicy` — kill-switch protects against runaway invocation during an incident bug.
