#!/usr/bin/env node
import { Command } from 'commander';

import { platformNew } from './commands/platform-new.js';

const program = new Command();

program
  .name('agentctl')
  .description('CLI for declaring and managing eks-agent-platform tenants')
  .version('0.0.0');

const platform = program.command('platform').description('Manage Platform tenants');

platform
  .command('new')
  .description('Scaffold a new Platform tenant')
  .requiredOption('--name <name>', 'Platform name (lowercase, alphanumeric + hyphens)')
  .requiredOption('--tenant <tenant>', 'Owning Tenant ID')
  .option(
    '--persona <persona>',
    'Persona: sales-ops|support|finance|ops|founder|eng|marketing|legal|generic',
    'generic',
  )
  .option('--monthly-usd <usd>', 'Monthly USD budget', (v) => parseInt(v, 10), 500)
  .option('--output <dir>', 'Output directory', '.')
  .action(
    (opts: {
      name: string;
      tenant: string;
      persona: string;
      monthlyUsd: number;
      output: string;
    }) => {
      platformNew(opts);
    },
  );

program.parseAsync().catch((err: unknown) => {
  console.error(err instanceof Error ? err.message : String(err));
  process.exit(1);
});
