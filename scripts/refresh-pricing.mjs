#!/usr/bin/env node
/**
 * Refresh Bedrock pricing in packages/pricing/src/index.ts.
 *
 * Intent: keep PRICES current without hand-editing values from memory.
 * Bedrock pricing changes regularly (new models, new tiers, occasional drops)
 * and outdated numbers silently undercount spend — a correctness issue for
 * BudgetPolicy.status.percentOfBudget math and downstream kill-switch
 * decisions.
 *
 * Wiring (Phase 2): a GitHub Actions workflow on weekly cron runs this
 * script against the AWS Pricing API (pricing.us-east-1.amazonaws.com,
 * service AmazonBedrock), diffs the result against the current PRICES
 * table, and opens a PR with the diff. Renovate cannot do this on its own
 * — Renovate watches package-manager manifests (package.json, go.mod,
 * helm chartVersion), not file contents in arbitrary source files.
 *
 * Status: Phase-1 scaffold. Body is intentionally unimplemented; calling
 * this exits non-zero so a stray cron invocation fails loudly rather
 * than masquerading as a successful refresh.
 */

// eslint-disable-next-line no-console
console.error(
  [
    'refresh-pricing: Phase-1 scaffold — wire to AWS Pricing API in Phase 2.',
    'Until then, edit packages/pricing/src/index.ts manually using the',
    'Bedrock pricing page footer for last-modified dates.',
    '',
    'Exiting non-zero so CI cron invocations fail visibly rather than',
    'silently report success on a no-op refresh.',
  ].join('\n'),
);
process.exit(2);
