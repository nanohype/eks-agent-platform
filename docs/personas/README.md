# Personas

Per-persona quickstarts. Each guide opens with what the persona can _do today_ — not concepts.

| Persona                     | Primary use cases                                             | Default model family        |
| --------------------------- | ------------------------------------------------------------- | --------------------------- |
| [sales-ops](./sales-ops.md) | Objection handling, deal research, sales playbooks            | Anthropic Claude 3.5 Sonnet |
| [support](./support.md)     | Ticket summarization, KB-article drafting, escalation routing | Meta Llama 3.1 70B          |
| [finance](./finance.md)     | Financial memos, reconciliation, forecast commentary          | Amazon Nova Pro             |
| [marketing](./marketing.md) | Campaign briefs, copy variants, multi-platform publishing     | Anthropic Claude 3.5 Haiku  |
| [ops](./ops.md)             | On-call summarization, runbook updates, incident postmortems  | Mistral Large               |
| [founder](./founder.md)     | Strategy memos, board updates, OKR drafts                     | Anthropic Claude 3.5 Sonnet |
| [eng](./eng.md)             | ADR drafting, code review, diagram generation                 | Anthropic Claude 3.5 Sonnet |
| [legal](./legal.md)         | Policy review, compliance gap analysis (HIPAA on by default)  | Anthropic Claude 3.5 Sonnet |

Each guide is a directed path from `agentctl platform new --persona <p>` to producing the first artifact the persona cares about, with Grafana dashboard call-outs and the three or four CRDs you'll edit in practice.
