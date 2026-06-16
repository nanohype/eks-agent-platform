# Runbook — A tenant says their agents stopped working

**Trigger**: a tenant pings you in their persona channel saying "our agent isn't responding". No automated page (the [PlatformSuspended](./platform-suspended.md) alert already fired but maybe the persona owner is not on-call and didn't see it).

## First check (30 seconds)

```bash
# Map tenant name → platform → phase
kubectl get tenant <tenant-name> -o wide
kubectl get platforms -l agents.nanohype.dev/tenant=<tenant-name>
```

If any Platform shows `Suspended`, follow [platform-suspended.md](./platform-suspended.md). Skip the rest of this runbook.

## If platform is Ready but agents not responding

Three independent failure modes — diagnose in parallel:

### Mode A: agentgateway can't reach Bedrock

```bash
# Check agentgateway pod logs for InvokeModel errors
kubectl -n agentgateway logs -l app.kubernetes.io/name=agentgateway --tail=100 | grep -i "bedrock\|invoke"

# Test the route directly from inside the tenant ns
kubectl -n tenants-<platform> run curl --rm -it --image=curlimages/curl --restart=Never -- \
  curl -sX POST http://agentgateway.agentgateway.svc.cluster.local:8080/v1/messages \
       -H 'content-type: application/json' \
       -d '{"route":"<route-name>","messages":[{"role":"user","content":"ping"}]}'
```

Cause: Bedrock model quota hit, cross-region inference profile mis-configured, agentgateway pod OOM.

### Mode B: fleet agents not pulling work

```bash
# Are the agent pods alive and reading from the queue (if SQS-backed)?
kubectl -n tenants-<platform> get pods -l agents.nanohype.dev/fleet=<fleet-name>

# KEDA-scaled fleet: is the ScaledObject reporting healthy?
kubectl -n tenants-<platform> get scaledobject -l agents.nanohype.dev/fleet=<fleet-name> -o yaml | grep -A 5 "status:"

# Inflight queue depth
aws sqs get-queue-attributes --queue-url <queue-url> --attribute-names ApproximateNumberOfMessages
```

Cause: KEDA TriggerAuthentication broken (IRSA), SQS queue policy missing tenant role, KEDA scaling cooldown holding pods at 0.

### Mode C: tenant pod itself crashed

```bash
kubectl -n tenants-<platform> describe pod <pod-name>
kubectl -n tenants-<platform> logs <pod-name> --previous
```

Cause: tenant code bug (out of scope — hand back to tenant). Most useful here: confirm IRSA is wired (`kubectl get sa tenant-runtime -o yaml | grep role-arn`) and the role has the baseline policy attached (`aws iam list-attached-role-policies --role-name <env>-<platform>-tenant`).

## Postmortem

Required if MTTR > 15 minutes. The root cause analysis usually points back at a regression in one of: agentgateway chart values, agent-iam baseline policy, KEDA pod-identity wiring. Add a dashboard check that would have caught it pre-customer-impact.
