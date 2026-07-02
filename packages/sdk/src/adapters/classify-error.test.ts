import {
  AccessDeniedException,
  InternalServerException,
  ResourceNotFoundException,
  ServiceQuotaExceededException,
  ServiceUnavailableException,
  ThrottlingException,
  ValidationException,
} from '@aws-sdk/client-bedrock-runtime';
import { describe, expect, it } from 'vitest';

import { AnthropicBedrockAdapter } from './anthropic.js';

const adapter = new AnthropicBedrockAdapter({ region: 'us-west-2' });

describe('BedrockAdapter.classifyError', () => {
  it('classifies ThrottlingException as RateLimit', () => {
    expect(
      adapter.classifyError(new ThrottlingException({ $metadata: {}, message: 'slow down' })),
    ).toBe('RateLimit');
  });

  it('classifies ServiceQuotaExceededException as RateLimit', () => {
    expect(
      adapter.classifyError(
        new ServiceQuotaExceededException({ $metadata: {}, message: 'over quota' }),
      ),
    ).toBe('RateLimit');
  });

  it('classifies ServiceUnavailableException as Overloaded', () => {
    expect(
      adapter.classifyError(new ServiceUnavailableException({ $metadata: {}, message: 'busy' })),
    ).toBe('Overloaded');
  });

  it('classifies ValidationException as BadRequest', () => {
    expect(adapter.classifyError(new ValidationException({ $metadata: {}, message: 'bad' }))).toBe(
      'BadRequest',
    );
  });

  it('classifies ResourceNotFoundException as BadRequest', () => {
    expect(
      adapter.classifyError(
        new ResourceNotFoundException({ $metadata: {}, message: 'no such model' }),
      ),
    ).toBe('BadRequest');
  });

  it('classifies AccessDeniedException as AuthFailure', () => {
    expect(
      adapter.classifyError(new AccessDeniedException({ $metadata: {}, message: 'denied' })),
    ).toBe('AuthFailure');
  });

  it('classifies InternalServerException as Server', () => {
    expect(
      adapter.classifyError(new InternalServerException({ $metadata: {}, message: 'oops' })),
    ).toBe('Server');
  });

  it('classifies AbortError by name as Cancelled', () => {
    const err = new Error('aborted');
    err.name = 'AbortError';
    expect(adapter.classifyError(err)).toBe('Cancelled');
  });

  it('classifies GuardrailIntervenedException by name as GuardrailBlock', () => {
    const err = new Error('blocked');
    err.name = 'GuardrailIntervenedException';
    expect(adapter.classifyError(err)).toBe('GuardrailBlock');
  });

  it("falls back to 'Server' for unrecognized errors", () => {
    expect(adapter.classifyError(new Error('something else'))).toBe('Server');
    expect(adapter.classifyError('string error')).toBe('Server');
    expect(adapter.classifyError(null)).toBe('Server');
  });
});
