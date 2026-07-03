import { defineConfig } from 'vitest/config';

interface PackageTestOptions {
  /**
   * Coverage floors for the package. Honest ratchets: floors sit just below
   * the measured actuals (each carries a `// measured` comment at the
   * call-site) so the gate catches a regression — a new untested module
   * dragging the denominator down — without flaking on minor fluctuation.
   */
  thresholds: {
    lines: number;
    functions: number;
    branches: number;
    statements: number;
  };
  /** Extra coverage excludes on top of the shared defaults (e.g. bin entry points). */
  coverageExclude?: string[];
}

/**
 * Shared vitest baseline for the workspace packages — the vitest counterpart
 * to tsconfig.base.json. Each package's vitest.config.ts calls this with its
 * own measured coverage thresholds. Run per package via `pnpm test:coverage`,
 * or across the workspace via `turbo run test:coverage`.
 */
export function packageConfig({ thresholds, coverageExclude = [] }: PackageTestOptions) {
  return defineConfig({
    test: {
      environment: 'node',
      include: ['src/**/*.test.ts'],
      coverage: {
        provider: 'v8',
        reporter: ['text'],
        // Explicit include so modules with zero tests still count against the
        // floor — the gate measures the whole src/ surface, not just what the
        // suite happened to load.
        include: ['src/**/*.ts'],
        exclude: ['src/**/*.test.ts', ...coverageExclude],
        thresholds,
      },
    },
  });
}
