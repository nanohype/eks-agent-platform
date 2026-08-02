import { describe, expect, it } from 'vitest';

import { main, plan, USAGE } from './cli.js';
import { anthropicBaseURL, describeError, formatReport, runConformance } from './run.js';

describe('plan', () => {
  it('skips rather than fails when no gateway is configured', () => {
    // The whole point: this needs a live cluster and a gateway that can sign to
    // Bedrock, neither of which exists in this repo's CI. Failing there would
    // train people to ignore it.
    expect(plan({}).action).toBe('skip');
  });

  it('names what went unchecked when it skips', () => {
    // A quiet skip is indistinguishable from a pass in a log, which is how a
    // check stops being worth anything. The reason has to say the contract is
    // unverified, not just that a variable was missing.
    const reason = plan({}).reason ?? '';
    expect(reason).toMatch(/UNVERIFIED/);
    expect(reason).toMatch(/GATEWAY_ENDPOINT/);
  });

  it('skips on an endpoint that is not an http(s) URL', () => {
    // `host:8080` parses as a URL whose protocol is `host:`. Caught here rather
    // than as an opaque client error on the first call.
    const decision = plan({ GATEWAY_ENDPOINT: 'blank-gateway.tenants-blank.svc:8080' });
    expect(decision.action).toBe('skip');
    expect(decision.reason).toMatch(/not an http\(s\) URL/);
  });

  it('runs with the endpoint given, defaulting the route', () => {
    const decision = plan({ GATEWAY_ENDPOINT: 'http://blank-gateway.tenants-blank.svc:8080' });
    expect(decision).toMatchObject({ action: 'run', route: 'primary' });
  });

  it('takes the route name as given', () => {
    const decision = plan({
      GATEWAY_ENDPOINT: 'http://gw.svc:8080',
      GATEWAY_ROUTE: 'embeddings',
    });
    expect(decision.route).toBe('embeddings');
  });

  it('ignores an unparseable timeout instead of sending NaN to the client', () => {
    const decision = plan({ GATEWAY_ENDPOINT: 'http://gw.svc:8080', GATEWAY_TIMEOUT_MS: 'soon' });
    expect(decision.timeoutMs).toBeUndefined();
  });

  it('documents both variables it reads', () => {
    expect(USAGE).toMatch(/GATEWAY_ENDPOINT/);
    expect(USAGE).toMatch(/GATEWAY_ROUTE/);
  });
});

describe('anthropicBaseURL', () => {
  it('appends the Anthropic prefix the gateway serves that format under', () => {
    // status.endpoint is not a usable base for any client on its own — each
    // wire format lives under its own prefix.
    expect(anthropicBaseURL('http://gw.tenants-blank.svc:8080')).toBe(
      'http://gw.tenants-blank.svc:8080/anthropic',
    );
  });

  it('does not double the separator on a trailing slash', () => {
    expect(anthropicBaseURL('http://gw.svc:8080/')).toBe('http://gw.svc:8080/anthropic');
  });

  it('trims a run of trailing slashes in linear time', () => {
    // The scan replaces /\\/+$/, which backtracks polynomially on exactly this
    // input. The endpoint comes from an operator-published status field or an
    // environment variable, so it is not a value this code chooses.
    const started = Date.now();
    expect(anthropicBaseURL(`http://gw.svc:8080${'/'.repeat(50_000)}`)).toBe(
      'http://gw.svc:8080/anthropic',
    );
    expect(Date.now() - started).toBeLessThan(1000);
  });

  it('handles an endpoint that is nothing but slashes', () => {
    expect(anthropicBaseURL('///')).toBe('/anthropic');
  });
});

describe('describeError', () => {
  it('surfaces the status, which is what separates the failure modes', () => {
    // 404 = the route never matched (no model header derived);
    // 500 = the upstream refused, usually credentials.
    expect(describeError({ status: 404, message: 'No matching route found' })).toMatch(/HTTP 404/);
    expect(describeError({ status: 500, message: 'cannot retrieve AWS credentials' })).toMatch(
      /HTTP 500/,
    );
  });

  it('says so when nothing answered at all', () => {
    expect(describeError(new Error('connect ECONNREFUSED'))).toMatch(/no response/);
  });
});

describe('formatReport', () => {
  const report = {
    baseURL: 'http://gw.svc:8080/anthropic',
    route: 'primary',
    results: [
      { name: 'route-resolves', question: 'q1', outcome: 'pass' as const, detail: 'resolved to X' },
      { name: 'prompt-cache', question: 'q2', outcome: 'fail' as const, detail: 'read 0 back' },
    ],
    failed: 1,
  };

  it('prints every probe detail, not just the verdict', () => {
    // The detail is the diagnosable part — a bare pass/fail says a route is
    // broken without saying which layer refused.
    const out = formatReport(report);
    expect(out).toContain('resolved to X');
    expect(out).toContain('read 0 back');
  });

  it('reports the failure count against the total', () => {
    expect(formatReport(report)).toMatch(/1 of 2 probes failed/);
  });

  it('says all passed when none failed', () => {
    expect(formatReport({ ...report, results: [report.results[0]!], failed: 0 })).toMatch(
      /all 1 probes passed/,
    );
  });
});

