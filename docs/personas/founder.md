# Founder persona

```bash
agentctl platform new --name founder --tenant acme --persona founder --monthly-usd 500
```

Default `strategy-memo` agent on Claude 3.5 Sonnet drafts strategy memos and pushes back on weak reasoning.

This is the lightest-footprint persona — usually one agent, low monthly budget, no scaling. The point isn't volume; it's a thinking partner with the company's full context loaded into the system prompt.

Common second agents:

- `board-deck-outliner` — section outlines for board decks, with hard length limits per section
- `okr-drafter` — draft OKRs from a goal statement
- `competitor-watch` — daily summary of competitor news (paired with a tool that scrapes RSS)

Watch the **Founder/Exec** Grafana dashboard for weekly spend trend and top initiatives by agent activity — that last panel is a leading indicator of which teams are actually using the platform.
