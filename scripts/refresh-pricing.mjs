#!/usr/bin/env node
/**
 * Refresh Bedrock model prices in the pricing SSOT from the AWS Price List API.
 *
 * The single source of truth is packages/pricing/src/data/bedrock-pricing.json.
 * Bedrock pricing changes regularly (new models, occasional drops) and stale
 * numbers silently undercount spend — a correctness issue for
 * BudgetPolicy.status.percentOfBudget math and downstream kill-switch
 * decisions. This script pulls current on-demand token prices from the AWS
 * Price List Query API (service AmazonBedrock) and rewrites the matching input
 * / output per-million-token prices in the JSON, then re-derives the Anthropic
 * prompt-caching prices (cache write = 1.25x input, cache read = 0.10x input).
 *
 * Refresh cadence: weekly. Run it from a scheduled GitHub Actions workflow (or
 * by hand) with AWS credentials that allow `pricing:GetProducts`
 * (AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY, plus AWS_SESSION_TOKEN for
 * temporary credentials); the Price List API lives only in us-east-1 and
 * ap-south-1. After it writes the JSON, regenerate the derived tables and open
 * a PR:
 *
 *   node scripts/refresh-pricing.mjs
 *   node scripts/gen-lambda-pricing.mjs   # keep the Lambda table in sync
 *   pnpm --filter @eks-agent/pricing build && pnpm --filter @eks-agent/pricing test
 *
 * The CI drift gate then verifies the generated Lambda table matches the JSON.
 * Renovate cannot do this — it watches package-manager manifests, not price
 * values inside a JSON data file.
 *
 * Usage:
 *   node scripts/refresh-pricing.mjs             # fetch + rewrite the JSON
 *   node scripts/refresh-pricing.mjs --dry-run    # print the diff, write nothing
 */

