# Onboarding — Engineering

You get a tenant tuned for **code review + diff explanation + design-doc sparring**. Single agent (`code-reviewer` on Sonnet, 60 rpm).

## What this gets you

A second-opinion code reviewer that catches the boring stuff (off-by-one, missing nil checks, unscoped IAM) so your humans focus on the design decisions. Default system prompt: "Flag only things that materially matter. No nitpicks."

Budget cap: **2000 USD/mo** — engineering volume scales with team size and CI frequency.

## 10-minute quickstart

```bash
agentctl tenant init platform-eng \
  --persona eng \
  --slack '#platform-eng' \
  | kubectl apply -f -

kubectl wait --for=condition=Aggregated tenant/platform-eng --timeout=5m
```

No `--schedule` flag — eval suites for eng tenants are usually manual since CI integration drives the real exercise.

## Integration shape

```
GitHub PR webhook → SQS → AgentFleet
                              ↓
                       Review comment on PR
```

The GitHub App is yours (off-the-shelf or built). The fleet receives the diff + relevant context (changed files + nearest tests), responds with structured review.

Alternative: CI step.

```
.github/workflows/review.yml:
  - run: curl -sX POST $GATEWAY_URL/v1/agents/platform-eng/code-reviewer \
           -d "$DIFF_PAYLOAD"
```

## Common workflows

- **PR review** — second opinion on every PR; human reviewer focuses on design, agent on style/safety.
- **Design doc sparring** — paste the doc, ask "what would a sharp staff engineer push back on?".
- **Migration safety** — for risky migrations (NOT NULL on a 50M-row table, etc.), get the agent's read on lock contention + rollback path.
- **Spike research** — "compare LiteLLM vs agentgateway for our shape" — get the table, not the recommendation.

## Cost expectations

A team of 10 with ~50 PRs/week and the agent reviewing each ≈ **$300-500/month**. The 2000 USD cap accommodates ~3x that volume + experimentation. Kill-switch at $2400.

## Why "no nitpicks" matters

The agent will flag what an experienced reviewer would flag. It won't tell you about your trailing-whitespace policy or your enum-case convention — your linter does that. You get value from the agent on the things linters can't catch: missing validation at trust boundaries, sneaky transaction semantics, races that span goroutines.
