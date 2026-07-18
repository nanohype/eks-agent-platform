#!/usr/bin/env tsx
/**
 * TS↔CRD schema-mirror drift gate.
 *
 * `packages/core/src/schemas.ts` declares zod schemas that mirror the operator's
 * Go CRD types — they are the runtime validation contract for every TS consumer
 * that reads a CR back out of the cluster (via `packages/client`). Because zod
 * strips unknown keys, a field the operator writes but the zod schema doesn't
 * model is silently invisible to those consumers — which is exactly how the
 * typed client once went blind to a Platform's suspension state.
 *
 * This gate closes that class. It renders each mirrored zod schema to a JSON
 * schema and diffs its `spec`/`status` property tree against the generated CRD
 * OpenAPI schema (`operators/config/crd/bases/*.yaml`, itself generated from the
 * Go types by controller-gen). A spec or status field present on one side but
 * not the other fails the build — the same regenerate-and-diff discipline the
 * repo already applies to the CRD reference and the Lambda pricing table.
 *
 * It compares property *names* recursively (spec, status, and every nested
 * object / array element), not types or constraints — the goal is to make a
 * missing mirrored field impossible to merge, not to re-validate OpenAPI.
 *
 * Usage: `pnpm check:schema-drift` (or `tsx scripts/check-schema-drift.mts`).
 */

import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import { parse as parseYaml } from 'yaml';
import { z } from 'zod';

import {
  ModelGatewaySpec,
  ModelGatewayStatus,
  PlatformSpec,
  PlatformStatus,
} from '../packages/core/src/schemas.ts';

const here = dirname(fileURLToPath(import.meta.url));
const repoRoot = join(here, '..');
const crdDir = join(repoRoot, 'operators/config/crd/bases');

const CRD_VERSION = 'v1alpha1';

/** Each mirrored resource: its CRD file plus the zod spec/status schemas. */
const RESOURCES = [
  {
    label: 'Platform',
    crdFile: 'platform.nanohype.dev_platforms.yaml',
    spec: PlatformSpec,
    status: PlatformStatus,
  },
  {
    label: 'ModelGateway',
    crdFile: 'agents.nanohype.dev_modelgateways.yaml',
    spec: ModelGatewaySpec,
    status: ModelGatewayStatus,
  },
] as const;

type JsonNode = Record<string, unknown> | undefined;

/** Resolve a `$ref` (zod may hoist a reused schema into `$defs`). */
function deref(node: JsonNode, root: JsonNode): JsonNode {
  if (node && typeof node.$ref === 'string') {
    const parts = node.$ref.replace(/^#\//, '').split('/');
    let cur: unknown = root;
    for (const p of parts) cur = (cur as Record<string, unknown> | undefined)?.[p];
    return (cur as JsonNode) ?? node;
  }
  return node;
}

function props(node: JsonNode): Record<string, JsonNode> | null {
  if (node && node.properties && typeof node.properties === 'object') {
    return node.properties as Record<string, JsonNode>;
  }
  return null;
}

/**
 * Recursively compare a zod-derived JSON node against a CRD OpenAPI node,
 * appending a human-readable message for every property-name divergence.
 */
function compare(
  path: string,
  zodNode: JsonNode,
  crdNode: JsonNode,
  zodRoot: JsonNode,
  drifts: string[],
): void {
  zodNode = deref(zodNode, zodRoot);

  // Array: descend into the element schema.
  if (zodNode?.type === 'array' || crdNode?.type === 'array') {
    const zi = deref(zodNode?.items as JsonNode, zodRoot);
    const ci = crdNode?.items as JsonNode;
    if (zi && ci) compare(`${path}[]`, zi, ci, zodRoot, drifts);
    return;
  }

  const zp = props(zodNode);
  const cp = props(crdNode);
  if (!zp && !cp) return; // leaf on both sides — names already matched by the caller
  if (!zp || !cp) {
    drifts.push(
      `${path}: shape mismatch (zod ${zp ? 'object' : 'scalar'} vs CRD ${cp ? 'object' : 'scalar'})`,
    );
    return;
  }

  for (const k of Object.keys(cp)) {
    if (!(k in zp)) drifts.push(`${path}.${k}: present in the CRD, missing from the zod schema`);
  }
  for (const k of Object.keys(zp)) {
    if (!(k in cp)) drifts.push(`${path}.${k}: present in the zod schema, missing from the CRD`);
  }
  for (const k of Object.keys(cp)) {
    if (k in zp) compare(`${path}.${k}`, zp[k], cp[k], zodRoot, drifts);
  }
}

function crdSchema(crdFile: string): { spec: JsonNode; status: JsonNode } {
  const doc = parseYaml(readFileSync(join(crdDir, crdFile), 'utf8')) as {
    spec: { versions: { name: string; schema: { openAPIV3Schema: JsonNode } }[] };
  };
  const version = doc.spec.versions.find((v) => v.name === CRD_VERSION);
  if (!version) throw new Error(`${crdFile}: no ${CRD_VERSION} version found`);
  const top = version.schema.openAPIV3Schema;
  const p = props(top);
  return { spec: p?.spec, status: p?.status };
}

function main(): void {
  const drifts: string[] = [];

  for (const r of RESOURCES) {
    const crd = crdSchema(r.crdFile);
    // Property names only — no type/constraint checking, no $throw on
    // JSON-Schema-unrepresentable zod types (they compare as leaves).
    const opts = { reused: 'inline', unrepresentable: 'any' } as const;
    const zodSpec = z.toJSONSchema(r.spec, opts) as JsonNode;
    const zodStatus = z.toJSONSchema(r.status, opts) as JsonNode;

    if (!crd.spec) {
      drifts.push(`${r.label}: CRD has no spec schema`);
    } else {
      compare(`${r.label}.spec`, zodSpec, crd.spec, zodSpec, drifts);
    }
    if (!crd.status) {
      drifts.push(`${r.label}: CRD has no status schema`);
    } else {
      compare(`${r.label}.status`, zodStatus, crd.status, zodStatus, drifts);
    }
  }

  if (drifts.length > 0) {
    // eslint-disable-next-line no-console
    console.error(
      'TS↔CRD schema drift detected — packages/core/src/schemas.ts has diverged\n' +
        'from the generated CRD OpenAPI schemas:\n\n' +
        drifts.map((d) => `  - ${d}`).join('\n') +
        '\n\nAdd or remove the field in packages/core/src/schemas.ts to match the Go\n' +
        'CRD types (and run `make crd-docs` in operators/ if you changed the Go side).',
    );
    process.exit(1);
  }
  // eslint-disable-next-line no-console
  console.log(
    `schemas.ts mirrors the CRD OpenAPI schemas (${RESOURCES.map((r) => r.label).join(', ')}): no drift.`,
  );
}

main();
