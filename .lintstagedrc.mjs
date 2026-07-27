export default {
  '*.{ts,tsx,js,jsx,mjs,cjs}': ['eslint --fix', 'prettier --write'],
  // Generated CRD / ref-docs surfaces are in .prettierignore; exclude them from
  // the globs too so a staged file under those trees cannot fight the
  // regenerate-and-diff gates (crd-docs, chart-crd-parity).
  '*.{json,md,yml,yaml}': (filenames) => {
    const filtered = filenames.filter(
      (f) =>
        !f.includes('operators/config/crd/') &&
        !f.includes('/crds/') &&
        !f.includes('docs/crd-reference/') &&
        !f.includes('zz_generated.'),
    );
    return filtered.length ? [`prettier --write ${filtered.map((f) => `"${f}"`).join(' ')}`] : [];
  },
  '*.tf': ['tofu fmt'],
};
