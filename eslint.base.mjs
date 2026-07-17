/**
 * ESLint base for this repo's TypeScript packages — flat config on the
 * typescript-eslint `strict` ruleset.
 *
 * Repo-specific plugins, ignores, and type-checked rules layer on top in the
 * thin `eslint.config.mjs`.
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
