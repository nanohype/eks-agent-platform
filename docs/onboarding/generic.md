# Onboarding — Generic

Use the `generic` persona when your team doesn't fit one of the seven role-specific personas (sales-ops, support, finance, ops, founder, eng, marketing, legal) and you want a neutral starting point you'll customize before going to prod.

You get a single agent (`assistant` on Sonnet, 30 rpm) with a vanilla "you are a helpful assistant" system prompt. Use this for prototypes, dev-environment scratch tenants, or as the starting point you'll mutate into a role-specific tenant once the workflow shape is clearer.

Budget cap: **1000 USD/mo**. No compliance baseline enforced (override if your tenant needs SOC2 or HIPAA).

## 10-minute quickstart

```bash
agentctl tenant init my-prototype \
  --persona generic \
  | kubectl apply -f -

kubectl wait --for=condition=Aggregated tenant/my-prototype --timeout=5m
agentctl tenant get my-prototype
```

## What to customize before going to prod

Generic defaults are intentionally bland. The minimum-viable customization:

1. **Replace the system prompt.** `"You are a helpful assistant"` is a placeholder. Write the actual instructions your agent needs (role, output format, constraints).
2. **Pick a real model route name.** The default `primary` route works but a domain-meaningful name (`triage`, `research`, `incident`) is what you want once the use case is clear.
3. **Set compliance flags** if your workload handles regulated data (SOC2 for audit-logged invocations; HIPAA for stricter object-lock + cross-region restrictions).
4. **Tune the budget** based on actual usage. Default 1000 USD is conservative; sustained workloads typically need higher.

If your customizations push the tenant toward a specific persona (e.g. the prompt becomes triage-shaped), consider re-scaffolding with the right persona for better defaults — `agentctl tenant init` is idempotent on the same name and will overwrite the scaffold.

## When NOT to use generic

If your role IS one of the seven specific personas, use that playbook. The persona defaults aren't just labels — system prompts, budget defaults, model choice, scaling bounds, and compliance baseline all differ. Starting from generic and reverse-engineering your way toward "support-shaped" is more work than starting from `support`.