describe('runConformance', () => {
  const endpoint = 'http://gw.tenants-blank.svc:8080';

  it('records every probe against the derived base URL', async () => {
    const report = await runConformance({
      endpoint,
      route: 'primary',
      probes: [
        {
          name: 'a',
          question: 'does a hold',
          run: async () => ({ outcome: 'pass' as const, detail: 'yes' }),
        },
        {
          name: 'b',
          question: 'does b hold',
          run: async () => ({ outcome: 'fail' as const, detail: 'no' }),
        },
      ],
    });
    expect(report.baseURL).toBe(`${endpoint}/anthropic`);
    expect(report.failed).toBe(1);
    expect(report.results.map((r) => r.name)).toEqual(['a', 'b']);
  });

  it('turns a thrown probe into a failure naming the layer that refused', async () => {
    // A probe that throws is the common case in practice — a 404 from the
    // route, a 500 from the signing container. It has to become a reported
    // failure rather than aborting the run and losing every later probe.
    const report = await runConformance({
      endpoint,
      route: 'primary',
      probes: [
        {
          name: 'boom',
          question: 'does the route answer',
          run: async () => {
            throw Object.assign(new Error('No matching route found'), { status: 404 });
          },
        },
        {
          name: 'after',
          question: 'does the run continue',
          run: async () => ({ outcome: 'pass' as const, detail: 'reached' }),
        },
      ],
    });
    expect(report.results[0]?.detail).toMatch(/HTTP 404/);
    expect(report.results[1]?.outcome).toBe('pass');
    expect(report.failed).toBe(1);
  });

  it('honours an explicit per-call timeout', async () => {
    const report = await runConformance({
      endpoint,
      route: 'primary',
      timeoutMs: 1000,
      probes: [
        { name: 'x', question: 'x holds', run: async () => ({ outcome: 'pass', detail: 'ok' }) },
      ],
    });
    expect(report.failed).toBe(0);
  });
});

describe('main', () => {
  it('exits 0 and says what went unchecked when no gateway is configured', async () => {
    const warned: string[] = [];
    const original = console.warn;
    console.warn = (msg?: unknown) => {
      warned.push(String(msg));
    };
    try {
      // Zero, not one: a precondition this repo's CI cannot meet is not a
      // defect in the change under test.
      expect(await main({})).toBe(0);
      expect(warned.join('\n')).toMatch(/SKIPPED/);
      expect(warned.join('\n')).toMatch(/UNVERIFIED/);
    } finally {
      console.warn = original;
    }
  });
});

describe('defensive paths', () => {
  it('treats a response with no usage block as an uncached call', async () => {
    // Not hypothetical: a translating hop that drops the cache fields entirely
    // returns a valid message with no usage. Reading it as a hit would be the
    // worst possible answer.
    const { PROBES } = await import('./probes.js');
    const p = PROBES.find((x) => x.name === 'prompt-cache');
    const client = {
      messages: { create: async () => ({ stop_reason: 'end_turn', content: [] }) },
    } as never;
    expect((await p?.run(client, 'primary'))?.outcome).toBe('fail');
  });

  it('fails rather than throwing when the tool runner returns nothing', async () => {
    const { PROBES } = await import('./probes.js');
    const p = PROBES.find((x) => x.name === 'tool-runner');
    const client = {
      beta: {
        messages: {
          toolRunner: (params: { tools: { run: (i: unknown) => Promise<unknown> }[] }) => ({
            runUntilDone: async () => {
              await params.tools[0]?.run({ city: 'SD' });
              return undefined;
            },
          }),
        },
      },
    } as never;
    expect((await p?.run(client, 'primary'))?.outcome).toBe('fail');
  });

  it('renders an error whose body is a structured object', () => {
    expect(describeError({ status: 400, error: { type: 'invalid_request_error' } })).toMatch(
      /invalid_request_error/,
    );
  });

  it('falls back to the default route when the variable is empty', () => {
    expect(plan({ GATEWAY_ENDPOINT: 'http://gw.svc:8080', GATEWAY_ROUTE: '  ' }).route).toBe(
      'primary',
    );
  });

  it('ignores a non-positive timeout', () => {
    expect(
      plan({ GATEWAY_ENDPOINT: 'http://gw.svc:8080', GATEWAY_TIMEOUT_MS: '0' }).timeoutMs,
    ).toBeUndefined();
  });

  it('skips on a whitespace-only endpoint', () => {
    expect(plan({ GATEWAY_ENDPOINT: '   ' }).action).toBe('skip');
  });
});

describe('main against an unreachable gateway', () => {
  it('exits 1 and reports every probe rather than aborting on the first', async () => {
    // Port 1 refuses immediately, so this exercises the whole run path at
    // connect speed. It is also the real first-contact failure: an endpoint
    // published by a ModelGateway whose data plane never programmed.
    const logged: string[] = [];
    const original = console.log;
    console.log = (msg?: unknown) => {
      logged.push(String(msg));
    };
    try {
      const code = await main({
        GATEWAY_ENDPOINT: 'http://127.0.0.1:1',
        GATEWAY_TIMEOUT_MS: '1500',
      });
      expect(code).toBe(1);
      const out = logged.join('\n');
      expect(out).toMatch(/probes failed/);
      // Every probe reported, not just the first to throw.
      expect(out).toMatch(/route-resolves/);
      expect(out).toMatch(/streaming/);
    } finally {
      console.log = original;
    }
  }, 60_000);
});

describe('describeError shapes', () => {
  it('handles an error carrying neither status nor message', () => {
    expect(describeError({})).toMatch(/no response/);
  });

  it('truncates a very long body rather than flooding the report', () => {
    expect(describeError({ status: 500, message: 'x'.repeat(1000) }).length).toBeLessThanOrEqual(
      300,
    );
  });
});
