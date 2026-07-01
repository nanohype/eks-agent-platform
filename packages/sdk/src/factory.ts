import type { ModelFamily } from '@eks-agent/core';

import { AnthropicBedrockAdapter } from './adapters/anthropic.js';
import { type BedrockAdapter, type BedrockAdapterOptions } from './adapters/bedrock-base.js';
import { NovaBedrockAdapter } from './adapters/nova.js';

type AdapterCtor = new (opts: BedrockAdapterOptions) => BedrockAdapter;

const REGISTRY: Partial<Record<ModelFamily, AdapterCtor>> = {
  anthropic: AnthropicBedrockAdapter,
  'amazon-nova': NovaBedrockAdapter,
};

/**
 * Construct the BedrockAdapter for a given model family. Throws if the
 * family has no shipped adapter — the alternative (silent fallback to a
 * default) would silently mis-route invocations.
 *
 * Adding a new family is a registry insert here plus a new subclass; ADR
 * 0003 names this contract explicitly.
 */
export function createBedrockAdapter(family: ModelFamily, opts: BedrockAdapterOptions): BedrockAdapter {
  // eslint-disable-next-line security/detect-object-injection
  const ctor = REGISTRY[family];
  if (!ctor) {
    throw new Error(
      `no BedrockAdapter registered for model family '${family}'. Shipped: ${Object.keys(REGISTRY).join(', ')}. To support another family, subclass BedrockAdapter and register the constructor in REGISTRY (packages/sdk/src/factory.ts).`,
    );
  }
  return new ctor(opts);
}

/** Families with a shipped BedrockAdapter implementation. */
export function shippedFamilies(): ModelFamily[] {
  return Object.keys(REGISTRY) as ModelFamily[];
}
