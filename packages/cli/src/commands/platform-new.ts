import { writeFileSync, mkdirSync, existsSync } from 'node:fs';
import { join } from 'node:path';

import { PlatformPersona } from '@eks-agent/core';
import chalk from 'chalk';
import { stringify } from 'yaml';

interface PlatformNewOpts {
  name: string;
  tenant: string;
  persona: string;
  monthlyUsd: number;
  output: string;
}

const PERSONA_DEFAULTS: Record<
  string,
  { modelFamily: string; modelId: string; agentName: string; systemPrompt: string }
> = {
  'sales-ops': {
    modelFamily: 'anthropic',
    modelId: 'us.anthropic.claude-3-5-sonnet-20241022-v2:0',
    agentName: 'objection-handler',
    systemPrompt: 'You help sales-ops staff handle customer objections with cited references.',
  },
  support: {
    modelFamily: 'meta',
    modelId: 'meta.llama3-1-70b-instruct-v1:0',
    agentName: 'ticket-summarizer',
    systemPrompt: 'You summarize support tickets into a one-paragraph diagnosis + next step.',
  },
  finance: {
    modelFamily: 'amazon-nova',
    modelId: 'amazon.nova-pro-v1:0',
    agentName: 'financial-memo',
    systemPrompt: 'You draft financial memos. Always show your assumptions and cite sources.',
  },
  marketing: {
    modelFamily: 'anthropic',
    modelId: 'anthropic.claude-3-5-haiku-20241022-v1:0',
    agentName: 'campaign-brief',
    systemPrompt: 'You draft campaign briefs in 5 sections. Be concise and concrete.',
  },
  ops: {
    modelFamily: 'mistral',
    modelId: 'mistral.mistral-large-2407-v1:0',
    agentName: 'oncall-summarizer',
    systemPrompt: 'You summarize on-call incidents into a runbook update candidate.',
  },
  founder: {
    modelFamily: 'anthropic',
    modelId: 'us.anthropic.claude-3-5-sonnet-20241022-v2:0',
    agentName: 'strategy-memo',
    systemPrompt: 'You help draft strategy memos. Push back on weak reasoning.',
  },
  eng: {
    modelFamily: 'anthropic',
    modelId: 'us.anthropic.claude-3-5-sonnet-20241022-v2:0',
    agentName: 'adr-drafter',
    systemPrompt: 'You draft Architectural Decision Records. Show trade-offs explicitly.',
  },
  legal: {
    modelFamily: 'anthropic',
    modelId: 'us.anthropic.claude-3-5-sonnet-20241022-v2:0',
    agentName: 'policy-reviewer',
    systemPrompt: 'You review policy text against jurisdiction-specific compliance requirements.',
  },
  generic: {
    modelFamily: 'anthropic',
    modelId: 'us.anthropic.claude-3-5-sonnet-20241022-v2:0',
    agentName: 'assistant',
    systemPrompt: 'You are a helpful assistant.',
  },
};

export function platformNew(opts: PlatformNewOpts): void {
  const persona = PlatformPersona.parse(opts.persona);
  // persona is constrained by PlatformPersona.parse above; the lookup is safe.
  // eslint-disable-next-line security/detect-object-injection
  const defaults = PERSONA_DEFAULTS[persona] ?? PERSONA_DEFAULTS.generic!;
  const outDir = join(opts.output, opts.name);
  // CLI tool writes files the user explicitly asked for under --output. Path
  // traversal is not a meaningful concern here — the user runs this on their
  // own machine to scaffold their own files.
  // eslint-disable-next-line security/detect-non-literal-fs-filename
  if (existsSync(outDir)) {
    throw new Error(`refusing to overwrite existing directory: ${outDir}`);
  }
  // eslint-disable-next-line security/detect-non-literal-fs-filename
  mkdirSync(outDir, { recursive: true });

  const docs = [
    {
      apiVersion: 'platform.nanohype.dev/v1alpha1',
      kind: 'Platform',
      metadata: {
        name: opts.name,
        labels: { 'agents.nanohype.dev/persona': persona, 'agents.nanohype.dev/tenant': opts.tenant },
      },
      spec: {
        displayName: opts.name,
        persona,
        tenant: opts.tenant,
        isolation: 'namespace',
        budget: { name: `${opts.name}-budget` },
        identity: { allowedModelFamilies: [defaults.modelFamily] },
        compliance: { soc2: true, hipaa: persona === 'legal' },
      },
    },
    {
      apiVersion: 'governance.nanohype.dev/v1alpha1',
      kind: 'BudgetPolicy',
      metadata: { name: `${opts.name}-budget`, labels: { 'agents.nanohype.dev/tenant': opts.tenant } },
      spec: {
        platformRef: { name: opts.name },
        monthlyUsd: String(opts.monthlyUsd),
        alertThresholdsPercent: [50, 80, 100],
        killSwitchEnabled: true,
      },
    },
    {
      apiVersion: 'agents.nanohype.dev/v1alpha1',
      kind: 'ModelGateway',
      metadata: { name: `${opts.name}-gateway` },
      spec: {
        platformRef: { name: opts.name },
        routes: [{ name: 'primary', modelFamily: defaults.modelFamily, modelId: defaults.modelId, rateLimit: 60 }],
      },
    },
    {
      apiVersion: 'agents.nanohype.dev/v1alpha1',
      kind: 'AgentFleet',
      metadata: { name: `${opts.name}-fleet` },
      spec: {
        platformRef: { name: opts.name },
        scaling: { enabled: true, min: 1, max: 5, queueDepthTrigger: 10 },
        agents: [{ name: defaults.agentName, systemPrompt: defaults.systemPrompt, modelRoute: 'primary' }],
      },
    },
    {
      apiVersion: 'governance.nanohype.dev/v1alpha1',
      kind: 'EvalSuite',
      metadata: { name: `${opts.name}-eval` },
      spec: {
        platformRef: { name: opts.name },
        agentFleetRef: { name: `${opts.name}-fleet` },
        schedule: '0 6 * * *',
        passThreshold: '0.85',
        cases: [{ name: 'smoke-test', input: "Reply with 'pong'.", expectContains: ['pong'], maxLatencyMs: 5000 }],
      },
    },
  ];

  const yaml = docs.map((d) => stringify(d)).join('---\n');
  const yamlPath = join(outDir, 'platform.yaml');
  // eslint-disable-next-line security/detect-non-literal-fs-filename
  writeFileSync(yamlPath, yaml, 'utf8');

  const readmePath = join(outDir, 'README.md');
  // eslint-disable-next-line security/detect-non-literal-fs-filename
  writeFileSync(
    readmePath,
    `# ${opts.name}\n\nGenerated tenant scaffold for persona **${persona}**.\n\nApply: \`kubectl apply -f platform.yaml\`.\n`,
    'utf8',
  );

  // eslint-disable-next-line no-console
  console.log(chalk.green(`✔`), `wrote`, chalk.bold(yamlPath));
}
