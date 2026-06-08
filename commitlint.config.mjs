/** @type {import('@commitlint/types').UserConfig} */
export default {
  extends: ['@commitlint/config-conventional'],
  rules: {
    'type-enum': [
      2,
      'always',
      ['feat', 'fix', 'docs', 'style', 'refactor', 'perf', 'test', 'build', 'ci', 'chore', 'revert', 'harden'],
    ],
    'scope-enum': [
      2,
      'always',
      [
        'operators',
        'charts',
        'terraform',
        'core',
        'sdk',
        'pricing',
        'client',
        'cli',
        'examples',
        'docs',
        'ci',
        'release',
        'deps',
        'security',
      ],
    ],
    // Allow capitalized Kubernetes Kind names + acronyms in subjects without
    // forcing all-lowercase. Still forbid pascal-case + all-upper subjects.
    'subject-case': [2, 'never', ['pascal-case', 'upper-case', 'start-case']],
    'header-max-length': [2, 'always', 100],
    'body-max-line-length': [0],
    'footer-max-line-length': [0],
  },
};
