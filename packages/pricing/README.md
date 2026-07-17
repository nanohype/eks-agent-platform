# @eks-agent/pricing

Bedrock per-million-token price tables. The single source of truth is
[`src/data/bedrock-pricing.json`](src/data/bedrock-pricing.json) — this package
imports it directly, and the Lambda cost publisher's Python table is generated
from the same file with a CI drift gate. Refresh it from the AWS Pricing API
with `scripts/refresh-pricing.mjs` (weekly cadence); never hand-edit prices from
memory — Bedrock pricing changes regularly and stale numbers silently
undercount spend.

```ts
import { estimateCost } from '@eks-agent/pricing';

const usd = estimateCost({
  modelId: 'us.anthropic.claude-sonnet-4-6',
  tokens: { inputTokens: 12500, outputTokens: 800, cacheReadTokens: 0, cacheWriteTokens: 0 },
});
// ≈ 0.0495
```

Cross-region inference profiles (`us.*`, `eu.*`, `jp.*`, `apac.*`, `global.*`)
strip to the base model price. Use `priceModel` when you need to distinguish a
real $0 from an unmetered miss (`priced: false`).
