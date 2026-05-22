# sandbox-worker

Container image for the Managed Agents **self-hosted sandbox** worker.

It runs Anthropic's `ant beta:worker` process, which claims sessions from a
`self_hosted` environment's work queue and executes the agent's tool calls
(`bash`, `read`, `write`, `edit`, `glob`, `grep`) inside the container — agent
code and its filesystem stay inside the cluster.

`ANTHROPIC_ENVIRONMENT_ID` and `ANTHROPIC_ENVIRONMENT_KEY` are supplied at
runtime by the workload that schedules the worker, not baked into the image.

Published to `ghcr.io/nanohype/eks-agent-platform/sandbox-worker` on
`sandbox-worker-v*` tags — see [`.github/workflows/release.yaml`](../.github/workflows/release.yaml).
