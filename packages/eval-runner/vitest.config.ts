import { packageConfig } from '../../vitest.base';

export default packageConfig({
  // Ratchets just below this suite's coverage, all four at or above the org
  // testing-rubric floor (branches 60 / lines 75 / functions 75 /
  // statements 75). The coverage report prints the actuals; a drop under any
  // threshold fails `pnpm test`.
  thresholds: {
    lines: 96,
    functions: 94,
    branches: 90,
    statements: 95,
  },
  coverageExclude: [
    // The bundle entrypoint's argv → exit-code bootstrap; run() itself is
    // covered. The `eval-runner-image` job in .github/workflows/ci.yaml
    // exercises it end to end by running `score` inside the built image.
    'src/main.ts',
  ],
});
