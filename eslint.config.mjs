import js from '@eslint/js';
import tseslint from 'typescript-eslint';
import importX from 'eslint-plugin-import-x';
import security from 'eslint-plugin-security';
import prettier from 'eslint-config-prettier';

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
  js.configs.recommended,
  ...tseslint.configs.recommendedTypeChecked,
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
      '@typescript-eslint/no-unused-vars': ['error', { argsIgnorePattern: '^_', varsIgnorePattern: '^_' }],
      '@typescript-eslint/no-misused-promises': ['error', { checksVoidReturn: false }],
      'import-x/order': ['warn', { 'newlines-between': 'always', alphabetize: { order: 'asc' } }],
      'no-console': ['warn', { allow: ['warn', 'error'] }],
    },
  },
  prettier,
);
