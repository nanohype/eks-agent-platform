import type Anthropic from '@anthropic-ai/sdk';

/**
 * What a probe found. `detail` is the load-bearing field: a probe that only
 * says pass/fail tells you a route is broken, and every one of these failures
 * is diagnosable only from what the gateway did *instead* — a 404 naming the
 * model, a 500 from a signing container, zero cache tokens on a call that
 * should have read a prefix back.
 */
export interface ProbeResult {
  name: string;
  /** The question, in the form a reader can check the answer against. */
  question: string;
  outcome: 'pass' | 'fail';
  detail: string;
}

export interface Probe {
  name: string;
  question: string;
  run(client: Anthropic, route: string): Promise<Omit<ProbeResult, 'name' | 'question'>>;
}

const MAX_TOKENS = 256;

/** Join every text block of a response. */
function textOf(content: readonly { type: string; text?: string }[] | undefined): string {
  return (content ?? [])
    .filter((b) => b.type === 'text')
    .map((b) => b.text ?? '')
    .join('');
}

/**
 * Prompt caching has a hard minimum (1024 tokens on Sonnet-class models). A
 * short prefix reports zero cache tokens whether or not the breakpoint
 * survived the hop, so a cache probe built on one passes vacuously. This
 * crosses the floor.
 */
const CACHEABLE_PREFIX = `You are a precise assistant used in a gateway conformance probe.
${'Filler, repeated verbatim so the prefix crosses the prompt-cache minimum. '.repeat(150)}
Answer exactly and briefly.`;

const WEATHER_TOOL = {
  name: 'get_weather',
  description: 'Get the current temperature for a city.',
  input_schema: {
    type: 'object' as const,
    properties: { city: { type: 'string', description: 'City name' } },
    required: ['city'],
  },
};

const TOOL_PROMPT = 'What is the temperature in San Diego? Use the tool.';

