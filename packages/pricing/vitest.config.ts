import { packageConfig } from '../../vitest.base';

export default packageConfig({
  thresholds: {
    lines: 95, // measured 100.00
    functions: 95, // measured 100.00
    branches: 95, // measured 100.00
    statements: 95, // measured 100.00
  },
});
