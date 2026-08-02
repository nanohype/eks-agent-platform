# @eks-agent/gateway-conformance

Asserts that a `ModelGateway` route serves the wire contract it publishes.

Everything else in this repo checks what the operator *wrote*. The envtest
conformance suite asserts the reconciler produces an `AIGatewayRoute` of the
right shape; the schema-drift gate asserts the typed client matches the CRDs.
Neither sends a request. A gateway can reconcile perfectly, report `Ready`,
publish an endpoint, and refuse every call — the resource graph is identical
either way.

This is the check that sends traffic.

## What it asserts

| Probe               | The question                                                    |
| ------------------- | --------------------------------------------------------------- |
| `route-resolves`    | does the route name resolve to a model and answer                |
| `prompt-cache`      | does a `cache_control` breakpoint survive to the model           |
| `tool-use`          | does the model emit a `tool_use` block                           |
| `tool-result`       | does a `tool_result` turn come back answered                     |
| `tool-runner`       | does the SDK tool runner drive a full loop through the gateway   |
| `beta-header`       | does an explicit `anthropic-beta` header survive the hop         |
| `structured-output` | does `output_config` json_schema survive the hop                 |
| `streaming`         | does SSE arrive incrementally rather than as one buffered block  |

Three of the thresholds are deliberately stricter than the obvious version,
because the obvious version passes on a hop that is actually broken:

- **`route-resolves`** asserts the response's `model` differs from the route
  name. An upstream that accepted the caller's string verbatim would answer
  correctly while proving the gateway resolved nothing.
- **`prompt-cache`** asserts a cache *read* on a second call, not a write. A
  hop that forwards the breakpoint but breaks prefix stability writes a fresh
  entry every call — all cost, no saving — and a write-only assertion calls
  that healthy.
- **`streaming`** asserts the *spread* between the first and last delta, not
  the delta count and not time-to-first-delta. Count measures how the model
  chunks its output, which is not the gateway's doing — a 23-token reply
  legitimately arrives in three deltas, and a count threshold fails a healthy
  hop for it. Time-to-first-delta measures the model's time to first token,
  roughly a second here, which swamps everything on a short completion. Spread
  is the property that actually differs: a hop that buffers and replays
  delivers every delta in one burst, so its spread collapses toward zero
  however long the generation ran.

## Running it

```bash
GATEWAY_ENDPOINT=$(kubectl get modelgateway <name> -n eks-agent-platform \
  -o jsonpath='{.status.endpoint}') \
GATEWAY_ROUTE=primary \
  pnpm --filter @eks-agent/gateway-conformance conformance
```

Read the endpoint off the cluster rather than assuming it. `status.routes[]`
carries the resolved wire format and its base URL per route, which is the
contract this asserts — the endpoint alone is not a usable base for any client,
because the gateway serves each format under its own prefix.

From outside the cluster, port-forward the gateway Service first. The kx
workspace wraps all of this in `task stack:ai-platform:conformance`.

## It skips without a gateway, loudly

With no `GATEWAY_ENDPOINT` it exits 0 and prints what went unchecked. This
needs a live cluster and a gateway that can sign to Bedrock, and neither exists
in this repo's CI — a check that goes red for its own preconditions is one
people learn to ignore, and then it is worth nothing on the day it catches
something.

The skip names the contract it did not verify rather than passing quietly. A
silent skip is indistinguishable from a pass in a log, and this repo already
has one lesson on that: `optional: true` on a credentials Secret is the same
instinct applied without the discipline, and it hid a real failure behind a
pod that reported 3/3.

## Unit tests

The probes cannot run in CI, but what each one *concludes* from a given
response can. `probes.test.ts` drives every probe against a scripted client and
pins both verdicts — a conformance check that cannot fail is worth nothing, and
the negative cases are where the thresholds above are held.
