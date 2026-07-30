#!/usr/bin/env node
/**
 * Refresh Bedrock model prices in the pricing SSOT from the AWS Price List API.
 *
 * The single source of truth is packages/pricing/src/data/bedrock-pricing.json.
 * Bedrock pricing changes regularly (new models, occasional drops) and stale
 * numbers silently undercount spend — a correctness issue for
 * BudgetPolicy.status.percentOfBudget math and downstream kill-switch decisions.
 *
 * Bedrock pricing is split across two Price List service codes with different
 * schemas, and both are needed:
 *
 *   AmazonBedrock                  Amazon first-party (Nova, Titan) and the
 *                                  open-weight catalog (Llama, Mistral, Qwen,
 *                                  DeepSeek, …). Model name in the `model`
 *                                  attribute; direction in `inferenceType`.
 *   AmazonBedrockFoundationModels  The marketplace path — every Anthropic model,
 *                                  plus Cohere. Model name in the `servicename`
 *                                  attribute as "Claude Opus 5 (Amazon Bedrock
 *                                  Edition)"; direction only in the price
 *                                  dimension's description. There is no `model`
 *                                  attribute here, and the usagetype
 *                                  (`USW2-MP:USW2_output_tokens_global_standard-Units`)
 *                                  carries no model name.
 *
 * Prices are region-specific, so the region is an input rather than whatever
 * pagination happened to yield last, and the region that was priced is written
 * into the JSON next to the numbers.
 *
 * Refresh cadence: weekly, run by hand. It needs AWS credentials that allow
 * `pricing:GetProducts` (AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY, plus
 * AWS_SESSION_TOKEN for temporary credentials) and the CI workflows have no AWS
 * identity, so scheduling it waits on one; the Price List API lives only in
 * us-east-1 and ap-south-1. What CI does run is `--self-test`, which covers the
 * classifiers and the SSOT's declarations without credentials — that is where
 * both of the bugs that produced wrong prices lived.
 *
 * After it writes the JSON, regenerate the derived tables and open a PR:
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
 *   node scripts/refresh-pricing.mjs              # fetch + rewrite the JSON
 *   node scripts/refresh-pricing.mjs --dry-run    # print the diff, write nothing
 *   node scripts/refresh-pricing.mjs --self-test  # check the parsers, no network
 *
 * Environment:
 *   AWS_PRICING_REGION        Price List API endpoint. us-east-1 (default) or ap-south-1.
 *   AWS_PRICING_MODEL_REGION  Region whose prices to read. Default us-west-2.
 */

import { createHash, createHmac } from 'node:crypto';
import { readFileSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const repoRoot = join(here, '..');
const SSOT = join(repoRoot, 'packages/pricing/src/data/bedrock-pricing.json');

const REGION = process.env.AWS_PRICING_REGION ?? 'us-east-1';
// The region whose prices land in the SSOT. Bedrock token prices vary by region
// by as much as 60% (Nova Pro on-demand input is 0.80/1M in us-west-2 and 1.28 in
// eu-south-1), and the same model is offered at several service tiers, so a query
// without a region filter returns a set that cannot be reduced to one number.
// us-west-2 is where the org invokes Bedrock.
const MODEL_REGION = process.env.AWS_PRICING_MODEL_REGION ?? 'us-west-2';
// The SigV4 credential scope for the Price List Query API is `pricing`, which is
// not the hostname's first label — signing with `api.pricing` is rejected with
// `InvalidSignatureException: Credential should be scoped to correct service`.
const SERVICE = 'pricing';
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
  const canonicalHeaders = Object.keys(headers)
    .sort()
    .map((h) => `${h}:${headers[h]}\n`)
    .join('');
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

/** Page through GetProducts for one service code in the priced region. */
async function* products(creds, serviceCode) {
  let nextToken;
  do {
    const body = {
      ServiceCode: serviceCode,
      FormatVersion: 'aws_v1',
      Filters: [
        { Type: 'TERM_MATCH', Field: 'regionCode', Value: MODEL_REGION },
        { Type: 'TERM_MATCH', Field: 'termType', Value: 'OnDemand' },
      ],
      MaxResults: 100,
    };
    if (nextToken) body.NextToken = nextToken;
    const page = await getProducts(creds, body);
    for (const raw of page.PriceList ?? []) {
      yield typeof raw === 'string' ? JSON.parse(raw) : raw;
    }
    nextToken = page.NextToken;
  } while (nextToken);
}

// ── AWS product name → SSOT id ───────────────────────────────────────────────

/** Normalize a label to hyphen-separated lowercase tokens for classification. */
const norm = (s) =>
  String(s)
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-|-$/g, '');

