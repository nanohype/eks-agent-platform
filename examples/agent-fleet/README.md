# agent-fleet

A support-desk Platform that runs **two agents on two model routes** and **scales on SQS queue depth**:

- `triage` route → Claude Haiku 4.5 (cheap first-line classification), rate-limited to 120 req/min.
- `resolve` route → Claude Sonnet 4.6 (drafts the reply), rate-limited to 60 req/min.
- `triage-bot` agent pins the `triage` route; `resolver` pins `resolve` and references a `knowledge-base-search` kagent ToolServer.
- The fleet's KEDA `ScaledObject` scales 1→8 on the work queue's depth.

Model ids are the org defaults from `nanohype/standards/llm-policy.json`. Tools are referenced by name only — the `knowledge-base-search` `ToolServer` is delivered by `addons-ai-platform` + External Secrets, not by this manifest set.

## Apply

```bash
kubectl apply -f platform.yaml
kubectl wait --for=condition=Ready platform/support-desk -n eks-agent-platform --timeout=5m
kubectl get -n tenants-support-desk agentfleet,scaledobject,pods
```

Before applying against a real cluster, replace the placeholder SQS queue in `spec.scaling.queueUrl` (`.../111111111111/support-desk-work`) with your work queue — the tenant IAM role is granted `sqs:GetQueueAttributes` on whatever queue you name.

## Validate without a cluster

```bash
# Renders + schema/CEL-validates against the installed CRDs without persisting.
kubectl apply --dry-run=server -f platform.yaml
```

## Teardown

```bash
kubectl delete -f platform.yaml
```
