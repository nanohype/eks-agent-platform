import { packageConfig } from '../../vitest.base';

export default packageConfig({
  // Honest ratchets just below the measured actuals (see the sibling packages'
  // configs). All four sit at or above the org testing-rubric floor
  // (branches 60 / lines 75 / functions 75 / statements 75); the CLI's
  // process.exit / argv bootstrap in cli.ts is the only lightly-covered edge.
  thresholds: {
    lines: 96, // measured 98.91
    functions: 94, // measured 96.87
    branches: 90, // measured 92.89
    statements: 95, // measured 97.24
  },
  coverageExclude: [
    // The bundle entrypoint's argv/exit bootstrap is exercised end-to-end by
    // the Dockerized image round-trip, not the unit suite; main(argv) itself
    // is covered.
    'src/main.ts',
  ],
});