/**
 * Compare key for a declared product name. Case and whitespace are forgiven;
 * nothing else is. Folding punctuation away — the obvious reuse of {@link norm} —
 * makes "Cohere Command R+" and "Cohere Command R" the same key, and they are two
 * models at different prices.
 */
const nameKey = (s) => String(s).toLowerCase().replace(/\s+/g, ' ').trim();

/**
 * The Price List names a model differently from its invocation id, in ways no
 * rule derives: "Llama 3.1 70B" against `meta.llama3-1-70b-instruct-v1:0`,
 * "Mistral Large" against `mistral.mistral-large-2402-v1:0`, "Cohere Command R+"
 * against `cohere.command-r-plus-v1:0`. Deriving one from the other needs a
 * heuristic, and a heuristic that is nearly right is worse here than none: fuzzy
 * substring matching is what let Sonnet 4's prices land in the Sonnet 4.6 entry.
 *
 * So each model in the SSOT declares the AWS product name it prices, in
 * `awsProductName`. That is a join key, not a price — it is stable across
 * refreshes while the numbers stay API-tracked, a model with no declaration is
 * reported rather than guessed at, and a declaration the Price List stops
 * returning is reported too.
 *
 * The name matches the `model` attribute in the legacy service code and the
 * `servicename` attribute (minus its " (Amazon Bedrock Edition)" suffix) in the
 * marketplace one.
 */
function buildNameIndex(models) {
  const index = new Map();
  const undeclared = [];
  const duplicates = [];
  for (const [id, entry] of Object.entries(models)) {
    const name = entry.awsProductName;
    if (!name) {
      undeclared.push(id);
      continue;
    }
    const key = nameKey(name);
    if (index.has(key)) duplicates.push(`${name}: ${index.get(key)} and ${id}`);
    index.set(key, id);
  }
  return { index, undeclared, duplicates };
}

const productSuffix = /\s*\(Amazon Bedrock Edition\)\s*$/;

// ── price dimensions → priced fields ─────────────────────────────────────────

/**
 * `inferenceType` values in the legacy service code that name a token price we
 * store, mapped to the SSOT field. Matched exactly: the family also carries
 * `Input tokens flex`, `Input tokens priority` and
 * `Prompt cache read input tokens flex`, which are different service tiers at
 * different prices. A substring test on "input" accepts all of them — and also
 * accepts `Prompt cache read input tokens` as an input price, overwriting the
 * real one.
 */
const LEGACY_FIELDS = {
  'Input tokens': 'input',
  'Output tokens': 'output',
  'Prompt cache read input tokens': 'cacheRead',
  'Prompt cache write input tokens': 'cacheWrite',
};

/**
 * Price-dimension descriptions in the marketplace service code. Three grammars
 * are in use — `Input Tokens - Standard, Global` on the newest models,
 * `Million Input Tokens Global` on the rest of the Anthropic family, and
 * `Price per 1 million input tokens` on Cohere — and the middle one calls output
 * tokens **Response** Tokens, so a search for "output" finds none of them.
 *
 * Scope ranks cross-region inference (the `Global` variant) above regional,
 * because every Anthropic model in the catalog is cross-region-only and the org
 * invokes through `us.anthropic.*` inference profiles. Pricing against the
 * regional rate would overstate every Anthropic line by ~10%.
 *
 * The legacy service code ranks below every marketplace scope. A handful of
 * models are listed in both (Claude 3 Haiku, Claude 3 Sonnet), and the
 * marketplace listing is the one that states its inference scope, so it wins
 * without the two being reported as a disagreement.
 */
const SCOPE_RANK = { global: 4, 'regional-cris': 3, regional: 2, '': 1, legacy: 0 };
const HYPHEN_FORM = /^(Input|Output|Cache Read|Cache Write) Tokens - Standard(?:, (Global))?$/;
const MILLION_FORM =
  /^Million (Input|Response|Cache Read Input|Cache Write Input) Tokens(?: (Global|Regional CRIS|Regional))?$/;
const PLAIN_FORM = /^Price per 1 million (input|output) tokens$/;
const KIND_FIELDS = {
  input: 'input',
  output: 'output',
  response: 'output',
  'cache-read': 'cacheRead',
  'cache-write': 'cacheWrite',
  'cache-read-input': 'cacheRead',
  'cache-write-input': 'cacheWrite',
};

