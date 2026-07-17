# bedrock-rag

A retrieval-augmented tenant. The manifests declare a Platform with a **primary + fallback** gateway; [`src/rag-client.ts`](./src/rag-client.ts) is the SDK side a RAG worker pod would run.

What the client shows, all real `@eks-agent/sdk` surface:

- **Cached corpus prefix** — the system instruction + retrieved context sit at the front with `cache: true` (a Bedrock `cachePoint`); the per-request question is the uncached tail, so repeat questions over the same corpus read the prefix at the cache-read price.
- **Fallback router** — `createModelRouter([Sonnet 4.6, Haiku 4.5])` answers on the primary and walks to the fallback on a retryable error, throwing a typed `ChainExhaustedError` only when the whole chain fails.
- **Streaming** — `messagesStream` surfaces token deltas via an `onText` handler and resolves with the full response.

## Typecheck the client

```bash
pnpm --filter @eks-agent-example/bedrock-rag typecheck
```

## Apply the tenant

```bash
kubectl apply -f platform.yaml
kubectl wait --for=condition=Ready platform/docs-rag -n eks-agent-platform --timeout=5m
```

Validate against the installed CRDs without a cluster write:

```bash
kubectl apply --dry-run=server -f platform.yaml
```

## Use the client

```ts
import { buildRouter, answerFromContext } from './src/rag-client.js';

const router = buildRouter({ region: 'us-west-2', platform: 'docs-rag', tenant: 'ragco' });
const res = await answerFromContext(
  router,
  retrievedCorpus,
  'How do I rotate a tenant key?',
  crypto.randomUUID(),
);
console.log(res.text, res.costUsd);
```

The `doc-retriever` `ToolServer` the fleet references is delivered separately (`addons-ai-platform` + External Secrets); the client stands in for the retrieval step so the example runs without a Knowledge Base.

## Teardown

```bash
kubectl delete -f platform.yaml
```
