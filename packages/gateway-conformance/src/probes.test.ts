import type Anthropic from '@anthropic-ai/sdk';
import { describe, expect, it } from 'vitest';

import { PROBES, type Probe } from './probes.js';

/**
 * A scripted stand-in for the Anthropic client.
 *
 * The probes themselves cannot run without a live gateway, but what each one
 * *concludes* from a given response is the part worth pinning — a conformance
 * check that cannot fail is worth nothing, and every threshold below was
 * chosen because a looser one passed on a hop that was actually broken.
 */
interface Scripted {
  messages?: unknown[];
  betaMessages?: unknown[];
  streamDeltas?: number;
  toolRunnerFinal?: unknown;
  onToolRun?: () => void;
}

function fakeClient(script: Scripted): Anthropic {
  const queue = [...(script.messages ?? [])];
  const betaQueue = [...(script.betaMessages ?? [])];
  const next = (q: unknown[]) => {
    const v = q.shift();
    if (v === undefined) throw new Error('scripted client ran out of responses');
    return v;
  };
  return {
    messages: {
      create: async () => next(queue),
      stream: () => {
        const handlers: (() => void)[] = [];
        return {
          on: (_event: string, fn: () => void) => {
            handlers.push(fn);
          },
          finalMessage: async () => {
            for (let i = 0; i < (script.streamDeltas ?? 0); i++) {
              for (const h of handlers) h();
            }
            return { stop_reason: 'end_turn', content: [] };
          },
        };
      },
    },
    beta: {
      messages: {
        create: async () => next(betaQueue),
        toolRunner: (params: { tools: { run: (i: unknown) => Promise<unknown> }[] }) => ({
          runUntilDone: async () => {
            script.onToolRun?.();
            await params.tools[0]?.run({ city: 'San Diego' });
            return script.toolRunnerFinal;
          },
        }),
      },
    },
  } as unknown as Anthropic;
}

const probe = (name: string): Probe => {
  const p = PROBES.find((x) => x.name === name);
  if (!p) throw new Error(`no probe named ${name}`);
  return p;
};

const text = (t: string) => [{ type: 'text', text: t }];

describe('route-resolves', () => {
  it('passes when the gateway rewrites the route name to a real model', async () => {
    const client = fakeClient({
      messages: [{ model: 'claude-sonnet-4-6', stop_reason: 'end_turn', content: text('pong') }],
    });
    const r = await probe('route-resolves').run(client, 'primary');
    expect(r.outcome).toBe('pass');
    expect(r.detail).toContain('claude-sonnet-4-6');
  });

  it('fails when the route name is echoed back unrewritten', async () => {
    // An upstream that accepted the caller's string verbatim would answer
    // correctly while proving the gateway never resolved anything.
    const client = fakeClient({
      messages: [{ model: 'primary', stop_reason: 'end_turn', content: text('pong') }],
    });
    expect((await probe('route-resolves').run(client, 'primary')).outcome).toBe('fail');
  });
});

describe('prompt-cache', () => {
  const call = (write: number, read: number) => ({
    stop_reason: 'end_turn',
    content: text('ok'),
    usage: { cache_creation_input_tokens: write, cache_read_input_tokens: read },
  });

  it('passes when the second call reads the prefix back', async () => {
    const client = fakeClient({ messages: [call(2124, 0), call(0, 2124)] });
    const r = await probe('prompt-cache').run(client, 'primary');
    expect(r.outcome).toBe('pass');
    expect(r.detail).toContain('2124');
  });

  it('fails on writes with no read, which is all cost and no saving', async () => {
    // The failure this threshold exists for: a hop that forwards the
    // breakpoint but breaks prefix stability writes a fresh entry every call.
    // Asserting on the write alone calls that healthy.
    const client = fakeClient({ messages: [call(2124, 0), call(2124, 0)] });
    const r = await probe('prompt-cache').run(client, 'primary');
    expect(r.outcome).toBe('fail');
    expect(r.detail).toMatch(/full input price/);
  });

  it('fails when the breakpoint is dropped entirely', async () => {
    const client = fakeClient({ messages: [call(0, 0), call(0, 0)] });
    expect((await probe('prompt-cache').run(client, 'primary')).outcome).toBe('fail');
  });
});

describe('tool-use', () => {
  it('passes on a tool_use block with a matching stop reason', async () => {
    const client = fakeClient({
      messages: [
        {
          stop_reason: 'tool_use',
          content: [{ type: 'tool_use', id: 't1', name: 'get_weather', input: { city: 'SD' } }],
        },
      ],
    });
    expect((await probe('tool-use').run(client, 'primary')).outcome).toBe('pass');
  });

  it('fails when the tools array never reached the model', async () => {
    const client = fakeClient({
      messages: [{ stop_reason: 'end_turn', content: text('It is warm in San Diego.') }],
    });
    const r = await probe('tool-use').run(client, 'primary');
    expect(r.outcome).toBe('fail');
    expect(r.detail).toMatch(/no tool_use block/);
  });
});

