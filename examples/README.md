# examples/

| Example                           | What it shows                                                                                              |
| --------------------------------- | ---------------------------------------------------------------------------------------------------------- |
| [`blank-tenant`](./blank-tenant/) | The minimal Platform — one agent, one route, daily smoke-test eval. The "did it work?" check after install. |
| [`agent-fleet`](./agent-fleet/)   | Multi-agent Platform: two model routes, two agents on their own images, KEDA SQS autoscaling.               |

`blank-tenant` is the canonical "minimum viable tenant" — copy it, rename, edit the persona / models / agent system-prompts to your use case. `agent-fleet` layers on one subsystem (SQS-driven autoscaling with tools), so you can lift the piece you need into your own tenant.

Every custom resource here is held to what the API server accepts.
`scripts/check-cr-admissibility.py` reads this directory against the CRDs the
operator chart ships, and `operators/test/admissibility` creates the same files
on a real API server, so a required property, a duplicate entry in a keyed list
or a misspelled key that would be silently dropped fails the build rather than
the copy someone made from it.

Both are CR sets, not applications, and that is the whole surface a tenant needs. An application reaches its models as ordinary HTTP against the route names its `ModelGateway` publishes, so there is no client library to import. Every model id is an org default from `nanohype/standards/llm-policy.json`.

## Running them

```bash
kubectl apply -f <example>/platform.yaml                    # apply the CR set
kubectl apply --dry-run=server -f <example>/platform.yaml   # validate against the CRDs, no write
```

Read the base URL and wire format back off the gateway rather than assuming them — each format is served under its own endpoint prefix, so the endpoint alone is not a usable base:

```bash
kubectl get modelgateway <name> -n eks-agent-platform -o jsonpath='{.status.routes}'
```
