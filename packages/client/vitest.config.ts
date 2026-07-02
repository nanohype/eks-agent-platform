import { packageConfig } from '../../vitest.base';

export default packageConfig({
  // The suite injects a fake CustomObjectsClient and covers the CRD call
  // paths; the uncovered remainder is the kubeconfig-resolution constructor
  // branch (loadFromFile / loadFromCluster / loadFromDefault — needs a real
  // kubeconfig or in-cluster env). With a 25-statement denominator that
  // lands the package below the org's 70-75% ideal, so the ratchet is set
  // where the suite actually is — raise it as the resolution branch gains
  // injectable seams.
  thresholds: {
    lines: 48, // measured 50.00
    functions: 81, // measured 83.33
    branches: 31, // measured 33.33
    statements: 46, // measured 48.00
  },
});
