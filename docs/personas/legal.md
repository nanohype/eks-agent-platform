# Legal persona

```bash
agentctl platform new --name legal --tenant acme --persona legal --monthly-usd 1000
```

**HIPAA defaults are ON** for this persona. The scaffold sets:

- `Platform.spec.compliance.hipaa = true` — Object Lock compliance mode on the artifacts bucket, no cross-region inference, mandatory PII Guardrail
- `BudgetPolicy.spec.killSwitchEnabled = true` — non-negotiable
- Mandatory `GuardrailPolicy` with PII detection (block, not anonymize) on output

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
