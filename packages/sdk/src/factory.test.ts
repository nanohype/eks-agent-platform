import { describe, expect, it } from 'vitest';

import { AnthropicBedrockAdapter } from './adapters/anthropic.js';
import { NovaBedrockAdapter } from './adapters/nova.js';
import { createBedrockAdapter, shippedFamilies } from './factory.js';

const opts = { region: 'us-west-2' };

describe('createBedrockAdapter', () => {
  it('constructs an AnthropicBedrockAdapter for family=anthropic', () => {
    const a = createBedrockAdapter('anthropic', opts);
    expect(a).toBeInstanceOf(AnthropicBedrockAdapter);
    expect(a.modelFamily).toBe('anthropic');
  });

  it('constructs a NovaBedrockAdapter for family=amazon-nova', () => {
    const a = createBedrockAdapter('amazon-nova', opts);
    expect(a).toBeInstanceOf(NovaBedrockAdapter);
    expect(a.modelFamily).toBe('amazon-nova');
  });

  it.each(['meta', 'mistral', 'cohere', 'amazon-titan', 'stability'] as const)(
    'throws naming the shipped families and the extension point for unshipped family %s',
    (family) => {
      expect(() => createBedrockAdapter(family, opts)).toThrow(/no BedrockAdapter registered/);
      expect(() => createBedrockAdapter(family, opts)).toThrow(/Shipped: anthropic, amazon-nova/);
      expect(() => createBedrockAdapter(family, opts)).toThrow(/register the constructor in REGISTRY/);
    },
  );
});

describe('shippedFamilies', () => {
  it('returns exactly the families with a registered constructor', () => {
    const families = shippedFamilies();
    expect(families).toContain('anthropic');
    expect(families).toContain('amazon-nova');
    expect(families).not.toContain('meta');
  });
});
