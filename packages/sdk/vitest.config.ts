import { packageConfig } from '../../vitest.base';

export default packageConfig({
  thresholds: {
    lines: 98, // measured 100.00
    functions: 98, // measured 100.00
    branches: 84, // measured 86.45
    statements: 92, // measured 94.93
  },
});
