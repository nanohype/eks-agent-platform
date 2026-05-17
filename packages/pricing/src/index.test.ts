import { describe, expect, it } from 'vitest';

import { estimateCost, PRICES } from './index.js';

describe('estimateCost', () => {
  it('prices a Claude 3.5 Sonnet call (US inference profile)', () => {
    const usd = estimateCost({
      modelId: 'us.anthropic.claude-3-5-sonnet-20241022-v2:0',
      tokens: { inputTokens: 1_000_000, outputTokens: 1_000_000, cacheReadTokens: 0, cacheWriteTokens: 0 },
    });
    // 3.0 + 15.0 = 18.0 per million in+out
    expect(usd).toBeCloseTo(18.0, 4);
  });

  it('handles cache read pricing for Anthropic', () => {
    const usd = estimateCost({
      modelId: 'anthropic.claude-3-5-sonnet-20241022-v2:0',
      tokens: { inputTokens: 0, outputTokens: 0, cacheReadTokens: 1_000_000, cacheWriteTokens: 0 },
    });
    expect(usd).toBeCloseTo(0.3, 4);
  });

  it.each([
    ['us', 'us.anthropic.claude-3-5-sonnet-20241022-v2:0'],
    ['eu', 'eu.anthropic.claude-3-5-sonnet-20241022-v2:0'],
    ['apac', 'apac.anthropic.claude-3-5-sonnet-20241022-v2:0'],
    ['ap', 'ap.anthropic.claude-3-5-sonnet-20241022-v2:0'],
  ])('strips %s. cross-region prefix and falls back to base model', (_, modelId) => {
    const usd = estimateCost({
      modelId,
      tokens: { inputTokens: 1_000_000, outputTokens: 0, cacheReadTokens: 0, cacheWriteTokens: 0 },
    });
    expect(usd).toBeCloseTo(3.0, 4);
  });

  it('returns 0 for unknown model', () => {
    const usd = estimateCost({
      modelId: 'unknown.model.id',
      tokens: { inputTokens: 1_000_000, outputTokens: 0, cacheReadTokens: 0, cacheWriteTokens: 0 },
    });
    expect(usd).toBe(0);
  });

  it('ships all expected model families', () => {
    const ids = Object.keys(PRICES);
    expect(ids.some((id) => id.startsWith('anthropic.'))).toBe(true);
    expect(ids.some((id) => id.startsWith('amazon.nova-'))).toBe(true);
    expect(ids.some((id) => id.startsWith('meta.'))).toBe(true);
    expect(ids.some((id) => id.startsWith('mistral.'))).toBe(true);
    expect(ids.some((id) => id.startsWith('cohere.'))).toBe(true);
  });
});