describe('tool-result', () => {
  const firstTurn = {
    stop_reason: 'tool_use',
    content: [{ type: 'tool_use', id: 't1', name: 'get_weather', input: { city: 'SD' } }],
  };

  it('passes when the second turn answers from the tool result', async () => {
    const client = fakeClient({
      messages: [firstTurn, { stop_reason: 'end_turn', content: text('It is 72 degrees.') }],
    });
    expect((await probe('tool-result').run(client, 'primary')).outcome).toBe('pass');
  });

  it('fails without continuing the loop when the first turn has no tool call', async () => {
    const client = fakeClient({ messages: [{ stop_reason: 'end_turn', content: text('warm') }] });
    const r = await probe('tool-result').run(client, 'primary');
    expect(r.outcome).toBe('fail');
    expect(r.detail).toMatch(/cannot be continued/);
  });

  it('fails when the tool result is ignored by the model', async () => {
    const client = fakeClient({
      messages: [firstTurn, { stop_reason: 'end_turn', content: text('I cannot say.') }],
    });
    expect((await probe('tool-result').run(client, 'primary')).outcome).toBe('fail');
  });
});

describe('tool-runner', () => {
  it('passes when the loop executes the tool and uses its output', async () => {
    const client = fakeClient({
      toolRunnerFinal: { stop_reason: 'end_turn', content: text('It is 72 degrees.') },
    });
    const r = await probe('tool-runner').run(client, 'primary');
    expect(r.outcome).toBe('pass');
    expect(r.detail).toMatch(/executed 1x/);
  });

  it('fails when the loop finishes without the tool output reaching the answer', async () => {
    const client = fakeClient({
      toolRunnerFinal: { stop_reason: 'end_turn', content: text('I could not check.') },
    });
    expect((await probe('tool-runner').run(client, 'primary')).outcome).toBe('fail');
  });
});

describe('beta-header', () => {
  it('passes when a beta-flagged request is answered', async () => {
    const client = fakeClient({
      betaMessages: [{ stop_reason: 'end_turn', content: text('pong') }],
    });
    expect((await probe('beta-header').run(client, 'primary')).outcome).toBe('pass');
  });

  it('fails when the beta request comes back empty', async () => {
    const client = fakeClient({ betaMessages: [{ stop_reason: 'end_turn', content: [] }] });
    expect((await probe('beta-header').run(client, 'primary')).outcome).toBe('fail');
  });
});

describe('structured-output', () => {
  it('passes when the response parses to the declared schema', async () => {
    const client = fakeClient({
      messages: [
        {
          stop_reason: 'end_turn',
          content: text('{"city":"San Diego","temperature_f":72,"conditions":"sunny"}'),
        },
      ],
    });
    expect((await probe('structured-output').run(client, 'primary')).outcome).toBe('pass');
  });

  it('fails on prose, which is what a dropped output_config produces', async () => {
    const client = fakeClient({
      messages: [{ stop_reason: 'end_turn', content: text('San Diego is 72F and sunny.') }],
    });
    const r = await probe('structured-output').run(client, 'primary');
    expect(r.outcome).toBe('fail');
    expect(r.detail).toMatch(/not the declared schema/);
  });

  it('fails on valid JSON of the wrong shape', async () => {
    const client = fakeClient({
      messages: [{ stop_reason: 'end_turn', content: text('{"city":"San Diego"}') }],
    });
    expect((await probe('structured-output').run(client, 'primary')).outcome).toBe('fail');
  });
});

describe('streaming', () => {
  it('passes when a long completion arrives in many deltas', async () => {
    expect(
      (await probe('streaming').run(fakeClient({ streamDeltas: 20 }), 'primary')).outcome,
    ).toBe('pass');
  });

  it('fails on a handful of deltas, which is a buffering proxy replaying', async () => {
    // The reason the threshold is not >1: a hop that buffers the whole
    // response and replays it still emits a couple of deltas, so the obvious
    // assertion passes on a broken hop.
    const r = await probe('streaming').run(fakeClient({ streamDeltas: 2 }), 'primary');
    expect(r.outcome).toBe('fail');
    expect(r.detail).toMatch(/buffering rather than streaming/);
  });
});

describe('the probe set', () => {
  it('gives every probe a distinct name and a question', () => {
    const names = PROBES.map((p) => p.name);
    expect(new Set(names).size).toBe(names.length);
    for (const p of PROBES) expect(p.question.length).toBeGreaterThan(10);
  });
});
