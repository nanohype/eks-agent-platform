# Runbook — import an open-weight model

How to bring an open-weight model onto the platform through Bedrock Custom Model
Import and route a tenant to it — no GPU nodes, one inference path to govern.
This is a deliberate, infrequent, account-level act (not per-tenant
self-service): a human imports the model out of band, and a `ModelGateway`
route references the resulting ARN. The sequence below was run end-to-end on
2026-07-23 (a Qwen2.5-0.5B import in us-west-2); the gotchas it surfaced are
called out inline.

Persona: ops. Do the steps in order.

---

## When

- A tenant needs a model that isn't a Bedrock foundation model, and its
  architecture is one Custom Model Import supports.
- You are **not** trying to serve embeddings — Custom Model Import does not
  support embedding models. Embeddings stay on a Bedrock foundation embedding
  model (e.g. Titan). Retrieval tenants keep generating embeddings there.

## Prerequisites

- The **`model-import`** landing-zone component is applied in the target
  account + region. It provisions the staging bucket and the Bedrock import
  service role, and publishes both to SSM:
  - `/eks-agent-platform/model-import/staging_bucket_name`
  - `/eks-agent-platform/model-import/import_role_arn`
- The model is in **Hugging Face weights format** and its architecture is
  supported: Mistral, Mixtral, Flan (T5), Llama 2/3/3.1/3.2/3.3 + Mllama,
  GPTBigCode, or Qwen2/2.5/Qwen3. Anything outside the list can't be imported.
- Custom Model Import runs in `us-west-2`, `us-east-1`, `us-east-2`, or
  `eu-central-1`. Import into the same region the model will be served from —
  an imported model is an account+region resource.
- Confirm your import complies with the model's license.

## Steps

Resolve the substrate the `model-import` component published:

```bash
BUCKET=$(aws ssm get-parameter --name /eks-agent-platform/model-import/staging_bucket_name --query Parameter.Value --output text)
ROLE=$(aws ssm get-parameter --name /eks-agent-platform/model-import/import_role_arn --query Parameter.Value --output text)
```

**1. Stage the weights.** Upload the Hugging Face files (`config.json`,
`*.safetensors`, `tokenizer*.json`, `generation_config.json`, `vocab.json`,
`merges.txt`, …) under a prefix in the staging bucket:

```bash
aws s3 cp ./my-model/ "s3://$BUCKET/my-model/" --recursive
```

**2. Start the import job.**

```bash
aws bedrock create-model-import-job \
  --job-name my-model-$(date +%s) \
  --imported-model-name my-model \
  --role-arn "$ROLE" \
  --model-data-source "{\"s3DataSource\":{\"s3Uri\":\"s3://$BUCKET/my-model/\"}}"
```

> **Gotcha — "The provided role ARN is invalid".** If the `model-import`
> component was applied only moments ago, the import role's trust may not have
> propagated yet, and Bedrock returns this misleading `ValidationException`. It
> is IAM eventual consistency, not a policy error — wait a few minutes and
> retry. (The trust itself is correct: `SourceAccount` + a `model-import-job`
> `SourceArn`, per AWS's guidance.)

> **Gotcha — "request a quota increase for Concurrent model import jobs".** The
> `Concurrent model import jobs` quota is a hard **1** and is **not adjustable**.
> This `ServiceQuotaExceededException` means another import is already running,
> not that you need to raise a quota — there is nothing to request. Wait for the
> in-flight job to finish, then retry.

**3. Wait for completion**, then capture the imported-model ARN:

```bash
aws bedrock get-model-import-job --job-identifier <jobArn> --query status        # → Completed
ARN=$(aws bedrock get-imported-model --model-identifier my-model --query modelArn --output text)
```

**4. Route a tenant to it.** Add an imported route to the tenant's
`ModelGateway` (or the chart values that render it):

```yaml
routes:
  - name: oss
    modelSource: imported
    modelId: arn:aws:bedrock:us-west-2:<account>:imported-model/<id>
```

`modelSource: imported` tells the operator this is a Custom Model Import route:
`modelFamily` and `crossRegionProfile` are rejected on it, and `modelId` must be
the ARN. The route serves through the ordinary `bedrock-runtime` `InvokeModel`
path, so its invocations flow into model-invocation logging like any foundation
model.

## Governance

- **Cost is capacity-billed, not per-token.** You pay per active model copy per
  minute (Custom Model Units), over 5-minute windows from the first invocation;
  an idle model scales its copies toward zero and accrues no inference charge.
  A stored imported model also carries a small per-model storage charge — delete
  models you no longer serve.
- **Make imported spend visible to the kill-switch.** Because CMU billing has no
  per-token rate, the cost publisher prices imported invocations at $0 by
  default (they stay observable via the `UnpricedInvocations` metric). To bring
  imported spend into the `EstimatedInvocationCostUsd` signal the budget
  kill-switch reads, set `imported_model_estimate_usd_per_mtokens` on the
  `cost-pipeline` component to a conservative per-token estimate. It is a
  threshold knob, not finance-grade — CUR stays authoritative for the bill.
- **Guardrails.** Inline Bedrock guardrails are foundation-model-only, so an
  imported route can't carry one. The gateway drops the inline guardrail and
  raises an `ImportedRouteGuardrailUnenforced` condition naming the route rather
  than serving it silently unguarded; enforcement via `ApplyGuardrail` is a
  tracked follow-up. Check the condition before pointing sensitive traffic at an
  imported route.

## Non-goal

The platform's model path is **Bedrock, including for open weights**. In-cluster
model serving is not a supported shape, and GPU nodes are not part of the model
story — Custom Model Import gives open-weight flexibility through the ordinary
Bedrock runtime with no accelerators. Adding a GPU-adjacent feature does not
reopen in-cluster serving.

## Teardown

```bash
aws bedrock delete-imported-model --model-identifier my-model   # stops the storage charge
aws s3 rm "s3://$BUCKET/my-model/" --recursive                  # staged weights are re-uploadable
```

Remove the imported route from the tenant's `ModelGateway` first, so nothing
routes to a model that's about to disappear.
