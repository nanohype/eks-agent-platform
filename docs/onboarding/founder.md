# Onboarding — Founder / Exec

You get a tenant tuned for **strategy sparring + market research + quick lookups**. Three routes (deep / fast / image) and three agents: `sparring-partner`, `market-researcher`, `quick-lookup`.

## What this gets you

A non-sycophantic thinking partner. The default system prompts push back on the premise before validating it — designed so you get useful pushback in the 10 minutes before a board call, not validation of whatever you happened to say.

Single-user workload, scales 0-1. Budget **500 USD/mo** — attention-bounded volume means this is hard to overrun unless you really try.

## 10-minute quickstart

```bash
agentctl tenant init founder-personal \
  --persona founder \
  --schedule '0 8 * * 1' \
  | kubectl apply -f -

kubectl wait --for=condition=Aggregated tenant/founder-personal --timeout=5m
```

Replace `founder-personal` with whatever names you actually want (e.g. your own first name).

## Common workflows

- **Strategy spar** — "We should pivot to enterprise. Push back." Get the trade-offs, not the encouragement.
- **Market brief** — "Mid-market platform engineering teams" — 200-word brief: ICP, buyer persona, top 3 competitors, regulatory shifts, two non-obvious risks.
- **Quick lookup** — "What's the typical SaaS gross margin for a vertical with $5M ARR?" — 2-sentence answer, no preamble.
- **Pre-board prep** — paste your slide titles, ask the sparring-partner "what would a sharp board member ask?" — three pointed questions per slide.

## Integration shape

```
Slack DM → /agent <prompt> → SQS → AgentFleet → Slack reply
   OR
agentctl chat (CLI) → direct gateway call
```

The Slack integration is a few-line Lambda; out of scope for the platform itself. The simplest path is direct CLI via `kubectl run -n tenants-founder-personal chat ...`.

## Cost expectations

Heavy daily use ≈ **$50-80/month**. Default 500 USD/mo cap is generous for personal use. Tune down to $100 if you want a tight feedback loop on your own usage.

## Why non-sycophantic matters

Validation feels good. It's also useless if you wanted feedback. The default system prompts include "challenge the premise before validating", "don't sycophant", "give an opinion with the trade-offs explicit". You'll get more friction than a generic chat UI — that's the point.
