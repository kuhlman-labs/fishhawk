import { describe, expect, it } from 'vitest';
import {
  decodeAcceptanceArtifact,
  isAcceptanceArtifact,
  normalizeAcceptanceCriteria,
} from './acceptance';

const flat = {
  verdict: 'failed',
  failure_mode: 'assertion_fail',
  criteria: [
    { id: 'lists-runs', result: 'passed', observed: '200 OK' },
    { id: 'rejects-bad-id', result: 'failed', expected: '400', observed: '500' },
  ],
  target_url: 'http://localhost:8080',
  evidence_hashes: ['sha256:aaa'],
  notes: 'overflow remark',
};

// The historical #1574 shape: an object keyed by criterion id, element ids
// absent (or equal to the key). Keys are deliberately out of order.
const objectKeyed = {
  verdict: 'passed',
  criteria: {
    'rejects-bad-id': { result: 'passed' },
    'lists-runs': { id: 'lists-runs', result: 'skipped' },
  },
  evidence_hashes: { second: 'sha256:bbb', first: 'sha256:aaa' },
};

// A PullRequestArtifactBody-shaped content: no `verdict` key at all.
const pullRequestShaped = {
  pr_number: 42,
  pr_url: 'https://github.com/kuhlman-labs/fishhawk/pull/42',
  branch: 'fishhawk/run-aaa/stage-bbb',
  head_sha: '1111111111111111111111111111111111111111',
  base_sha: '2222222222222222222222222222222222222222',
  title: 'feat: something',
  files_changed_count: 3,
};

describe('normalizeAcceptanceCriteria', () => {
  it('returns [] for absent and null criteria', () => {
    expect(normalizeAcceptanceCriteria(undefined)).toEqual([]);
    expect(normalizeAcceptanceCriteria(null)).toEqual([]);
  });

  it('passes the flat array through, keeping optional evidence fields', () => {
    expect(normalizeAcceptanceCriteria(flat.criteria)).toEqual([
      { id: 'lists-runs', result: 'passed', observed: '200 OK' },
      { id: 'rejects-bad-id', result: 'failed', expected: '400', observed: '500' },
    ]);
  });

  it('folds the object key into the element id and sorts by id', () => {
    expect(normalizeAcceptanceCriteria(objectKeyed.criteria)).toEqual([
      { id: 'lists-runs', result: 'skipped' },
      { id: 'rejects-bad-id', result: 'passed' },
    ]);
  });

  it('refuses an object key that conflicts with a non-empty element id', () => {
    expect(normalizeAcceptanceCriteria({ a: { id: 'b', result: 'passed' } })).toBeNull();
  });

  it.each([
    ['a string', 'passed'],
    ['a number', 3],
    ['an array element with no id', [{ result: 'passed' }]],
    ['an array element with an unknown result', [{ id: 'x', result: 'green' }]],
    ['an object value that is not an object', { x: 'passed' }],
  ])('returns null for %s', (_label, raw) => {
    expect(normalizeAcceptanceCriteria(raw)).toBeNull();
  });
});

describe('isAcceptanceArtifact', () => {
  it('accepts the flat-array shape', () => {
    expect(isAcceptanceArtifact(flat)).toBe(true);
  });

  it('accepts the historical object-keyed shape (normalized before the guard decides)', () => {
    expect(isAcceptanceArtifact(objectKeyed)).toBe(true);
  });

  it('accepts a verdict with no criteria', () => {
    expect(isAcceptanceArtifact({ verdict: 'passed' })).toBe(true);
  });

  it('rejects a pull_request-shaped body with no verdict', () => {
    expect(isAcceptanceArtifact(pullRequestShaped)).toBe(false);
  });

  it('rejects criteria that decode under neither shape', () => {
    expect(isAcceptanceArtifact({ verdict: 'passed', criteria: 'all good' })).toBe(false);
  });

  it.each([null, undefined, 'string', 42, []])('rejects non-objects (%s)', (v) => {
    expect(isAcceptanceArtifact(v)).toBe(false);
  });
});

describe('decodeAcceptanceArtifact', () => {
  it('decodes the flat shape with every optional field', () => {
    expect(decodeAcceptanceArtifact(flat)).toEqual(flat);
  });

  it('returns the normalized flat criteria array for an object-keyed artifact', () => {
    const body = decodeAcceptanceArtifact(objectKeyed);
    expect(body?.criteria.map((c) => c.id)).toEqual(['lists-runs', 'rejects-bad-id']);
    expect(body?.evidence_hashes).toEqual(['sha256:aaa', 'sha256:bbb']);
  });

  it('returns null for a body the guard rejects', () => {
    expect(decodeAcceptanceArtifact(pullRequestShaped)).toBeNull();
  });

  it('drops an unreadable evidence_hashes value rather than failing the artifact', () => {
    const body = decodeAcceptanceArtifact({ verdict: 'passed', evidence_hashes: [1, 2] });
    expect(body).not.toBeNull();
    expect(body?.evidence_hashes).toBeUndefined();
  });
});