import { createHash, createHmac } from 'node:crypto';
import { readFileSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const repoRoot = join(here, '..');
const SSOT = join(repoRoot, 'packages/pricing/src/data/bedrock-pricing.json');

const REGION = process.env.AWS_PRICING_REGION ?? 'us-east-1';
const SERVICE = 'api.pricing';
const HOST = `api.pricing.${REGION}.amazonaws.com`;
const TARGET = 'AWSPriceListService.GetProducts';

const dryRun = process.argv.includes('--dry-run');

function requireCreds() {
  const accessKeyId = process.env.AWS_ACCESS_KEY_ID;
  const secretAccessKey = process.env.AWS_SECRET_ACCESS_KEY;
  if (!accessKeyId || !secretAccessKey) {
    console.error(
      'refresh-pricing: AWS credentials not found. Set AWS_ACCESS_KEY_ID and\n' +
        'AWS_SECRET_ACCESS_KEY (plus AWS_SESSION_TOKEN for temporary creds) for a\n' +
        'principal allowed to call pricing:GetProducts, then re-run.',
    );
    process.exit(2);
  }
  return { accessKeyId, secretAccessKey, sessionToken: process.env.AWS_SESSION_TOKEN };
}

const sha256hex = (data) => createHash('sha256').update(data, 'utf8').digest('hex');
const hmac = (key, data) => createHmac('sha256', key).update(data, 'utf8').digest();

/** Sign and send one GetProducts request. */
async function getProducts(creds, body) {
  const payload = JSON.stringify(body);
  const now = new Date();
  const amzDate = now.toISOString().replace(/[:-]|\.\d{3}/g, '');
  const dateStamp = amzDate.slice(0, 8);

  const headers = {
    'content-type': 'application/x-amz-json-1.1',
    host: HOST,
    'x-amz-date': amzDate,
    'x-amz-target': TARGET,
  };
  if (creds.sessionToken) headers['x-amz-security-token'] = creds.sessionToken;

  const signedHeaders = Object.keys(headers).sort().join(';');
  const canonicalHeaders =
    Object.keys(headers)
      .sort()
      .map((h) => `${h}:${headers[h]}\n`)
      .join('') + '';
  const canonicalRequest = [
    'POST',
    '/',
    '',
    canonicalHeaders,
    signedHeaders,
    sha256hex(payload),
  ].join('\n');

  const scope = `${dateStamp}/${REGION}/${SERVICE}/aws4_request`;
  const stringToSign = ['AWS4-HMAC-SHA256', amzDate, scope, sha256hex(canonicalRequest)].join('\n');

  const kDate = hmac(`AWS4${creds.secretAccessKey}`, dateStamp);
  const kRegion = hmac(kDate, REGION);
  const kService = hmac(kRegion, SERVICE);
  const kSigning = hmac(kService, 'aws4_request');
  const signature = createHmac('sha256', kSigning).update(stringToSign, 'utf8').digest('hex');

  headers.authorization =
    `AWS4-HMAC-SHA256 Credential=${creds.accessKeyId}/${scope}, ` +
    `SignedHeaders=${signedHeaders}, Signature=${signature}`;

  const res = await fetch(`https://${HOST}/`, { method: 'POST', headers, body: payload });
  if (!res.ok) {
    throw new Error(`GetProducts ${res.status}: ${await res.text()}`);
  }
  return res.json();
}

/** Normalize a model name / id to a comparable token (alnum, lowercased). */
const norm = (s) =>
  String(s)
    .toLowerCase()
    .replace(/[^a-z0-9]/g, '');

/**
 * Map an AWS Price List product's model attribute to a Bedrock model id in the
 * SSOT. Matches on the model-name portion of the id (after the provider
 * prefix), so "Claude Sonnet 4.6" resolves to "anthropic.claude-sonnet-4-6".
 */
function matchModelId(ids, modelName) {
  const target = norm(modelName);
  if (!target) return undefined;
  return ids.find((id) => {
    const core = norm(id.split('.').slice(1).join('.'));
    return core.startsWith(target) || target.startsWith(core) || core.includes(target);
  });
}

function classifyDirection(attrs, pd) {
  const hay = norm(
    [attrs.usagetype, attrs.inferenceType, attrs.feature, pd.description].filter(Boolean).join(' '),
  );
  if (hay.includes('inputtoken') || hay.includes('input')) return 'input';
  if (hay.includes('outputtoken') || hay.includes('output')) return 'output';
  return undefined;
}

/** Price per unit → USD per 1,000,000 tokens, using the dimension's unit. */
function toPerMillion(usd, unit) {
  const per = Number(usd);
  if (!Number.isFinite(per) || per <= 0) return undefined;
  const u = norm(unit);
  if (u.includes('1ktokens') || u.includes('1000tokens')) return per * 1000;
  if (u.includes('1mtokens') || u.includes('1000000tokens')) return per;
  if (u.includes('tokens')) return per * 1_000_000; // per single token
  return undefined;
}

async function main() {
  const creds = requireCreds();
  const doc = JSON.parse(readFileSync(SSOT, 'utf8'));
  const ids = Object.keys(doc.models);

  // fetched[id] = { input?, output? } in per-million-token USD.
  const fetched = {};
  let nextToken;
  do {
    const body = {
      ServiceCode: 'AmazonBedrock',
      FormatVersion: 'aws_v1',
      Filters: [{ Type: 'TERM_MATCH', Field: 'termType', Value: 'OnDemand' }],
      MaxResults: 100,
    };
    if (nextToken) body.NextToken = nextToken;
    // eslint-disable-next-line no-await-in-loop
    const page = await getProducts(creds, body);
    for (const raw of page.PriceList ?? []) {
      const product = typeof raw === 'string' ? JSON.parse(raw) : raw;
      const attrs = product.product?.attributes ?? {};
      const modelName = attrs.model ?? attrs.titanModel ?? attrs.modelName;
      const id = matchModelId(ids, modelName);
      if (!id) continue;
      const onDemand = product.terms?.OnDemand ?? {};
      for (const offer of Object.values(onDemand)) {
        for (const pd of Object.values(offer.priceDimensions ?? {})) {
          const direction = classifyDirection(attrs, pd);
          const perM = toPerMillion(pd.pricePerUnit?.USD, pd.unit);
          if (!direction || perM === undefined) continue;
          fetched[id] = fetched[id] ?? {};
          fetched[id][direction] = perM;
        }
      }
    }
    nextToken = page.NextToken;
  } while (nextToken);

  const changes = [];
  for (const [id, price] of Object.entries(fetched)) {
    const entry = doc.models[id];
    if (!entry) continue;
    if (price.input !== undefined && entry.inputPerMillion !== price.input) {
      changes.push(`${id} input ${entry.inputPerMillion} -> ${price.input}`);
      entry.inputPerMillion = price.input;
      if (entry.cacheWritePerMillion !== undefined) {
        entry.cacheWritePerMillion = Number((price.input * 1.25).toFixed(4));
      }
      if (entry.cacheReadPerMillion !== undefined) {
        entry.cacheReadPerMillion = Number((price.input * 0.1).toFixed(4));
      }
    }
    if (price.output !== undefined && entry.outputPerMillion !== price.output) {
      changes.push(`${id} output ${entry.outputPerMillion} -> ${price.output}`);
      entry.outputPerMillion = price.output;
    }
  }

  const matched = Object.keys(fetched);
  const unmatched = ids.filter((id) => !matched.includes(id));

  if (changes.length === 0) {
    console.log('refresh-pricing: prices already current — no changes.');
  } else {
    console.log(`refresh-pricing: ${changes.length} price change(s):`);
    for (const c of changes) console.log(`  ${c}`);
  }
  if (unmatched.length) {
    console.log(
      `refresh-pricing: ${unmatched.length} model(s) not matched in the Price List ` +
        `(verify by hand): ${unmatched.join(', ')}`,
    );
  }

  if (dryRun) {
    console.log('refresh-pricing: --dry-run, wrote nothing.');
    return;
  }
  writeFileSync(SSOT, `${JSON.stringify(doc, null, 2)}\n`, 'utf8');
  console.log(`refresh-pricing: wrote ${SSOT}. Regenerate derived tables:`);
  console.log('  node scripts/gen-lambda-pricing.mjs');
}

main().catch((err) => {
  console.error(`refresh-pricing: ${err.message}`);
  process.exit(1);
});
