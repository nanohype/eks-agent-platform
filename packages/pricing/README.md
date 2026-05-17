# @eks-agent/pricing

Bedrock per-million-token price tables. Bedrock pricing changes regularly; this package is **Renovate-managed** and refreshed weekly via `scripts/refresh-pricing.mjs`. Never hand-edit values — outdated numbers silently undercount spend.

```ts
import { estimateCost } from '@eks-agent/pricing';

const usd = estimateCost({
  modelFamily: 'anthropic',
  modelId: 'us.anthropic.claude-3-5-sonnet-20241022-v2:0',
  tokens: { inputTokens: 12500, outputTokens: 800, cacheReadTokens: 0, cacheWriteTokens: 0 },
});
// ≈ 0.0495
```

Cross-region inference profiles (`us.*`, `eu.*`, `apac.*`) fall back to the base model price.
