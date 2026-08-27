# Personas

Per-persona quickstarts. Each guide opens with what the persona can _do today_ — not concepts.

| Persona                     | Primary use cases                                             | Default model              |
| --------------------------- | ------------------------------------------------------------- | -------------------------- |
| [sales-ops](./sales-ops.md) | Objection handling, deal research, sales playbooks            | Claude Sonnet 4.6          |
| [support](./support.md)     | Ticket summarization, KB-article drafting, escalation routing | Claude Haiku 4.5           |
| [finance](./finance.md)     | Financial memos, reconciliation, forecast commentary          | Claude Sonnet 4.6          |
| [marketing](./marketing.md) | Campaign briefs, copy variants, multi-platform publishing     | Claude Sonnet 4.6          |
| [ops](./ops.md)             | On-call summarization, runbook updates, incident postmortems  | Claude Sonnet 4.6          |
| [founder](./founder.md)     | Strategy memos, board updates, OKR drafts                     | Claude Sonnet 4.6          |
| [eng](./eng.md)             | ADR drafting, code review, diagram generation                 | Claude Sonnet 4.6          |
| [legal](./legal.md)         | Policy review, compliance gap analysis (HIPAA on by default)  | Claude Sonnet 4.6          |

Each guide is a directed path from `agentctl platform new --persona <p>` to producing the first artifact the persona cares about, with Grafana dashboard call-outs and the three or four CRDs you'll edit in practice.
