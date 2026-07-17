import { packageConfig } from '../../vitest.base';

export default packageConfig({
  // The suite injects a fake CustomObjectsClient for the CRD call paths and a
  // fake KubeConfig for resolveApi's precedence branches (explicit path →
  // KUBECONFIG → in-cluster → default). The only uncovered remainder is the
  // production `new KubeConfig()` default-parameter construction, which needs a
  // real kubeconfig or in-cluster env. Floors sit just below the measured
  // actuals — comfortably above the org floor (lines/functions/statements 75,
  // branches 60) — so a regression fails the build without flaking.
  thresholds: {
    lines: 88, // measured 90.47
    functions: 83, // measured 85.71
    branches: 85, // measured 87.50
    statements: 88, // measured 90.90
  },
});
