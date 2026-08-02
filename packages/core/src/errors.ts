/**
 * Unified error taxonomy. Anything that calls a model classifies failures into
 * this set, so retry semantics can be reasoned about without knowing which
 * route answered, which wire format it spoke, or which model backed it.
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
