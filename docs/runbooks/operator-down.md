# Runbook — OperatorLeaderMissing

**Severity**: critical. **Pages**: PagerDuty + Slack #incidents.

## Symptom

`absent(workqueue_depth{namespace="eks-agent-platform"})` for 5 minutes — no replica is reporting metrics. Either the deployment is down or every replica's leader-election lease is stale.

## Diagnose

```bash
# Are pods running?
kubectl -n eks-agent-platform get pods -l app.kubernetes.io/name=operator -o wide

# Who is the current leader?
kubectl -n eks-agent-platform get lease eks-agent-platform.nanohype.dev -o yaml

# Recent events
kubectl -n eks-agent-platform get events --sort-by=.lastTimestamp | tail -20

# Pod logs (last 100 lines per pod)
for p in $(kubectl -n eks-agent-platform get pods -l app.kubernetes.io/name=operator -o name); do
  echo "=== $p ==="
  kubectl -n eks-agent-platform logs "$p" --tail=100
done
```

## Likely causes

1. **OOMKilled** — the operator hit its memory limit. Check `kubectl get pods` for `OOMKilled` state. Bump `resources.limits.memory` in the operator chart values.
2. **CrashLoopBackOff** — startup failure. Read `kubectl logs <pod>` for the panic message. Most likely a malformed SSM config or missing IRSA wiring.
3. **Leader election lease wedged** — rare; manually delete the lease to force re-election: `kubectl -n eks-agent-platform delete lease eks-agent-platform.nanohype.dev`.

## Mitigate

1. If OOMKilled, bump limit and `kubectl rollout restart deploy/operator -n eks-agent-platform`.
2. If CrashLoop, fix the config / wiring and let the deployment converge.
3. If lease wedged, delete it as above. Replicas re-elect within 15s.

## Recover

Workqueue metrics resume within 30s of a healthy replica taking leadership. Alert resolves at 5m of present metrics.

## Postmortem

Required for any outage > 15 minutes. Capture:

- root cause (OOM / cert / wedged lease / other),
- whether the staging deployment showed the same symptom (and why CI didn't catch it if not),
- what monitoring would have given a 30-minute heads-up — file as a new dashboard item.
