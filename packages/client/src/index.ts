import type { PlatformSpec, ModelRouteSpec } from '@eks-agent/core';
import { KubeConfig, CustomObjectsApi } from '@kubernetes/client-node';

const GROUP = 'agents.stxkxs.io';
const VERSION = 'v1alpha1';

export interface ResourceMeta {
  name: string;
  namespace?: string;
  uid?: string;
  resourceVersion?: string;
  labels?: Record<string, string>;
  annotations?: Record<string, string>;
}

export interface Platform {
  apiVersion: `${typeof GROUP}/${typeof VERSION}`;
  kind: 'Platform';
  metadata: ResourceMeta;
  spec: PlatformSpec;
  status?: { phase?: string; iamRoleArn?: string; namespace?: string };
}

export interface ModelGateway {
  apiVersion: `${typeof GROUP}/${typeof VERSION}`;
  kind: 'ModelGateway';
  metadata: ResourceMeta;
  spec: {
    platformRef: { name: string };
    routes: ModelRouteSpec[];
    defaultGuardrailRef?: { name: string };
  };
  status?: { phase?: string; endpoint?: string };
}

export interface ClientOptions {
  /** Optional explicit kubeconfig path; defaults to KUBECONFIG env or in-cluster config. */
  kubeconfigPath?: string;
  context?: string;
}

export class EksAgentClient {
  readonly api: CustomObjectsApi;

  constructor(opts: ClientOptions = {}) {
    const kc = new KubeConfig();
    if (opts.kubeconfigPath) {
      kc.loadFromFile(opts.kubeconfigPath);
    } else if (process.env.KUBECONFIG) {
      kc.loadFromFile(process.env.KUBECONFIG);
    } else if (process.env.KUBERNETES_SERVICE_HOST) {
      kc.loadFromCluster();
    } else {
      kc.loadFromDefault();
    }
    if (opts.context) kc.setCurrentContext(opts.context);
    this.api = kc.makeApiClient(CustomObjectsApi);
  }

  async listPlatforms(): Promise<Platform[]> {
    const r: unknown = await this.api.listClusterCustomObject({ group: GROUP, version: VERSION, plural: 'platforms' });
    return (r as { items?: Platform[] }).items ?? [];
  }

  async getPlatform(name: string): Promise<Platform> {
    const r: unknown = await this.api.getClusterCustomObject({
      group: GROUP,
      version: VERSION,
      plural: 'platforms',
      name,
    });
    return r as Platform;
  }

  async applyPlatform(p: Platform): Promise<Platform> {
    const r: unknown = await this.api.createClusterCustomObject({
      group: GROUP,
      version: VERSION,
      plural: 'platforms',
      body: p,
    });
    return r as Platform;
  }

  async deletePlatform(name: string): Promise<void> {
    await this.api.deleteClusterCustomObject({ group: GROUP, version: VERSION, plural: 'platforms', name });
  }

  async listModelGateways(namespace: string): Promise<ModelGateway[]> {
    const r: unknown = await this.api.listNamespacedCustomObject({
      group: GROUP,
      version: VERSION,
      namespace,
      plural: 'modelgateways',
    });
    return (r as { items?: ModelGateway[] }).items ?? [];
  }
}
