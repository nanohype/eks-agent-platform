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

## Failure mode: kill-switch fired but the platform never suspended

**Trigger**: the `KillSwitchUnrouted` alert (severity critical, persona finance). A budget breach published a kill-switch event, but the tenant is _still spending_ — the suspension never landed. This is the dangerous case: the spend ceiling is not being enforced.

The path is: budget reconciler `PutEvents` → EventBridge rule (`<cluster>-killswitch-breach`) → Step Functions (`<cluster>-killswitch`) detaches the tenant's Bedrock baseline policy and tags the IRSA role `platform.nanohype.dev/suspended=true` → the PlatformReconciler observes the tag and flips the Platform to `Suspended`. A break anywhere in that chain leaves `BudgetPolicy.status.killSwitchFiredAt` set while the Platform stays `Ready`. The reconciler detects this after a grace window (default 3 budget ticks), sets `KillSwitchUnrouted=True` on the BudgetPolicy, emits `agents_killswitch_unrouted_total`, and re-publishes the breach on a bounded exponential backoff — so a transient EventBridge/SFN blip self-heals, but a real misconfiguration pages.

Diagnose in order — each step narrows where the chain broke:

```bash
# 1. Confirm the reconciler's view.
kubectl get budgetpolicy <name> -n tenants-<platform> -o jsonpath='{.status.killSwitchFiredAt} {.status.killSwitchRefireCount}{"\n"}'
kubectl get budgetpolicy <name> -n tenants-<platform> -o jsonpath='{range .status.conditions[?(@.type=="KillSwitchUnrouted")]}{.reason}: {.message}{"\n"}{end}'

# 2. Did the event reach the bus? The rule counts matched invocations.
aws cloudwatch get-metric-statistics --namespace AWS/Events \
  --metric-name MatchedEvents --dimensions Name=RuleName,Value=<cluster>-killswitch-breach \
  --start-time "$(date -u -v-2H +%FT%TZ)" --end-time "$(date -u +%FT%TZ)" --period 3600 --statistics Sum

# 3. Did the state machine run, and how did it end?
aws stepfunctions list-executions --state-machine-arn "$(aws ssm get-parameter \
  --name /eks-agent-platform/<cluster>/kill-switch/state_machine_arn --query Parameter.Value --output text)" \
  --max-results 5
# For a failed / RecordFailure execution, read the history for the failing state.
aws stepfunctions get-execution-history --execution-arn <arn> --reverse-order --max-results 20
```

Most likely causes, by where the chain broke:

- **Event never matched (MatchedEvents = 0)**: the `source` / `detail-type` / `severity` on the published event no longer matches the rule's `event_pattern`. EventBridge matching is exact. The Go `budgetEventSource` constant and the terraform `event_pattern` are pinned together by a contract test, so this should only happen if the kill-switch component wasn't re-applied after an upgrade — run `tofu apply` on `terraform/components/kill-switch`.
- **State machine failed at `DetachBedrockPolicy` / `TagRoleSuspended`**: the `tenant_role_name_pattern` no longer matches the operator's minted role name, or the SFN role lost `iam:DetachRolePolicy` / `iam:TagRole` on the tenant IAM path. Check the failing state's cause in the execution history against `data.aws_ssm_parameter.tenant_iam_path`.
- **Tag set but Platform still Ready**: the PlatformReconciler isn't reconciling (operator down, or its 60s tag-poll requeue wedged). Check `kubectl get platform <name> -o yaml` for the `platform.nanohype.dev/suspended` tag vs `status.phase`, and the operator logs.

**Immediate mitigation while you diagnose** (the spend ceiling is not enforced — do not wait): detach the baseline policy by hand.

```bash
aws iam detach-role-policy --role-name <cluster>-<platform>-tenant \
  --policy-arn "$(aws ssm get-parameter --name /eks-agent-platform/<cluster>/agent-iam/tenant_baseline_policy_arn --query Parameter.Value --output text)"
aws iam tag-role --role-name <cluster>-<platform>-tenant \
  --tags Key=platform.nanohype.dev/suspended,Value=true Key=platform.nanohype.dev/suspended-reason,Value=manual-unrouted
```

The reconciler will observe the tag and settle the latch (`KillSwitchUnrouted` → `SuspensionObserved`) on its next tick.

## Postmortem

Required if MTTR > 15 minutes. The root cause analysis usually points back at a regression in one of: agentgateway chart values, agent-iam baseline policy, KEDA pod-identity wiring. Add a dashboard check that would have caught it pre-customer-impact.
