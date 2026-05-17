# Onboarding — Marketing

You get a tenant tuned for **copy + image generation**. Two routes: `copy` (Sonnet, 60 rpm) and `image` (Amazon Nova Pro, 10 rpm). Default agent: `copy-writer` — writes 3 variants with explicit trade-offs for each.

## What this gets you

A brand-voice-aware copy assistant + an image generation route for thumbnails/banners. Default prompt enforces "show 3 variants with trade-offs" — designed so you pick the best, not so the agent picks for you.

Budget cap: **1500 USD/mo**. Image generation is the cost driver; copy is cheap.

## 10-minute quickstart

```bash
agentctl tenant init acme-marketing \
  --persona marketing \
  --slack '#marketing-team' \
  --schedule '0 6 * * *' \
  | kubectl apply -f -

kubectl wait --for=condition=Aggregated tenant/acme-marketing --timeout=5m
```

Edit `agents[0].systemPrompt` to embed your brand voice guidelines (e.g., "voice: confident but not boastful; never use 'revolutionary'").

## Integration shape

```
Notion / Figma → manual paste OR
Email queue → SQS → AgentFleet → variants written back to Notion page
```

For most marketing workflows the input is human-curated (campaign brief, voice doc) so the input path is paste-based. The output goes back to your existing tool — Notion/Figma/CMS.

## Common workflows

- **Campaign copy** — paste brief, get 3 variants of subject line + 3 of body + 3 of CTA.
- **Brand voice spotcheck** — paste draft, ask "does this match the voice doc above?" — agent flags specific phrases.
- **Image generation** — Nova Pro at 10 rpm for thumbnails. Per-image cost is meaningful so the lower rate limit is intentional.
- **A/B test variant generation** — request 5 short variants for an A/B; agent enforces meaningful differences (not synonym swaps).

## Cost expectations

Copy at moderate volume ≈ **$100/month**. Image generation can quickly become the budget driver — at $0.08/image and 10 rpm sustained, you can burn the budget in a day if you're not careful. 1500 USD/mo cap with rate-limit at 10 rpm gives you ~18k images/month — far beyond typical marketing use.

## Voice doc as the source of truth

Spend more time on the system prompt than on individual queries. A well-flexed system prompt with explicit brand voice rules + 3-5 do/don't examples produces dramatically better copy than tweaking each query.
