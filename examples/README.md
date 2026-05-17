# examples/

| Example                           | What it shows                                                                                               |
| --------------------------------- | ----------------------------------------------------------------------------------------------------------- |
| [`blank-tenant`](./blank-tenant/) | The minimal Platform — one agent, one route, daily smoke-test eval. The "did it work?" check after install. |
| [`agent-fleet`](./agent-fleet/)   | Snippet: multi-agent fleet with KEDA SQS scaling + ToolBindings.                                            |
| [`bedrock-rag`](./bedrock-rag/)   | Snippet: RAG via Bedrock Knowledge Base + retrieval tool through agentgateway.                              |

`blank-tenant` is the canonical "minimum viable tenant" — copy it, rename, edit the Persona / models / agent system-prompts to your use case. The other two are snippets covering specific subsystems (KEDA SQS scaling, Bedrock Knowledge Base RAG) that you'd fold into a full Platform CR set.

Each example is a workspace package; `pnpm install` at the repo root sets them up.
