/**
 * Org-canonical ESLint base — flat config on the typescript-eslint `strict`
 * ruleset. Canonical source: nanohype `library/config/eslint.base.mjs`.
 *
 * Consumers carry a byte-identical copy (drift-gated by their
 * `scripts/sync-vendored.mjs` where the repo vendors from a nanohype
 * checkout) or extend this file by relative path inside the nanohype repo
 * itself. Fixes land here, then re-sync — never patch a copy.
 *
 * Repo-specific plugins, ignores, and rules layer on top in each repo's thin
 * `eslint.config.*`.
 */
import eslint from '@eslint/js';
import tseslint from 'typescript-eslint';

export default tseslint.config(
  eslint.configs.recommended,
  ...tseslint.configs.strict,
  {
    rules: {
      '@typescript-eslint/no-unused-vars': [
        'error',
        { argsIgnorePattern: '^_', destructuredArrayIgnorePattern: '^_', varsIgnorePattern: '^_' },
      ],
      '@typescript-eslint/no-non-null-assertion': 'off',
    },
  },
  {
    ignores: ['**/dist/', '**/coverage/', 'eslint.config.*', 'eslint.base.mjs'],
  },
);
