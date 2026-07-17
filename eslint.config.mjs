import tseslint from 'typescript-eslint';
import importX from 'eslint-plugin-import-x';
import security from 'eslint-plugin-security';
import prettier from 'eslint-config-prettier';
import base from './eslint.base.mjs';

export default tseslint.config(
  {
    ignores: [
      '**/dist/**',
      '**/node_modules/**',
      '**/.turbo/**',
      '**/generated/**',
      '**/*.tsbuildinfo',
      // Don't lint config files themselves — they pull in plugins whose
      // types aren't always strict enough for type-checked rules.
      'eslint.config.mjs',
      '*.config.mjs',
      '.lintstagedrc.mjs',
      'scripts/**',
    ],
  },
  // Org base — vendored byte-identical from nanohype library/config
  // (drift-gated by scripts/sync-vendored.mjs): @eslint/js recommended +
  // typescript-eslint strict + the shared rule options.
  ...base,
  // Type-checked layers on top of the strict base.
  ...tseslint.configs.recommendedTypeCheckedOnly,
  ...tseslint.configs.stylisticTypeChecked,
  security.configs.recommended,
  {
    languageOptions: {
      parserOptions: {
        projectService: {
          allowDefaultProject: [
            '*.config.mjs',
            '*.config.js',
            '*.config.cjs',
            '*.config.ts',
            'vitest.base.ts',
            'packages/*/vitest.config.ts',
            '.lintstagedrc.mjs',
            'scripts/*.mjs',
            'scripts/*.js',
          ],
        },
        tsconfigRootDir: import.meta.dirname,
      },
    },
    plugins: { 'import-x': importX },
    rules: {
      '@typescript-eslint/consistent-type-imports': [
        'error',
        { prefer: 'type-imports', fixStyle: 'inline-type-imports' },
      ],
      '@typescript-eslint/no-misused-promises': ['error', { checksVoidReturn: false }],
      'import-x/order': ['error', { 'newlines-between': 'always', alphabetize: { order: 'asc' } }],
      'no-console': ['error', { allow: ['warn', 'error'] }],
    },
  },
  prettier,
);