/**
 * Dimensions priced per million tokens that are not a model's on-demand token
 * price: other service tiers, batch, the 1-hour cache TTL, fine-tuning and
 * provisioned capacity. Each of these names "Input" or "Tokens" and would
 * otherwise be read as one.
 */
const NOT_ON_DEMAND =
  /batch|latency optimized|1h ttl|1 hour|provisioned|reserved|tpm|customi|storage/i;

/** Classify a marketplace price dimension, or undefined if it is not one we store. */
function classifyDimension(description) {
  const label = String(description).split('|').pop().trim();
  if (NOT_ON_DEMAND.test(label)) return undefined;
  const m = HYPHEN_FORM.exec(label) ?? MILLION_FORM.exec(label) ?? PLAIN_FORM.exec(label);
  if (!m) return undefined;
  const field = KIND_FIELDS[norm(m[1])];
  if (!field) return undefined;
  return { field, scope: norm(m[2] ?? '') };
}

/**
 * Price per unit → USD per 1,000,000 tokens.
 *
 * Only the two token units are accepted. A permissive test (any unit naming
 * tokens) also matches `Training Tokens`, the fine-tuning unit, and scales a
 * per-token price by a million.
 *
 * Scaling a per-token price up is binary floating point, so it lands on values
 * like 1.6800000000000002 and 0.008749999999999999. This file is a hand-reviewed
 * source of truth refreshed on a schedule — unrounded noise makes every weekly
 * diff unreadable and buries the price change that actually moved. Six decimals
 * is finer than any published Bedrock token price and leaves the real value
 * intact.
 */
function toPerMillion(usd, unit) {
  const per = Number(usd);
  if (!Number.isFinite(per) || per <= 0) return undefined;
  const u = norm(unit);
  if (u === '1k-tokens') return round6(per * 1000);
  if (u === '1m-tokens') return round6(per);
  return undefined;
}

function round6(n) {
  return Number(n.toFixed(6));
}

// ── accumulation ─────────────────────────────────────────────────────────────

/**
 * Collect the best candidate per (model, field). A higher-ranked scope replaces a
 * lower one; two different prices at the same scope are a conflict rather than a
 * last-write-wins, which is how the table silently became a mix of regions and
 * service tiers.
 */
function makeSink() {
  const best = new Map();
  const conflicts = [];
  return {
    best,
    conflicts,
    offer(id, field, value, scope, label) {
      const key = `${id} ${field}`;
      const rank = SCOPE_RANK[scope] ?? -1;
      const held = best.get(key);
      if (!held || rank > held.rank) {
        best.set(key, { id, field, value, rank, scope, label });
        return;
      }
      if (rank === held.rank && held.value !== value) {
        conflicts.push(`${id} ${field}: ${held.value} (${held.label}) vs ${value} (${label})`);
      }
    },
  };
}

const lastSegment = (description) => String(description).split('|').pop().trim();

async function collect(creds, index, sink) {
  for await (const product of products(creds, 'AmazonBedrock')) {
    const attrs = product.product?.attributes ?? {};
    if (attrs.feature !== 'On-demand Inference') continue;
    const field = LEGACY_FIELDS[attrs.inferenceType];
    if (!field) continue;
    const id = index.get(nameKey(attrs.model ?? attrs.titanModel ?? ''));
    if (!id) continue;
    for (const offer of Object.values(product.terms?.OnDemand ?? {})) {
      for (const pd of Object.values(offer.priceDimensions ?? {})) {
        const perM = toPerMillion(pd.pricePerUnit?.USD, pd.unit);
        if (perM !== undefined) sink.offer(id, field, perM, 'legacy', attrs.inferenceType);
      }
    }
  }
  for await (const product of products(creds, 'AmazonBedrockFoundationModels')) {
    const attrs = product.product?.attributes ?? {};
    const id = index.get(nameKey(String(attrs.servicename ?? '').replace(productSuffix, '')));
    if (!id) continue;
    for (const offer of Object.values(product.terms?.OnDemand ?? {})) {
      for (const pd of Object.values(offer.priceDimensions ?? {})) {
        const kind = classifyDimension(pd.description ?? '');
        if (!kind) continue;
        const perM = toPerMillion(pd.pricePerUnit?.USD, pd.unit);
        if (perM !== undefined) {
          sink.offer(id, kind.field, perM, kind.scope, lastSegment(pd.description));
        }
      }
    }
  }
}

// ── self-test ────────────────────────────────────────────────────────────────

