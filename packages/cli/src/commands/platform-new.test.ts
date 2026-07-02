import { mkdtempSync, readFileSync, rmSync, mkdirSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { parseAllDocuments } from 'yaml';

import { platformNew } from './platform-new.js';

describe('platformNew', () => {
  let outDir: string;

  beforeEach(() => {
    outDir = mkdtempSync(join(tmpdir(), 'agentctl-test-'));
  });

  afterEach(() => {
    rmSync(outDir, { recursive: true, force: true });
  });

  const scaffold = (persona = 'generic') => {
    platformNew({
      name: 'acme-assist',
      tenant: 'acme',
      persona,
      monthlyUsd: 250,
      output: outDir,
    });
    // Test reads back the scaffold it just wrote into a mkdtemp sandbox.
    // eslint-disable-next-line security/detect-non-literal-fs-filename
    const yaml = readFileSync(join(outDir, 'acme-assist', 'platform.yaml'), 'utf8');
    return parseAllDocuments(yaml).map((d) => d.toJS() as Record<string, unknown>);
  };

  it('emits the five-document tenant scaffold wired to one Platform', () => {
    const docs = scaffold();
    expect(docs.map((d) => d.kind)).toEqual([
      'Platform',
      'BudgetPolicy',
      'ModelGateway',
      'AgentFleet',
      'EvalSuite',
    ]);
    const [, budget, gateway, fleet, evalSuite] = docs as {
      spec: { platformRef?: { name: string }; agentFleetRef?: { name: string } };
    }[];
    for (const doc of [budget, gateway, fleet, evalSuite]) {
      expect(doc?.spec.platformRef).toEqual({ name: 'acme-assist' });
    }
    expect(evalSuite?.spec.agentFleetRef).toEqual({ name: 'acme-assist-fleet' });
  });

  it('carries the budget and tenant through to the emitted specs', () => {
    const docs = scaffold();
    const platform = docs[0] as { spec: { tenant: string; budget: { name: string } } };
    const budget = docs[1] as { spec: { monthlyUsd: string; killSwitchEnabled: boolean } };
    expect(platform.spec.tenant).toBe('acme');
    expect(platform.spec.budget.name).toBe('acme-assist-budget');
    expect(budget.spec.monthlyUsd).toBe('250');
    expect(budget.spec.killSwitchEnabled).toBe(true);
  });

  it('routes the fleet agent at the gateway primary route for the persona', () => {
    const docs = scaffold('support');
    const gateway = docs[2] as { spec: { routes: { name: string; modelId: string }[] } };
    const fleet = docs[3] as { spec: { agents: { modelRoute: string }[] } };
    expect(gateway.spec.routes[0]?.name).toBe('primary');
    expect(gateway.spec.routes[0]?.modelId).toContain('llama');
    expect(fleet.spec.agents[0]?.modelRoute).toBe('primary');
  });

  it('rejects personas outside the schema', () => {
    expect(() =>
      platformNew({
        name: 'acme-assist',
        tenant: 'acme',
        persona: 'astrologer',
        monthlyUsd: 250,
        output: outDir,
      }),
    ).toThrow();
  });

  it('refuses to overwrite an existing scaffold directory', () => {
    // eslint-disable-next-line security/detect-non-literal-fs-filename
    mkdirSync(join(outDir, 'acme-assist'));
    expect(() =>
      platformNew({
        name: 'acme-assist',
        tenant: 'acme',
        persona: 'generic',
        monthlyUsd: 250,
        output: outDir,
      }),
    ).toThrow(/refusing to overwrite/);
  });
});