export const PROBES: Probe[] = [
  {
    name: 'route-resolves',
    question: 'does the route name resolve to a model and answer',
    async run(client, route) {
      const res = await client.messages.create({
        model: route,
        max_tokens: 32,
        messages: [{ role: 'user', content: 'Reply with exactly: pong' }],
      });
      const text = textOf(res.content);
      // res.model is the *resolved* model. Asserting it differs from the route
      // name is what proves the gateway rewrote it rather than passing the
      // caller's string through to an upstream that happened to accept it.
      const rewritten = res.model !== route;
      return {
        outcome: text.toLowerCase().includes('pong') && rewritten ? 'pass' : 'fail',
        detail: `route "${route}" resolved to ${res.model}, stop=${res.stop_reason}, text=${JSON.stringify(text.slice(0, 40))}`,
      };
    },
  },
  {
    name: 'prompt-cache',
    question: 'does a cache_control breakpoint survive to the model',
    async run(client, route) {
      const send = () =>
        client.messages.create({
          model: route,
          max_tokens: 32,
          system: [{ type: 'text', text: CACHEABLE_PREFIX, cache_control: { type: 'ephemeral' } }],
          messages: [{ role: 'user', content: 'Reply with exactly: ok' }],
        });
      const first = await send();
      const second = await send();
      const wrote = first.usage?.cache_creation_input_tokens ?? 0;
      const read = second.usage?.cache_read_input_tokens ?? 0;
      // The assertion is on the READ, not the write. A gateway that forwards
      // the breakpoint but breaks prefix stability writes a new entry every
      // call and never reads one back — all cost, no saving, and a
      // write-only assertion calls that healthy.
      return {
        outcome: read > 0 ? 'pass' : 'fail',
        detail:
          read > 0
            ? `wrote ${wrote} token(s), read ${read} back on the second call`
            : `read 0 cache tokens back (wrote ${wrote}) — the breakpoint is dropped or the prefix is not stable, so every call pays full input price`,
      };
    },
  },
  {
    name: 'tool-use',
    question: 'does the model emit a tool_use block',
    async run(client, route) {
      const res = await client.messages.create({
        model: route,
        max_tokens: MAX_TOKENS,
        tools: [WEATHER_TOOL],
        messages: [{ role: 'user', content: TOOL_PROMPT }],
      });
      const call = res.content.find((b) => b.type === 'tool_use');
      return {
        outcome: call && res.stop_reason === 'tool_use' ? 'pass' : 'fail',
        detail: call
          ? `stop=${res.stop_reason}, called ${call.name} with ${JSON.stringify(call.input)}`
          : `stop=${res.stop_reason}, no tool_use block — the tools array did not reach the model`,
      };
    },
  },
  {
    name: 'tool-result',
    question: 'does a tool_result turn come back answered',
    async run(client, route) {
      const first = await client.messages.create({
        model: route,
        max_tokens: MAX_TOKENS,
        tools: [WEATHER_TOOL],
        messages: [{ role: 'user', content: TOOL_PROMPT }],
      });
      const call = first.content.find((b) => b.type === 'tool_use');
      if (!call) {
        return {
          outcome: 'fail',
          detail: 'no tool_use block on the first turn, so the loop cannot be continued',
        };
      }
      const second = await client.messages.create({
        model: route,
        max_tokens: MAX_TOKENS,
        tools: [WEATHER_TOOL],
        messages: [
          { role: 'user', content: TOOL_PROMPT },
          { role: 'assistant', content: first.content },
          {
            role: 'user',
            content: [
              { type: 'tool_result', tool_use_id: call.id, content: '72 degrees Fahrenheit' },
            ],
          },
        ],
      });
      const text = textOf(second.content);
      return {
        outcome: text.includes('72') ? 'pass' : 'fail',
        detail: `stop=${second.stop_reason}, text=${JSON.stringify(text.slice(0, 80))}`,
      };
    },
  },
  {
    name: 'tool-runner',
    question: 'does the SDK tool runner drive a full loop through the gateway',
    async run(client, route) {
      let invoked = 0;
      const runner = client.beta.messages.toolRunner({
        model: route,
        max_tokens: 512,
        messages: [{ role: 'user', content: TOOL_PROMPT }],
        tools: [
          {
            ...WEATHER_TOOL,
            run: async ({ city }: { city: string }) => {
              invoked += 1;
              return `72 degrees Fahrenheit in ${city}`;
            },
          },
        ],
      });
      // runUntilDone, not done(): done() settles only the message in flight,
      // so on a turn that asked for a tool it never resolves.
      const final = await runner.runUntilDone();
      const text = textOf(final?.content);
      return {
        outcome: invoked > 0 && text.includes('72') ? 'pass' : 'fail',
        detail: `tool executed ${invoked}x, stop=${final?.stop_reason}, text=${JSON.stringify(text.slice(0, 80))}`,
      };
    },
  },
  {
    name: 'beta-header',
    question: 'does an explicit anthropic-beta header survive the hop',
    async run(client, route) {
      const res = await client.beta.messages.create({
        model: route,
        max_tokens: 32,
        messages: [{ role: 'user', content: 'Reply with exactly: pong' }],
        betas: ['context-management-2025-06-27'],
      });
      const text = textOf(res.content);
      return {
        outcome: text.toLowerCase().includes('pong') ? 'pass' : 'fail',
        detail: `stop=${res.stop_reason}, text=${JSON.stringify(text.slice(0, 40))}`,
      };
    },
  },
  {
    name: 'structured-output',
    question: 'does output_config json_schema survive the hop',
    async run(client, route) {
      const res = await client.messages.create({
        model: route,
        max_tokens: MAX_TOKENS,
        messages: [{ role: 'user', content: 'San Diego is 72F and sunny. Report it.' }],
        output_config: {
          format: {
            type: 'json_schema',
            schema: {
              type: 'object',
              properties: {
                city: { type: 'string' },
                temperature_f: { type: 'number' },
                conditions: { type: 'string' },
              },
              required: ['city', 'temperature_f', 'conditions'],
              additionalProperties: false,
            },
          },
        },
      });
      const text = textOf(res.content);
      let parsed: { temperature_f?: unknown } | null = null;
      try {
        parsed = JSON.parse(text);
      } catch {
        parsed = null;
      }
      return {
        outcome: parsed !== null && typeof parsed.temperature_f === 'number' ? 'pass' : 'fail',
        detail: parsed
          ? `parsed ${JSON.stringify(parsed)}`
          : `response is not the declared schema: ${JSON.stringify(text.slice(0, 80))}`,
      };
    },
  },
  {
    name: 'streaming',
    question: 'does SSE arrive incrementally rather than as one buffered block',
    async run(client, route) {
      let deltas = 0;
      let firstAt: number | null = null;
      let lastAt = 0;
      const started = Date.now();
      const stream = client.messages.stream({
        model: route,
        max_tokens: 600,
        messages: [{ role: 'user', content: 'Write 40 short numbered lines about the ocean.' }],
      });
      stream.on('text', () => {
        deltas += 1;
        lastAt = Date.now();
        firstAt ??= lastAt;
      });
      const final = await stream.finalMessage();

      // The assertion is the SPREAD between the first and last delta, not the
      // delta count and not time-to-first-delta.
      //
      // Count measures how the model chunks its output, which is not the
      // gateway's doing — a short answer legitimately arrives in three deltas,
      // so a count threshold fails a healthy hop. Time-to-first-delta measures
      // the model's time to first token, which is ~1s here and swamps
      // everything on a short completion.
      //
      // Spread is the property that actually differs: a hop that buffers the
      // whole response and replays it delivers every delta in one burst, so
      // its spread collapses toward zero no matter how long the generation
      // ran or how many chunks it was cut into.
      const spread = firstAt === null ? 0 : lastAt - firstAt;
      const SPREAD_AT_LEAST_MS = 250;
      const incremental = deltas > 1 && spread >= SPREAD_AT_LEAST_MS;
      return {
        outcome: incremental ? 'pass' : 'fail',
        detail: incremental
          ? `${deltas} deltas spread over ${spread}ms (first at ${(firstAt ?? 0) - started}ms of ${Date.now() - started}ms), stop=${final.stop_reason}`
          : `${deltas} delta(s) arrived within ${spread}ms of each other — the hop is buffering the response and replaying it, not streaming`,
      };
    },
  },
];
