import { describe, expect, it } from 'vitest';
import { isDeploymentArtifact } from './deployment';

const valid = {
  environment: 'production',
  ref: '1111111111111111111111111111111111111111',
  external_run_url: 'https://github.com/kuhlman-labs/fishhawk/actions/runs/42',
  outcome: 'succeeded',
};

describe('isDeploymentArtifact', () => {
  it('accepts a complete artifact body', () => {
    expect(isDeploymentArtifact(valid)).toBe(true);
  });

  it('accepts a body with the optional rollback_handle / rollback_action fields', () => {
    expect(
      isDeploymentArtifact({
        ...valid,
        outcome: 'rolled_back',
        rollback_handle: 'rollback-token-7',
        rollback_action: 'completed',
      }),
    ).toBe(true);
  });

  it.each([null, undefined, 'string', 42, []])('rejects non-objects (%s)', (v) => {
    expect(isDeploymentArtifact(v)).toBe(false);
  });

  it('rejects an empty object', () => {
    expect(isDeploymentArtifact({})).toBe(false);
  });

  it.each(['environment', 'ref', 'external_run_url', 'outcome'])(
    'rejects when required field %s is missing',
    (field) => {
      const partial = { ...valid } as Record<string, unknown>;
      delete partial[field];
      expect(isDeploymentArtifact(partial)).toBe(false);
    },
  );

  it('rejects when environment is a non-string', () => {
    expect(isDeploymentArtifact({ ...valid, environment: 42 })).toBe(false);
  });

  it('rejects when outcome is a non-string', () => {
    expect(isDeploymentArtifact({ ...valid, outcome: 7 })).toBe(false);
  });
});
