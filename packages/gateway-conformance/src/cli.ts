import { formatReport, runConformance } from './run.js';

export interface CliEnv {
  GATEWAY_ENDPOINT?: string;
  GATEWAY_ROUTE?: string;
  GATEWAY_TIMEOUT_MS?: string;
}

export interface CliPlan {
  action: 'run' | 'skip';
  /** Why, when skipping. Printed prominently — see the note below. */
  reason?: string;
  endpoint?: string;
  route?: string;
  timeoutMs?: number;
}

export const USAGE = `gateway-conformance — assert a ModelGateway route serves the contract it publishes

  GATEWAY_ENDPOINT   ModelGateway.status.endpoint (required)
  GATEWAY_ROUTE      a route name from spec.routes[].name (default: primary)
  GATEWAY_TIMEOUT_MS per-call ceiling in ms (default: 90000)

Read both off the cluster rather than assuming them:

  kubectl get modelgateway <name> -n eks-agent-platform \\
    -o jsonpath='{.status.endpoint}{"\\n"}{.status.routes}'
`;

/**
 * Decide what to do from the environment.
 *
 * No endpoint means skip, not fail. This needs a live cluster and a gateway
 * that can sign to Bedrock, and neither exists in the repo's own CI — a check
 * that goes red for its own preconditions is one people learn to ignore, and
 * then it is worth nothing on the day it catches something.
 *
 * The skip is loud on purpose. `optional: true` on the credentials Secret is
 * the same instinct applied without that discipline, and it hid a real failure
 * behind a healthy-looking pod. A skip has to say what went unchecked.
 */
export function plan(env: CliEnv): CliPlan {
  const endpoint = env.GATEWAY_ENDPOINT?.trim();
  if (!endpoint) {
    return {
      action: 'skip',
      reason:
        'GATEWAY_ENDPOINT is unset, so nothing was checked. The wire contract this ' +
        'asserts — prompt caching, tool use, beta headers, structured output, ' +
        'streaming — is UNVERIFIED for this change. Run it against a live gateway ' +
        'before trusting a route.',
    };
  }
  if (!/^https?:\/\//.test(endpoint)) {
    return {
      action: 'skip',
      reason: `GATEWAY_ENDPOINT is "${endpoint}", which is not an http(s) URL. Nothing was checked.`,
    };
  }
  const timeoutRaw = env.GATEWAY_TIMEOUT_MS?.trim();
  const parsed = timeoutRaw ? Number(timeoutRaw) : Number.NaN;
  // An unparseable or non-positive value leaves the property absent rather
  // than set to undefined, so the default applies instead of NaN reaching the
  // client as a timeout.
  const timeout = Number.isFinite(parsed) && parsed > 0 ? { timeoutMs: parsed } : {};
  return {
    action: 'run',
    endpoint,
    route: env.GATEWAY_ROUTE?.trim() || 'primary',
    ...timeout,
  };
}

/** Exit code: 0 pass or skip, 1 any probe failed. */
export async function main(env: CliEnv = process.env): Promise<number> {
  const decision = plan(env);
  if (decision.action === 'skip') {
    console.warn(`\nSKIPPED — ${decision.reason}\n`);
    console.warn(USAGE);
    return 0;
  }
  // Built up rather than spread with an explicit `undefined`:
  // exactOptionalPropertyTypes draws a distinction between an absent optional
  // property and one set to undefined, and only the former means "use the
  // default".
  const report = await runConformance({
    endpoint: decision.endpoint ?? '',
    route: decision.route ?? 'primary',
    ...(decision.timeoutMs === undefined ? {} : { timeoutMs: decision.timeoutMs }),
  });
  console.log(formatReport(report));
  return report.failed === 0 ? 0 : 1;
}
