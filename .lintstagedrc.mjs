export default {
  // Biome owns format + lint for TS/JS. Generated CRD / ref-docs surfaces are
  // excluded via biome.json includes so a staged file under those trees cannot
  // fight the regenerate-and-diff gates (crd-docs, chart-crd-parity).
  '*.{ts,tsx,js,jsx,mjs,cjs}': ['biome check --write --no-errors-on-unmatched'],
  '*.{json,md}': (filenames) => {
    const filtered = filenames.filter(
      (f) =>
        !f.includes('operators/config/crd/') &&
        !f.includes('/crds/') &&
        !f.includes('docs/crd-reference/') &&
        !f.includes('zz_generated.'),
    );
    return filtered.length
      ? [`biome format --write --no-errors-on-unmatched ${filtered.map((f) => `"${f}"`).join(' ')}`]
      : [];
  },
  // YAML is out of Biome's formatter scope. Generated YAML stays unformatted by
  // design; hand-written YAML is left to author judgment / CI render checks.
  '*.tf': ['tofu fmt'],
};
