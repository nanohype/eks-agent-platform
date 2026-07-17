import { afterEach, describe, expect, it, vi } from 'vitest';

import {
  EksAgentClient,
  resolveApi,
  type CustomObjectsClient,
  type KubeConfigLoader,
  type Platform,
} from './index.js';

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

// A fake KubeConfig that records which load path the resolver chose, so the
// precedence branches are covered without a real kubeconfig file or cluster env.
function fakeKubeConfig(): { kc: KubeConfigLoader; loads: string[]; context?: string } {
  const state: { kc: KubeConfigLoader; loads: string[]; context?: string } = {
    loads: [],
    kc: {} as KubeConfigLoader,
  };
  state.kc = {
    loadFromFile: (p: string) => state.loads.push(`file:${p}`),
    loadFromCluster: () => state.loads.push('cluster'),
    loadFromDefault: () => state.loads.push('default'),
    setCurrentContext: (c: string) => {
      state.context = c;
    },
    makeApiClient: () => ({}) as CustomObjectsClient,
  } as KubeConfigLoader;
  return state;
}

describe('resolveApi kubeconfig resolution', () => {
  const savedEnv = { ...process.env };
  afterEach(() => {
    process.env = { ...savedEnv };
    vi.restoreAllMocks();
  });

  it('prefers an explicit kubeconfigPath and applies a context override', () => {
    delete process.env.KUBECONFIG;
    delete process.env.KUBERNETES_SERVICE_HOST;
    const f = fakeKubeConfig();
    resolveApi({ kubeconfigPath: '/tmp/kc', context: 'staging' }, f.kc);
    expect(f.loads).toEqual(['file:/tmp/kc']);
    expect(f.context).toBe('staging');
  });

  it('falls back to the KUBECONFIG env path', () => {
    process.env.KUBECONFIG = '/env/kubeconfig';
    delete process.env.KUBERNETES_SERVICE_HOST;
    const f = fakeKubeConfig();
    resolveApi({}, f.kc);
    expect(f.loads).toEqual(['file:/env/kubeconfig']);
  });

  it('loads the in-cluster config when running inside a pod', () => {
    delete process.env.KUBECONFIG;
    process.env.KUBERNETES_SERVICE_HOST = '10.0.0.1';
    const f = fakeKubeConfig();
    resolveApi({}, f.kc);
    expect(f.loads).toEqual(['cluster']);
  });

  it('falls back to the default kubeconfig discovery', () => {
    delete process.env.KUBECONFIG;
    delete process.env.KUBERNETES_SERVICE_HOST;
    const f = fakeKubeConfig();
    resolveApi({}, f.kc);
    expect(f.loads).toEqual(['default']);
  });

  it('the constructor skips resolution entirely when an api is injected', () => {
    const f = fakeKubeConfig();
    const { api } = fakeApi();
    // Passing api short-circuits resolveApi; the fake loader stays untouched.
    const client = new EksAgentClient({ api });
    expect(client.api).toBe(api);
    expect(f.loads).toEqual([]);
  });
});
