// Bundle entrypoint for the eval-runner image. The Argo WorkflowTemplate
// invokes `evaluate …` and `score …`, which are thin wrapper scripts that
// exec `node /app/cli.js <subcommand> …`. All logic lives in run()/runEvaluate/
// runScore (unit-tested); this file is only the argv → exit-code bootstrap and
// is verified by the image round-trip, so it is excluded from unit coverage.
import { run } from './cli.js';

run(process.argv.slice(2))
  .then((code) => process.exit(code))
  .catch((err: unknown) => {
    console.error(err);
    process.exit(1);
  });
