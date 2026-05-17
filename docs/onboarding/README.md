# Onboarding playbooks

Persona-routed quickstarts for getting your team's tenant on the eks-agent-platform.

| Persona               | Playbook                       | Default budget | Default model      |
| --------------------- | ------------------------------ | -------------- | ------------------ |
| Sales Operations      | [sales-ops.md](./sales-ops.md) | 2500 USD/mo    | Sonnet + Nova Lite |
| Customer Support      | [support.md](./support.md)     | 1500 USD/mo    | Haiku + Sonnet     |
| Finance               | [finance.md](./finance.md)     | 1000 USD/mo    | Sonnet             |
| Operations / Platform | [ops.md](./ops.md)             | 1500 USD/mo    | Sonnet             |
| Founder / Exec        | [founder.md](./founder.md)     | 500 USD/mo     | Sonnet + Haiku     |
| Engineering           | [eng.md](./eng.md)             | 2000 USD/mo    | Sonnet             |
| Marketing             | [marketing.md](./marketing.md) | 1500 USD/mo    | Sonnet + Nova Pro  |
| Legal                 | [legal.md](./legal.md)         | 800 USD/mo     | Sonnet             |
| Generic (prototype)   | [generic.md](./generic.md)     | 1000 USD/mo    | Sonnet             |

All defaults are tunable. Every playbook starts with `agentctl tenant init` and walks the same six-resource onboarding (Tenant → Platform → Budget → Gateway → Fleet → Eval).

If your role isn't here, start from [eng.md](./eng.md) — it's the most general — and adjust budget + system prompts to fit.

## Local development

- [local-kx.md](./local-kx.md) — land the operator + a smoke-test tenant on the [`kx`](https://github.com/stxkxs/kx) kind cluster. Two modes: k8s-only (validates CR emission against real upstream CRDs) + bedrock (mounts laptop AWS creds onto agentgateway for end-to-end real Bedrock calls).