/**
 * Check the classifiers against the cases that produced wrong prices. Both bugs
 * were in classification rather than transport, so they are reachable with no
 * network and the check runs on every invocation.
 */
function selfTest() {
  const { index, undeclared, duplicates } = buildNameIndex({
    'anthropic.claude-sonnet-4-6': { awsProductName: 'Claude Sonnet 4.6' },
    'meta.llama3-1-70b-instruct-v1:0': { awsProductName: 'Llama 3.1 70B' },
    'cohere.command-r-plus-v1:0': { awsProductName: 'Cohere Command R+' },
    'cohere.command-r-v1:0': { awsProductName: 'Cohere Command R' },
    'amazon.titan-nothing': {},
  });
  const lookup = (name) => index.get(nameKey(String(name).replace(productSuffix, '')));
  const j = (v) => JSON.stringify(v);
  const cases = [
    // The declared join key, including the two shapes a heuristic gets wrong: a
    // name the id does not contain, and two names one prefix of the other.
    [lookup('Claude Sonnet 4.6 (Amazon Bedrock Edition)'), 'anthropic.claude-sonnet-4-6'],
    [lookup('Llama 3.1 70B'), 'meta.llama3-1-70b-instruct-v1:0'],
    [lookup('Cohere Command R+'), 'cohere.command-r-plus-v1:0'],
    [lookup('Cohere Command R'), 'cohere.command-r-v1:0'],
    // A near-miss resolves to nothing rather than to its neighbour.
    [lookup('Claude Sonnet 4'), undefined],
    [lookup('Llama 3.1 8B'), undefined],
    // An entry with no declaration is reported, not silently skipped.
    [undeclared, ['amazon.titan-nothing']],
    [duplicates, []],
    [
      buildNameIndex({ a: { awsProductName: 'Nova Pro' }, b: { awsProductName: 'nova pro' } })
        .duplicates,
      ['nova pro: a and b'],
    ],
    // Both description grammars, including Response Tokens as the output price.
    [
      classifyDimension('x|us-west-2|Input Tokens - Standard, Global'),
      { field: 'input', scope: 'global' },
    ],
    [classifyDimension('x|us-west-2|Output Tokens - Standard'), { field: 'output', scope: '' }],
    [
      classifyDimension('x|us-west-2|Cache Read Tokens - Standard, Global'),
      { field: 'cacheRead', scope: 'global' },
    ],
    [
      classifyDimension('x|us-west-2|Million Response Tokens Global'),
      { field: 'output', scope: 'global' },
    ],
    [
      classifyDimension('x|us-west-2|Million Input Tokens Regional CRIS'),
      { field: 'input', scope: 'regional-cris' },
    ],
    [
      classifyDimension('x|us-west-2|Million Cache Write Input Tokens'),
      { field: 'cacheWrite', scope: '' },
    ],
    [
      classifyDimension('x|us-west-2|Price per 1 million input tokens'),
      { field: 'input', scope: '' },
    ],
    [
      classifyDimension('x|us-west-2|Price per 1 million output tokens'),
      { field: 'output', scope: '' },
    ],
    // Everything that names tokens but is not an on-demand token price.
    [classifyDimension('x|Input Tokens - Batch, Global'), undefined],
    [classifyDimension('x|Million Batch Response Tokens Global'), undefined],
    [classifyDimension('x|Cache Write Tokens (1h TTL) - Standard, Global'), undefined],
    [classifyDimension('x|Million 1 hour Cache Write Input Tokens Global'), undefined],
    [classifyDimension('x|Million Input Tokens (Latency Optimized Inference)'), undefined],
    [classifyDimension('x|Million model customization tokens'), undefined],
    [classifyDimension('x|Per Hour per 1K Input TPM Reserved 1 Month Global'), undefined],
    [classifyDimension('x|Provisioned Throughput Hourly Price (No Commit)'), undefined],
    // Units: the fine-tuning unit must not be scaled as a per-token price.
    [toPerMillion('0.0008', '1K tokens'), 0.8],
    [toPerMillion('25.0000000000', '1M tokens'), 25],
    [toPerMillion('0.00799', 'Training Tokens'), undefined],
    [toPerMillion('13.08', 'hour'), undefined],
    [toPerMillion('0.0002', '1M TPM Hour'), undefined],
  ];

  let failed = 0;
  for (const [got, want] of cases) {
    if (j(got) !== j(want)) {
      failed += 1;
      console.error(`  FAIL want ${j(want)} got ${j(got)}`);
    }
  }
  // A same-scope disagreement must be reported, not resolved by arrival order.
  const sink = makeSink();
  sink.offer('m', 'input', 0.8, 'legacy', 'Input tokens');
  sink.offer('m', 'input', 1.68, 'legacy', 'Input tokens priority');
  if (sink.conflicts.length !== 1) {
    failed += 1;
    console.error(`  FAIL same-scope disagreement not reported: ${j(sink.conflicts)}`);
  }
  sink.offer('m', 'input', 0.75, 'global', 'Input Tokens - Standard, Global');
  if (sink.best.get('m input').value !== 0.75) {
    failed += 1;
    console.error('  FAIL cross-region scope did not win over the legacy listing');
  }

  // The real SSOT's join keys, checked here rather than only on a credentialed
  // run: a model added without a declaration is a model the refresh cannot price,
  // and it would sit in the table looking maintained until someone next runs this
  // with AWS access.
  const ssot = buildNameIndex(JSON.parse(readFileSync(SSOT, 'utf8')).models);
  if (ssot.undeclared.length) {
    failed += 1;
    console.error(`  FAIL no awsProductName: ${ssot.undeclared.join(', ')}`);
  }
  if (ssot.duplicates.length) {
    failed += 1;
    console.error(`  FAIL duplicate awsProductName: ${ssot.duplicates.join('; ')}`);
  }

  const total = cases.length + 4;
  if (failed) {
    console.error(`refresh-pricing: self-test failed (${failed} of ${total}).`);
    process.exit(1);
  }
  return total;
}

