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
    'throws with a Phase-2 hint for unshipped family %s',
    (family) => {
      expect(() => createBedrockAdapter(family, opts)).toThrow(/Phase 2/);
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
