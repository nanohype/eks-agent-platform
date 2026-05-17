# blank-tenant

The smallest viable Platform: one agent, one route, one budget, one daily smoke-test eval.

```bash
kubectl apply -f platform.yaml
kubectl wait --for=condition=Ready platform/blank --timeout=5m
kubectl get -n tenants-blank pods
```

Smoke-test the gateway from inside the cluster:

```bash
kubectl run -n tenants-blank curl --rm -it --image=curlimages/curl --restart=Never -- \
  curl -sX POST http://agentgateway.agentgateway.svc.cluster.local:8080/v1/messages \
  -H 'content-type: application/json' \
  -d '{"route":"primary","messages":[{"role":"user","content":"ping"}],"max_tokens":16}'
```

Teardown:

```bash
kubectl delete -f platform.yaml
```
