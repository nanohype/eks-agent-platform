/**
 * Unified error taxonomy. Every ProviderAdapter classifies errors into this
 * set so the runtime can reason about retry semantics independent of the
 * underlying SDK.
 */

export type ErrorClass =
  | 'RateLimit'
  | 'Overloaded'
  | 'BadRequest'
  | 'Server'
  | 'Network'
  | 'AuthFailure'
  | 'GuardrailBlock'
  | 'BudgetExceeded'
  | 'ContextLengthExceeded'
  | 'Cancelled';

export class AgentError extends Error {
  readonly class: ErrorClass;
  readonly retryable: boolean;
  readonly correlationId?: string;

  constructor(args: {
    class: ErrorClass;
    message: string;
    cause?: unknown;
    correlationId?: string;
  }) {
    super(args.message, args.cause ? { cause: args.cause } : undefined);
    this.name = 'AgentError';
    this.class = args.class;
    if (args.correlationId !== undefined) this.correlationId = args.correlationId;
    this.retryable = RETRYABLE.has(args.class);
  }
}

const RETRYABLE: ReadonlySet<ErrorClass> = new Set<ErrorClass>([
  'RateLimit',
  'Overloaded',
  'Server',
  'Network',
]);

export function isRetryable(err: unknown): boolean {
  return err instanceof AgentError && err.retryable;
}
