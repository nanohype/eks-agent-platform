import { afterEach, describe, expect, it, vi } from 'vitest';

import {
  type CustomObjectsClient,
  EksAgentClient,
  type KubeConfigLoader,
  type Platform,
  resolveApi,
} from './index.js';

function fakeApi(overrides: Partial<CustomObjectsClient> = {}): {
  api: CustomObjectsClient;
  calls: Record<string, unknown[]>;
} {
  const calls: Record<string, unknown[]> = {};
  const record =
    (name: string, result: unknown) =>
    (...args: unknown[]) => {
      // eslint-disable-next-line security/detect-object-injection -- test-local record keyed by a literal method name
      calls[name] = args;
      return Promise.resolve(result);
    };
  const api = {
    listClusterCustomObject: record('listClusterCustomObject', { items: [] }),
    getClusterCustomObject: record('getClusterCustomObject', {}),
    // The API server echoes the persisted object back on create; mirror that so
    // the client's read-boundary parse has a real resource to validate.
    createClusterCustomObject: (...args: unknown[]) => {
      calls.createClusterCustomObject = args;
      return Promise.resolve((args[0] as { body?: unknown }).body ?? {});
    },
    deleteClusterCustomObject: record('deleteClusterCustomObject', {}),
    listNamespacedCustomObject: record('listNamespacedCustomObject', { items: [] }),
    getNamespacedCustomObject: record('getNamespacedCustomObject', {}),
    createNamespacedCustomObject: (...args: unknown[]) => {
      calls.createNamespacedCustomObject = args;
      return Promise.resolve((args[0] as { body?: unknown }).body ?? {});
    },
    deleteNamespacedCustomObject: record('deleteNamespacedCustomObject', {}),
    ...overrides,
  };
  return { api, calls };
}

const platform = (name: string, namespace?: string): Platform => ({
  apiVersion: 'platform.nanohype.dev/v1alpha1',
  kind: 'Platform',
  metadata: namespace ? { name, namespace } : { name },
  spec: {
    persona: 'ops',
    tenant: 'acme',
    budget: { name: `${name}-budget` },
    identity: {
      allowedModels: [],
      allowedModelFamilies: ['anthropic'],
      extraPolicyArns: [],
      capabilities: [],
      directSecretReads: [],
    },
    isolation: 'namespace',
  },
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
      limit: 100,
    });
  });

  it('follows the continue token across pages', async () => {
    let call = 0;
    const seen: (string | undefined)[] = [];
    const { api } = fakeApi({
      listClusterCustomObject: (...args: unknown[]) => {
        seen.push((args[0] as { _continue?: string })._continue);
        call += 1;
        return call === 1
          ? Promise.resolve({ items: [platform('a')], metadata: { continue: 'tok' } })
          : Promise.resolve({ items: [platform('b')] });
      },
    });
    const client = new EksAgentClient({ api });
    const platforms = await client.listPlatforms();
    expect(platforms.map((p) => p.metadata.name)).toEqual(['a', 'b']);
    // First page has no token, the second is fetched with the returned token.
    expect(seen).toEqual([undefined, 'tok']);
  });

  it('parses read responses through the schema, rejecting a malformed CR', async () => {
    const { api } = fakeApi({
      getNamespacedCustomObject: () => Promise.resolve({ apiVersion: 'wrong', kind: 'Platform' }),
    });
    const client = new EksAgentClient({ api });
    await expect(client.getPlatform('tenants-acme', 'acme')).rejects.toThrow();
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

  // Platform is `scope: Namespaced`. Addressing a single object through the
  // cluster-scoped path builds a URL with no namespace segment, which the API
  // server answers 404 for however real the object is — so these assert the
  // METHOD as much as the arguments.
  it('addresses a single platform through the namespaced scope', async () => {
    const calls: Record<string, unknown[]> = {};
    const { api } = fakeApi({
      // The read boundary parses, so the fake has to answer with a real
      // resource rather than an empty object.
      getNamespacedCustomObject: (...args: unknown[]) => {
        calls.getNamespacedCustomObject = args;
        return Promise.resolve(platform('acme', 'tenants-acme'));
      },
      createNamespacedCustomObject: (...args: unknown[]) => {
        calls.createNamespacedCustomObject = args;
        return Promise.resolve((args[0] as { body?: unknown }).body ?? {});
      },
      deleteNamespacedCustomObject: (...args: unknown[]) => {
        calls.deleteNamespacedCustomObject = args;
        return Promise.resolve({});
      },
      getClusterCustomObject: (...args: unknown[]) => {
        calls.getClusterCustomObject = args;
        return Promise.resolve({});
      },
      createClusterCustomObject: (...args: unknown[]) => {
        calls.createClusterCustomObject = args;
        return Promise.resolve({});
      },
      deleteClusterCustomObject: (...args: unknown[]) => {
        calls.deleteClusterCustomObject = args;
        return Promise.resolve({});
      },
    });
    const client = new EksAgentClient({ api });
    await client.getPlatform('tenants-acme', 'acme');
    await client.applyPlatform(platform('acme', 'tenants-acme'));
    await client.deletePlatform('tenants-acme', 'acme');

    expect(calls.getNamespacedCustomObject?.[0]).toMatchObject({
      namespace: 'tenants-acme',
      plural: 'platforms',
      name: 'acme',
    });
    expect(calls.createNamespacedCustomObject?.[0]).toMatchObject({
      namespace: 'tenants-acme',
      body: { metadata: { name: 'acme' } },
    });
    expect(calls.deleteNamespacedCustomObject?.[0]).toMatchObject({
      namespace: 'tenants-acme',
      name: 'acme',
    });

    // The cluster-scoped single-object methods must go unused for this kind.
    expect(calls.getClusterCustomObject).toBeUndefined();
    expect(calls.createClusterCustomObject).toBeUndefined();
    expect(calls.deleteClusterCustomObject).toBeUndefined();
  });

  it('refuses to apply a platform that names no namespace', async () => {
    // The namespace comes from the object, so an object without one has nowhere
    // to go and the API server has no default to fall back on. Failing here
    // names the missing field; the alternative is a 404 that reads as a broken
    // cluster.
    const { api, calls } = fakeApi();
    const client = new EksAgentClient({ api });
    await expect(client.applyPlatform(platform('acme'))).rejects.toThrow(/namespace is required/);
    expect(calls.createNamespacedCustomObject).toBeUndefined();
  });

  it('rejects when the caller-supplied signal is already aborted', async () => {
    const { api } = fakeApi();
    const client = new EksAgentClient({ api });
    await expect(
      client.listPlatforms({ signal: AbortSignal.abort(new Error('caller cancelled')) }),
    ).rejects.toThrow(/caller cancelled/);
  });

  it('enforces the default deadline on a hung API call', async () => {
    const { api } = fakeApi({
      // A call that never settles — only the deadline can end it.
      listClusterCustomObject: () => new Promise<never>(() => undefined),
    });
    const client = new EksAgentClient({ api, timeoutMs: 5 });
    await expect(client.listPlatforms()).rejects.toThrow();
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
