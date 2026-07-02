import { describe, expect, it } from 'vitest';

import { EksAgentClient, type CustomObjectsClient, type Platform } from './index.js';

function fakeApi(overrides: Partial<CustomObjectsClient> = {}): {
  api: CustomObjectsClient;
  calls: Record<string, unknown[]>;
} {
  const calls: Record<string, unknown[]> = {};
  const record =
    (name: string, result: unknown) =>
    (...args: unknown[]) => {
      calls[name] = args;
      return Promise.resolve(result);
    };
  const api = {
    listClusterCustomObject: record('listClusterCustomObject', { items: [] }),
    getClusterCustomObject: record('getClusterCustomObject', {}),
    createClusterCustomObject: record('createClusterCustomObject', {}),
    deleteClusterCustomObject: record('deleteClusterCustomObject', {}),
    listNamespacedCustomObject: record('listNamespacedCustomObject', { items: [] }),
    ...overrides,
  };
  return { api, calls };
}

const platform = (name: string): Platform => ({
  apiVersion: 'platform.nanohype.dev/v1alpha1',
  kind: 'Platform',
  metadata: { name },
  spec: {} as Platform['spec'],
});

describe('EksAgentClient', () => {
  it('uses the injected api without touching kubeconfig resolution', () => {
    const { api } = fakeApi();
    const client = new EksAgentClient({ api });
    expect(client.api).toBe(api);
  });

  it('lists platforms from the platform group and unwraps items', async () => {
    const { api, calls } = fakeApi({
      listClusterCustomObject: (...args: unknown[]) => {
        calls.listClusterCustomObject = args;
        return Promise.resolve({ items: [platform('a'), platform('b')] });
      },
    });
    const client = new EksAgentClient({ api });
    const platforms = await client.listPlatforms();
    expect(platforms.map((p) => p.metadata.name)).toEqual(['a', 'b']);
    expect(calls.listClusterCustomObject?.[0]).toMatchObject({
      group: 'platform.nanohype.dev',
      version: 'v1alpha1',
      plural: 'platforms',
    });
  });

  it('returns an empty list when the API response carries no items', async () => {
    const { api } = fakeApi({
      listClusterCustomObject: () => Promise.resolve({}),
    });
    const client = new EksAgentClient({ api });
    await expect(client.listPlatforms()).resolves.toEqual([]);
  });

  it('routes model gateways through the agents group in the given namespace', async () => {
    const { api, calls } = fakeApi();
    const client = new EksAgentClient({ api });
    await client.listModelGateways('tenants-acme');
    expect(calls.listNamespacedCustomObject?.[0]).toMatchObject({
      group: 'agents.nanohype.dev',
      namespace: 'tenants-acme',
      plural: 'modelgateways',
    });
  });

  it('applies and deletes platforms by name against the cluster scope', async () => {
    const { api, calls } = fakeApi();
    const client = new EksAgentClient({ api });
    await client.applyPlatform(platform('acme'));
    await client.deletePlatform('acme');
    expect(calls.createClusterCustomObject?.[0]).toMatchObject({
      body: { metadata: { name: 'acme' } },
    });
    expect(calls.deleteClusterCustomObject?.[0]).toMatchObject({ name: 'acme' });
  });
});
