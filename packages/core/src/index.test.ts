import { describe, expect, it } from 'vitest';

import {
  AgentError,
  AgentId,
  CorrelationId,
  isRetryable,
  ModelRouteName,
  PlatformId,
  TenantId,
  WorkspaceId,
} from './index.js';

describe('branded IDs', () => {
  it('accepts valid slugs', () => {
    expect(PlatformId('marketing-team')).toBe('marketing-team');
    expect(TenantId('acme-corp')).toBe('acme-corp');
    expect(WorkspaceId('q1-launch')).toBe('q1-launch');
    expect(AgentId('campaign-writer')).toBe('campaign-writer');
    expect(ModelRouteName('primary')).toBe('primary');
  });

  it('rejects invalid slugs', () => {
    expect(() => PlatformId('Marketing-Team')).toThrow(/invalid PlatformId/);
    expect(() => PlatformId('mt')).toThrow(); // too short
    expect(() => PlatformId('-leading-hyphen')).toThrow();
    expect(() => PlatformId('trailing-hyphen-')).toThrow();
  });

  it('enforces the Kubernetes 63-char ceiling', () => {
    const sixtyThree = 'a' + 'b'.repeat(62); // 1 + 62 = 63 chars total
    expect(() => PlatformId(sixtyThree)).not.toThrow();
    const sixtyFour = 'a' + 'b'.repeat(63); // 64 chars — over the limit
    expect(() => PlatformId(sixtyFour)).toThrow(/3-63/);
  });

  it("each constructor's error message names the right brand", () => {
    expect(() => WorkspaceId('Bad')).toThrow(/invalid WorkspaceId/);
    expect(() => AgentId('Bad')).toThrow(/invalid AgentId/);
    expect(() => ModelRouteName('Bad')).toThrow(/invalid ModelRouteName/);
  });
});

describe('CorrelationId', () => {
  it('produces a valid UUID v4', () => {
    const id = CorrelationId();
    // UUIDv4 pattern: 8-4-4-4-12 hex, version nibble = 4, variant 8/9/a/b
    expect(id).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i);
  });

  it('produces distinct values on each call', () => {
    const set = new Set([CorrelationId(), CorrelationId(), CorrelationId(), CorrelationId(), CorrelationId()]);
    expect(set.size).toBe(5);
  });
});

describe('AgentError', () => {
  it('marks RateLimit and Overloaded as retryable', () => {
    const e = new AgentError({ class: 'RateLimit', message: 'throttled' });
    expect(e.retryable).toBe(true);
    expect(isRetryable(e)).toBe(true);
  });

  it('marks BadRequest as non-retryable', () => {
    const e = new AgentError({ class: 'BadRequest', message: 'malformed' });
    expect(e.retryable).toBe(false);
    expect(isRetryable(e)).toBe(false);
  });

  it.each([
    ['RateLimit', true],
    ['Overloaded', true],
    ['Server', true],
    ['Network', true],
    ['BadRequest', false],
    ['AuthFailure', false],
    ['GuardrailBlock', false],
    ['BudgetExceeded', false],
    ['ContextLengthExceeded', false],
    ['Cancelled', false],
  ] as const)("'%s' retry classification", (cls, expected) => {
    const e = new AgentError({ class: cls, message: 'x' });
    expect(e.retryable).toBe(expected);
  });

  it('preserves correlationId when provided', () => {
    const e = new AgentError({ class: 'Server', message: 'boom', correlationId: 'abc' });
    expect(e.correlationId).toBe('abc');
  });

  it('omits correlationId when not provided', () => {
    const e = new AgentError({ class: 'Server', message: 'boom' });
    expect(e.correlationId).toBeUndefined();
  });

  it('preserves the underlying cause for inspection', () => {
    const root = new Error('root');
    const e = new AgentError({ class: 'Server', message: 'wrapped', cause: root });
    expect(e.cause).toBe(root);
  });
});

describe('isRetryable', () => {
  it('returns false for non-AgentError values', () => {
    expect(isRetryable(new Error('plain'))).toBe(false);
    expect(isRetryable('throttled')).toBe(false);
    expect(isRetryable(null)).toBe(false);
    expect(isRetryable(undefined)).toBe(false);
  });
});
