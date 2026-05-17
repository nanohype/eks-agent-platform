# Onboarding — Legal

You get a tenant tuned for **contract review + clause flagging**. Single agent (`contract-reviewer` on Sonnet, 15 rpm). Low rate-limit because contract reviews are bursty but small-volume.

## What this gets you

A contract review assistant that flags non-standard clauses, missing liability caps, and unbounded obligations. Always cites section numbers. Designed for the "first pass" — a lawyer still reviews, but they start from "here are the concerns" instead of cold-reading 30 pages.

Budget cap: **800 USD/mo**. SOC2 baseline enforced (every invocation logged + retained).

## 10-minute quickstart

```bash
agentctl tenant init acme-legal \
  --persona legal \
  --slack '#legal' \
  | kubectl apply -f -

kubectl wait --for=condition=Aggregated tenant/acme-legal --timeout=5m
```

Edit `agents[0].systemPrompt` to embed your standard playbook (e.g., your default MSA terms, your acceptable liability cap range, jurisdictions you'll/won't accept).

## Integration shape

```
DocuSign/CCM tool → manual upload OR webhook → S3 → AgentFleet
                                                       ↓
                                               clause flagging report
                                                       ↓
                                          Notion / Confluence page
                                                       ↓
                                           Lawyer's review queue
```

## Common workflows

- **Inbound MSA review** — paste counterparty draft, get the diff against your standard + flagged clauses.
- **NDA fast-track** — for low-risk standard NDAs, the agent confirms "matches our standard" with no flags → human spot-checks → sign.
- **Audit clause monitoring** — quarterly run across the contract library checking that all customer contracts have the audit-log clause; flag any missing.
- **Risk register update** — for any flagged contract, the agent drafts the entry for your risk register (counterparty, clause, exposure, recommended fix).

## Cost expectations

50 contracts/month at ~30k tokens each (mid-size contract) ≈ **$60-100/month**. Cap at 800 USD gives ~8x headroom for a quarter-end deal spike.

## Why agent-first, lawyer-second

Lawyers' time is the bottleneck; commodity clauses don't need senior attention. The agent does the boring first-pass scan so the human review is focused on the actually-novel terms. Risk: agent misses an unfamiliar pattern, lawyer misses it because they trusted the agent's summary. Mitigation: the agent cites section numbers — lawyers do spot-check the cited sections, not the whole doc, but they're checking the agent's _reasoning_, not skipping the doc.
