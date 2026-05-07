import { describe, expect, it } from 'vitest';
import { isPullRequestArtifact } from './pull-request';

const valid = {
  pr_number: 42,
  pr_url: 'https://github.com/kuhlman-labs/fishhawk/pull/42',
  branch: 'fishhawk/run-aaa/stage-bbb',
  head_sha: '1111111111111111111111111111111111111111',
  base_sha: '2222222222222222222222222222222222222222',
  title: 'Fishhawk: implement stage 22222222',
  files_changed_count: 3,
};

describe('isPullRequestArtifact', () => {
  it('accepts a complete artifact body', () => {
    expect(isPullRequestArtifact(valid)).toBe(true);
  });

  it('accepts a body without the optional `body` field', () => {
    const { ...without } = valid;
    expect(isPullRequestArtifact(without)).toBe(true);
  });

  it('accepts a body with `body` set to a markdown string', () => {
    expect(isPullRequestArtifact({ ...valid, body: 'Closes #184' })).toBe(true);
  });

  it.each([null, undefined, 'string', 42, []])('rejects non-objects (%s)', (v) => {
    expect(isPullRequestArtifact(v)).toBe(false);
  });

  it('rejects an empty object', () => {
    expect(isPullRequestArtifact({})).toBe(false);
  });

  it.each([
    'pr_number',
    'pr_url',
    'branch',
    'head_sha',
    'base_sha',
    'title',
    'files_changed_count',
  ])('rejects when required field %s is missing', (field) => {
    const partial = { ...valid } as Record<string, unknown>;
    delete partial[field];
    expect(isPullRequestArtifact(partial)).toBe(false);
  });

  it('rejects when pr_number is a string', () => {
    expect(isPullRequestArtifact({ ...valid, pr_number: '42' })).toBe(false);
  });

  it('rejects when files_changed_count is a string', () => {
    expect(isPullRequestArtifact({ ...valid, files_changed_count: '3' })).toBe(false);
  });
});
