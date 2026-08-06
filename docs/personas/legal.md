# Legal persona

```bash
agentctl platform new --name legal --tenant acme --persona legal --monthly-usd 1000
```

**HIPAA is declared** for this persona. The scaffold sets:

- `Platform.spec.compliance.hipaa = true` — declares the Platform in HIPAA scope. `cloudgov platform audit` holds it to its Tenant's posture; it configures nothing on its own, and a HIPAA workload needs a BAA with AWS
- `BudgetPolicy.spec.killSwitchEnabled = true` — non-negotiable, and the one thing `cloudgov platform audit` will fail a `soc2` Platform for

Set the guardrail yourself before this persona handles anything real. The scaffold does not attach one: give every route a `ModelGateway.spec.routes[].guardrailRef` (or set the gateway's `defaultGuardrailRef`) pointing at a Guardrail with PII detection on output set to block rather than anonymize.

Default agent `policy-reviewer` reads policy text and flags clauses that conflict with jurisdiction-specific requirements (configure jurisdiction in the system prompt).

Add a `contract-redliner` for routine NDA / MSA review:

```yaml
- name: contract-redliner
  systemPrompt: |
    You redline contracts against the company's standard playbook (see attached).
    Output the issue, the suggested redline, and the playbook section referenced.
  modelRoute: primary
```

Legal almost never tolerates a wrong answer being attributed to "the model." Treat `EvalSuite` like a regression test suite — every flagged miss becomes a case.
