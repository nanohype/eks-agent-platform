# Marketing persona

```bash
agentctl platform new --name marketing-team --tenant acme --persona marketing --monthly-usd 2500
```

Default agent is `campaign-brief` on Claude 3.5 Haiku (high volume, low-latency, cost-efficient). Outputs structured briefs in a 5-section format.

Add copy-variant generation:

```yaml
- name: copy-variants
  systemPrompt: |
    Given a campaign concept, produce 4 copy variants:
    1. headline (≤8 words), 2. subheadline (≤16 words),
    3. CTA (≤4 words), 4. body (≤80 words).
  modelRoute: primary
```

Marketing reviewers tend to find subtle voice problems before evals do — when they do, add the example to `EvalSuite.spec.cases` as a regression test.
