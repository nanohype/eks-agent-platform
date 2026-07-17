#!/usr/bin/env node
/**
 * Copy the model-default SSOT into the TypeScript scaffolder package.
 *
 * The canonical file is operators/internal/agentctl/model_defaults.json, which
 * the Go agentctl CLI embeds via go:embed. TypeScript can't import a file
 * outside its own package tree, so the scaffolder (packages/cli) consumes a
 * byte-identical copy at packages/cli/src/data/model-defaults.json. This script
 * writes that copy; CI runs it with --check so an edit to the canonical file
 * that isn't propagated (or a hand-edit of the copy) fails the build.
 *
 * Usage:
 *   node scripts/gen-model-defaults.mjs           # write the copy
 *   node scripts/gen-model-defaults.mjs --check    # exit 1 if it is stale
 */

import { readFileSync, writeFileSync, mkdirSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const repoRoot = join(here, '..');
const SSOT = join(repoRoot, 'operators/internal/agentctl/model_defaults.json');
const COPY = join(repoRoot, 'packages/cli/src/data/model-defaults.json');

const canonical = readFileSync(SSOT, 'utf8');
const check = process.argv.includes('--check');

if (check) {
  let current = '';
  try {
    current = readFileSync(COPY, 'utf8');
  } catch {
    current = '';
  }
  if (current !== canonical) {
    // eslint-disable-next-line no-console
    console.error(
      'packages/cli/src/data/model-defaults.json is stale — regenerate with\n' +
        '`node scripts/gen-model-defaults.mjs` and commit it (the TS copy must\n' +
        'match operators/internal/agentctl/model_defaults.json).',
    );
    process.exit(1);
  }
  // eslint-disable-next-line no-console
  console.log('model-defaults TS copy is in sync with the SSOT.');
} else {
  mkdirSync(dirname(COPY), { recursive: true });
  writeFileSync(COPY, canonical, 'utf8');
  // eslint-disable-next-line no-console
  console.log(`wrote ${COPY}`);
}
