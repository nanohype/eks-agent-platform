import { describe, expect, it } from 'vitest';

import { bareModel, estimateCost, PRICES, priceModel } from './index.js';

describe('estimateCost', () => {
  it('prices a Claude Sonnet 4.6 call (US inference profile)', () => {
    const usd = estimateCost({
      modelId: 'us.anthropic.claude-sonnet-4-6',
      tokens: {
        inputTokens: 1_000_000,
        outputTokens: 1_000_000,
        cacheReadTokens: 0,
        cacheWriteTokens: 0,
      },
    });
    // 3.0 + 15.0 = 18.0 per million in+out
    expect(usd).toBeCloseTo(18.0, 4);
  });

  it('prices a Claude Opus 4.8 call', () => {
    const usd = estimateCost({
      modelId: 'anthropic.claude-opus-4-8',
      tokens: {
        inputTokens: 1_000_000,
        outputTokens: 1_000_000,
        cacheReadTokens: 0,
        cacheWriteTokens: 0,
      },
    });
    // 5.0 + 25.0 = 30.0 per million in+out
    expect(usd).toBeCloseTo(30.0, 4);
  });

  it('prices a Claude Haiku 4.5 call (US inference profile)', () => {
    const usd = estimateCost({
      modelId: 'us.anthropic.claude-haiku-4-5-20251001-v1:0',
      tokens: {
        inputTokens: 1_000_000,
        outputTokens: 1_000_000,
        cacheReadTokens: 0,
        cacheWriteTokens: 0,
      },
    });
    // 1.0 + 5.0 = 6.0 per million in+out
    expect(usd).toBeCloseTo(6.0, 4);
  });

  it('handles cache read pricing for Anthropic', () => {
    const usd = estimateCost({
      modelId: 'anthropic.claude-sonnet-4-6',
      tokens: { inputTokens: 0, outputTokens: 0, cacheReadTokens: 1_000_000, cacheWriteTokens: 0 },
    });
    expect(usd).toBeCloseTo(0.3, 4);
  });

  it.each([
    ['us', 'us.anthropic.claude-sonnet-4-6'],
    ['eu', 'eu.anthropic.claude-sonnet-4-6'],
    ['jp', 'jp.anthropic.claude-sonnet-4-6'],
    ['apac', 'apac.anthropic.claude-sonnet-4-6'],
    ['ap', 'ap.anthropic.claude-sonnet-4-6'],
    ['global', 'global.anthropic.claude-sonnet-4-6'],
  ])('strips %s. cross-region prefix and resolves to the base model', (_, modelId) => {
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

  it('ships the current-generation claude-4 family', () => {
    for (const id of [
      'anthropic.claude-sonnet-4-6',
      'anthropic.claude-opus-4-8',
      'anthropic.claude-opus-4-6-v1',
      'anthropic.claude-haiku-4-5-20251001-v1:0',
    ]) {
      // eslint-disable-next-line security/detect-object-injection
      expect(PRICES[id]).toBeDefined();
    }
  });
});

describe('bareModel', () => {
  it('strips a single cross-region prefix', () => {
    expect(bareModel('us.anthropic.claude-sonnet-4-6')).toBe('anthropic.claude-sonnet-4-6');
    expect(bareModel('global.anthropic.claude-opus-4-6-v1')).toBe('anthropic.claude-opus-4-6-v1');
  });

  it('leaves a bare provider id untouched', () => {
    expect(bareModel('anthropic.claude-3-opus-20240229-v1:0')).toBe(
      'anthropic.claude-3-opus-20240229-v1:0',
    );
    expect(bareModel('anthropic.claude-sonnet-4-6')).toBe('anthropic.claude-sonnet-4-6');
  });
});

describe('priceModel', () => {
  it('returns priced:true with the cost for a known model', () => {
    const r = priceModel({
      modelId: 'anthropic.claude-sonnet-4-6',
      tokens: {
        inputTokens: 1_000_000,
        outputTokens: 1_000_000,
        cacheReadTokens: 0,
        cacheWriteTokens: 0,
      },
    });
    expect(r.priced).toBe(true);
    expect(r.costUsd).toBeCloseTo(18.0, 4);
  });

  it('flags an unknown model id as priced:false with an unmetered 0', () => {
    const r = priceModel({
      modelId: 'unknown.model.id',
      tokens: {
        inputTokens: 5_000_000,
        outputTokens: 5_000_000,
        cacheReadTokens: 0,
        cacheWriteTokens: 0,
      },
    });
    expect(r.priced).toBe(false);
    expect(r.costUsd).toBe(0);
  });

  it('keeps estimateCost in sync with priceModel.costUsd', () => {
    const args = {
      modelId: 'amazon.nova-pro-v1:0',
      tokens: {
        inputTokens: 2_000_000,
        outputTokens: 1_000_000,
        cacheReadTokens: 0,
        cacheWriteTokens: 0,
      },
    };
    expect(estimateCost(args)).toBe(priceModel(args).costUsd);
  });
});
