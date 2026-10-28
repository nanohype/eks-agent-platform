// Bundle entrypoint for the eval-runner image. The Argo WorkflowTemplate
// invokes `evaluate …` and `score …`, which are thin wrapper scripts that
// exec `node /app/cli.js <subcommand> …`. All logic lives in run()/runEvaluate/
// runScore (unit-tested); this file is only the argv → exit-code bootstrap, so
// it is excluded from unit coverage. The `eval-runner-image` job in
// .github/workflows/ci.yaml exercises it instead: it runs `score` inside the
// built image on each release platform and compares the score.json it writes
// against testdata/score.golden.json.
import { run } from './cli.js';

run(process.argv.slice(2))
  .then((code) => process.exit(code))
  .catch((err: unknown) => {
    console.error(err);
    process.exit(1);
  });
