import { packageConfig } from '../../vitest.base';

export default packageConfig({
  thresholds: {
    lines: 98, // measured 99.38
    functions: 98, // measured 100.00
    branches: 84, // measured 86.88
    statements: 92, // measured 96.84
  },
});