// ── main ─────────────────────────────────────────────────────────────────────

async function main() {
  const checked = selfTest();
  if (process.argv.includes('--self-test')) {
    console.log(`refresh-pricing: self-test passed (${checked} cases).`);
    return;
  }

  const creds = requireCreds();
  const doc = JSON.parse(readFileSync(SSOT, 'utf8'));
  const ids = Object.keys(doc.models);
  const { index, undeclared, duplicates } = buildNameIndex(doc.models);

  if (duplicates.length) {
    console.error('refresh-pricing: two models declare the same awsProductName:');
    for (const d of duplicates) console.error(`  ${d}`);
    process.exit(1);
  }
  if (undeclared.length) {
    console.error(
      `refresh-pricing: ${undeclared.length} model(s) declare no awsProductName, so they ` +
        `cannot be priced: ${undeclared.join(', ')}`,
    );
    process.exit(1);
  }

  const sink = makeSink();
  await collect(creds, index, sink);

  if (sink.conflicts.length) {
    console.error('refresh-pricing: the Price List returned disagreeing prices at the same scope:');
    for (const c of sink.conflicts) console.error(`  ${c}`);
    console.error('refresh-pricing: wrote nothing — resolve the ambiguity first.');
    process.exit(1);
  }

  const changes = [];
  const matched = new Set();
  const scopeOf = new Map();
  for (const { id, field, value, scope } of sink.best.values()) {
    const entry = doc.models[id];
    if (!entry) continue;
    matched.add(id);
    if (scope) scopeOf.set(id, scope);
    const key = `${field}PerMillion`;
    if (entry[key] !== value) {
      changes.push(`${id} ${field} ${entry[key] ?? '—'} -> ${value}`);
      entry[key] = value;
    }
  }

  doc.region = MODEL_REGION;

  if (changes.length === 0) {
    console.log(`refresh-pricing: prices already current for ${MODEL_REGION} — no changes.`);
  } else {
    console.log(`refresh-pricing: ${changes.length} price change(s) in ${MODEL_REGION}:`);
    for (const c of changes.sort()) console.log(`  ${c}`);
  }
  // Only a model the marketplace lists with an explicit scope can be "regional
  // instead of cross-region". The legacy service code states no scope at all, so
  // reporting its models here would read as a downgrade that did not happen.
  const regional = [...matched].filter((id) => {
    const scope = scopeOf.get(id);
    return scope && scope !== 'global' && scope !== 'legacy';
  });
  if (regional.length) {
    console.log(
      `refresh-pricing: priced at the regional rate — no cross-region offer: ${regional.join(', ')}`,
    );
  }
  const unmatched = ids.filter((id) => !matched.has(id));
  if (unmatched.length) {
    console.log(
      `refresh-pricing: ${unmatched.length} model(s) in the SSOT with no on-demand price in ` +
        `${MODEL_REGION} — retired, not offered here, or the id is wrong: ${unmatched.join(', ')}`,
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
