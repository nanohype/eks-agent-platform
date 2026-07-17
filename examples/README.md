# examples/

| Example                           | What it shows                                                                                                         |
| --------------------------------- | --------------------------------------------------------------------------------------------------------------------- |
| [`blank-tenant`](./blank-tenant/) | The minimal Platform — one agent, one route, daily smoke-test eval. The "did it work?" check after install.           |
| [`agent-fleet`](./agent-fleet/)   | Multi-agent Platform: two model routes, two agents, KEDA SQS autoscaling, and a kagent ToolServer reference.          |
| [`bedrock-rag`](./bedrock-rag/)   | Retrieval-augmented tenant + a typechecked `@eks-agent/sdk` client: cached corpus prefix, fallback router, streaming. |

`blank-tenant` is the canonical "minimum viable tenant" — copy it, rename, edit the persona / models / agent system-prompts to your use case. `agent-fleet` and `bedrock-rag` are complete Platform CR sets that each layer on one subsystem (SQS-driven autoscaling with tools; SDK-side retrieval and prompt caching), so you can lift the piece you need into your own tenant.

Each example is a workspace package; `pnpm install` at the repo root sets them up. Every model id is an org default from `nanohype/standards/llm-policy.json`.

## Running them

```bash
kubectl apply -f <example>/platform.yaml                    # apply the CR set
kubectl apply --dry-run=server -f <example>/platform.yaml   # validate against the CRDs, no write
```

`bedrock-rag` also carries a real TypeScript client — typecheck it against the SDK:

```bash
pnpm --filter @eks-agent-example/bedrock-rag typecheck
```
