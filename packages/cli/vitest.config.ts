import { packageConfig } from '../../vitest.base';

export default packageConfig({
  coverageExclude: [
    // CLI entry point — commander wiring + process entry, exercised
    // end-to-end by running the binary, not unit-testable in isolation.
    'src/cli.ts',
  ],
  thresholds: {
    lines: 95, // measured 100.00
    functions: 95, // measured 100.00
    branches: 70, // measured 75.00
    statements: 95, // measured 100.00
  },
});
