import { describe, expect, it } from 'vitest';

import { ModelGatewayResource, PlatformResource } from './schemas.js';

describe('PlatformResource', () => {
  it('surfaces spec.attribution', () => {
    const p = PlatformResource.parse({
      apiVersion: 'platform.nanohype.dev/v1alpha1',
      kind: 'Platform',
      metadata: { name: 'acme', namespace: 'tenants-acme' },
      spec: {
        persona: 'ops',
        tenant: 'acme',
        budget: { name: 'acme' },
        identity: { allowedModelFamilies: ['anthropic'] },
        attribution: { operators: ['dev@acme.example'] },
      },
    });
    expect(p.spec.attribution?.operators).toEqual(['dev@acme.example']);
    // server-side default is mirrored so a read exposes it
    expect(p.spec.attribution?.sessionRoleMaxDurationSeconds).toBe(3600);
  });

  it('surfaces the suspension-state status block the kill-switch writes', () => {
    const p = PlatformResource.parse({
      apiVersion: 'platform.nanohype.dev/v1alpha1',
      kind: 'Platform',
      metadata: { name: 'acme' },
      spec: {
        persona: 'ops',
        tenant: 'acme',
        budget: { name: 'acme' },
        identity: {},
      },
      status: {
        phase: 'Suspended',
        iamRoleArn: 'arn:aws:iam::000000000000:role/cluster-acme-tenant',
        sessionRoleArn: 'arn:aws:iam::000000000000:role/cluster-acme-session',
        observedGeneration: 4,
        suspendedAt: '2026-07-18T00:00:00.000Z',
        suspendedReason: 'budget-exceeded',
        conditions: [
          {
            type: 'Suspended',
            status: 'True',
            observedGeneration: 4,
            lastTransitionTime: '2026-07-18T00:00:00.000Z',
            reason: 'BudgetExceeded',
            message: 'kill-switch fired at 120% of budget',
          },
        ],
      },
    });
    expect(p.status?.suspendedReason).toBe('budget-exceeded');
    expect(p.status?.sessionRoleArn).toContain('session');
    expect(p.status?.conditions?.[0]?.type).toBe('Suspended');
  });

  it('parses a status with none of the optional fields set', () => {
    const p = PlatformResource.parse({
      apiVersion: 'platform.nanohype.dev/v1alpha1',
      kind: 'Platform',
      metadata: { name: 'acme' },
      spec: { persona: 'ops', tenant: 'acme', budget: { name: 'acme' }, identity: {} },
      status: { phase: 'Ready' },
    });
    expect(p.status?.phase).toBe('Ready');
    expect(p.status?.suspendedAt).toBeUndefined();
  });
});

describe('ModelGatewayResource', () => {
  it('surfaces status.conditions and observedGeneration', () => {
    const mg = ModelGatewayResource.parse({
      apiVersion: 'agents.nanohype.dev/v1alpha1',
      kind: 'ModelGateway',
      metadata: { name: 'acme-gw' },
      spec: {
        platformRef: { name: 'acme' },
        routes: [
          { name: 'primary', modelFamily: 'anthropic', modelId: 'anthropic.claude-sonnet-4-6' },
        ],
      },
      status: {
        phase: 'Ready',
        endpoint: 'agentgateway.acme.svc',
        observedGeneration: 2,
        conditions: [
          {
            type: 'Ready',
            status: 'True',
            lastTransitionTime: '2026-07-18T00:00:00.000Z',
            reason: 'RoutesReconciled',
            message: 'all routes healthy',
          },
        ],
      },
    });
    expect(mg.status?.observedGeneration).toBe(2);
    expect(mg.status?.conditions?.[0]?.status).toBe('True');
  });
});
