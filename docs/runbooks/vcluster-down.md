# Runbook — VClusterNotReady

**Severity**: warning. **Pages**: Slack #ops (not PagerDuty — host containment and running synced pods are intact while a vcluster is down).

## Symptom

`kube_customresource_condition{customresource_kind="Platform",condition_type="VClusterReady",condition_status="False"} == 1` for 15 minutes. A `Platform` with `spec.isolation: vcluster` has had its virtual cluster unavailable long enough that it is past normal bring-up.

`VClusterReady=False` is normal and transient during install (the vcluster chart deploys, the tenant ServiceAccount syncs to the host) and clears within a few minutes. A sustained 15m means the control plane is down or the tier is failing closed.

## What is and isn't affected

- **Unaffected**: host-side Platform provisioning (namespace, `ResourceQuota`, `LimitRange`, `NetworkPolicy`, IAM/KMS) is independent and keeps succeeding. Already-running synced pods on the host **keep running** — a control-plane blip does not evict the data plane.
- **Affected**: new workload reconciles _into_ the vcluster (AgentFleet kagent Agents, AgentSandbox pods, KEDA scalers) error and requeue with backoff until the vcluster recovers. The Pod Identity binding for the synced ServiceAccount is not (re)created until the SA is back.

## Diagnose

```bash
# Which Platform? Read the alert's $labels.name. Then find its tenant namespace:
kubectl get platform <name> -A -o jsonpath='{.status.namespace}{"\n"}'
NS=tenants-<name>

# 1. Check the VClusterReady condition reason — it names the failure mode.
kubectl get platform <name> -A -o jsonpath='{range .status.conditions[?(@.type=="VClusterReady")]}{.reason}: {.message}{"\n"}{end}'

# 2. Is the vcluster control-plane pod up?
kubectl -n "$NS" get statefulset,pod -l app=vcluster
kubectl -n "$NS" logs sts/vcluster --tail=100 -c syncer 2>/dev/null || \
  kubectl -n "$NS" logs -l app=vcluster --tail=100

# 3. Is the ArgoCD Application healthy/synced?
kubectl -n argocd get application <name>-vcluster -o jsonpath='{.status.sync.status} / {.status.health.status}{"\n"}'

# 4. Did the tenant SA sync to the host? (the Pod Identity target)
kubectl -n "$NS" get serviceaccount -l vcluster.loft.sh/managed-by=vcluster
```

## Likely causes

1. **`ArgoCDRequired`** (condition reason) — ArgoCD is not installed on this cluster. The vcluster tier fails closed by design; it cannot install the vcluster without ArgoCD. Either install ArgoCD (`eks-gitops` addon) or move the tenant back to `isolation: namespace` (re-create the Platform — the field is immutable).
2. **vcluster control-plane pod down** — OOMKilled or evicted. Check `kubectl -n "$NS" get pod` for the vcluster StatefulSet pod's state and the host `ResourceQuota` (`kubectl -n "$NS" describe resourcequota tenant-default`) — the tenant quota caps the vcluster's resource use, so a too-tight quota can starve it.
3. **ArgoCD Application OutOfSync/Degraded** — the vcluster chart failed to render or install (bad `vcluster.chart.version`, a values error, or an init-chart — kagent/KEDA — failing inside the vcluster). Read the Application's conditions and the vcluster syncer logs.
4. **Naming mismatch** (condition message names the discovered vs computed SA) — a vcluster upgrade changed its host-name algorithm. The operator refuses to bind Pod Identity to the wrong name. Pin `vcluster.chart.version` back to the known-good line and reconcile `pkg/util/translate.SafeConcatName` in `operators/internal/controller/vcluster_naming.go` against the new upstream before bumping.

## Mitigate

1. **ArgoCDRequired**: install ArgoCD, or (deliberate) re-create the Platform as `isolation: namespace`. There is no in-place tier flip — the field is immutable so a half-migrated Platform can't strand a vcluster.
2. **Control-plane OOM/evicted**: raise the tenant `ResourceQuota` if the vcluster is being starved, then `kubectl -n "$NS" rollout restart statefulset/vcluster`. Synced pods keep running through the restart.
3. **Application OutOfSync**: fix the chart values / version and let ArgoCD resync (`kubectl -n argocd annotate application <name>-vcluster argocd.argoproj.io/refresh=hard`).
4. **Naming mismatch**: roll `vcluster.chart.version` back to the pinned known-good version; do not bump vcluster majors without re-verifying the naming replica (there is a byte-identity unit test — `TestSafeConcatName_ByteIdenticalToUpstream`).

## Recover

Once the vcluster control plane is healthy and the tenant SA has re-synced to the host, the operator's next Platform reconcile flips `VClusterReady=True` and rebinds Pod Identity; queued workload reconciles drain on their backoff. The alert resolves after 15m of `VClusterReady=True`.

## Postmortem

Required if any tenant's fleet was unable to serve for > 30 minutes. Capture:

- root cause (ArgoCD absent / OOM / chart failure / naming mismatch / other),
- whether the host containment held (no synced pod escaped quota/PSS/netpol — it should have; if not, that's a separate, higher-severity finding),
- for a naming mismatch: the upstream vcluster change and whether the byte-identity test should have caught it pre-upgrade.
