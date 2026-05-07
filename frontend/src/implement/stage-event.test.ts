import { describe, expect, it } from 'vitest';
import { renderStageEvent } from './stage-event';
import type { AuditEntry } from '@/api/types';

function makeEntry(over: Partial<AuditEntry> = {}): AuditEntry {
  return {
    id: 'aaaaaaaa-1111-1111-1111-111111111111',
    sequence: 1,
    run_id: '11111111-2222-3333-4444-555555555555',
    stage_id: 'bbbbbbbb-2222-2222-2222-222222222222',
    ts: '2026-05-07T12:00:00Z',
    category: 'stage_dispatched',
    actor_kind: 'system',
    actor_subject: null,
    payload: {},
    prev_hash: null,
    entry_hash: 'abcdef1234567890',
    ...over,
  };
}

describe('renderStageEvent', () => {
  it('renders stage_dispatched as a bare label', () => {
    expect(renderStageEvent(makeEntry())).toEqual({ label: 'Stage dispatched' });
  });

  it('renders signing_key_issued as a bare label', () => {
    expect(renderStageEvent(makeEntry({ category: 'signing_key_issued' }))).toEqual({
      label: 'Signing key issued',
    });
  });

  it('appends the auth_method to installation_token_issued when present', () => {
    expect(
      renderStageEvent(
        makeEntry({
          category: 'installation_token_issued',
          payload: { auth_method: 'oidc' },
        }),
      ),
    ).toEqual({ label: 'Installation token issued · oidc' });
  });

  it('omits the auth_method clause when not provided', () => {
    expect(
      renderStageEvent(makeEntry({ category: 'installation_token_issued', payload: {} })),
    ).toEqual({ label: 'Installation token issued' });
  });

  it('summarizes a passing policy_evaluated entry with file count', () => {
    const out = renderStageEvent(
      makeEntry({
        category: 'policy_evaluated',
        payload: {
          passed: true,
          diff: [{ path: 'a.go' }, { path: 'b.go' }, { path: 'c.go' }],
        },
      }),
    );
    expect(out.label).toBe('Policy evaluated');
    expect(out.detail).toBe('3 files · pass');
  });

  it('uses singular "file" when exactly one diff entry is present', () => {
    const out = renderStageEvent(
      makeEntry({
        category: 'policy_evaluated',
        payload: { passed: true, diff: [{ path: 'a.go' }] },
      }),
    );
    expect(out.detail).toBe('1 file · pass');
  });

  it('summarizes a failing policy_evaluated entry with violation count', () => {
    const out = renderStageEvent(
      makeEntry({
        category: 'policy_evaluated',
        payload: {
          passed: false,
          diff: [{ path: 'infra/main.tf' }],
          violations: [
            { rule: 'forbidden_paths', message: 'infra/** disallowed' },
            { rule: 'tests_added_or_updated', message: 'no tests changed' },
          ],
        },
      }),
    );
    expect(out.detail).toBe('1 file · 2 violations');
  });

  it('renders pull_request_opened with the PR number and file count', () => {
    expect(
      renderStageEvent(
        makeEntry({
          category: 'pull_request_opened',
          payload: { pr_number: 209, files_changed_count: 3 },
        }),
      ),
    ).toEqual({ label: 'Pull request opened · #209 · 3 files' });
  });

  it('handles pull_request_opened with single file changed', () => {
    expect(
      renderStageEvent(
        makeEntry({
          category: 'pull_request_opened',
          payload: { pr_number: 1, files_changed_count: 1 },
        }),
      ),
    ).toEqual({ label: 'Pull request opened · #1 · 1 file' });
  });

  it('renders stage_failed with category and reason detail', () => {
    expect(
      renderStageEvent(
        makeEntry({
          category: 'stage_failed',
          payload: { failure_category: 'B', reason: 'forbidden_paths violation' },
        }),
      ),
    ).toEqual({ label: 'Stage failed · category B', detail: 'forbidden_paths violation' });
  });

  it('falls back to the raw category + JSON peek for unknown categories', () => {
    const out = renderStageEvent(
      makeEntry({
        category: 'something_new',
        payload: { foo: 'bar' },
      }),
    );
    expect(out.label).toBe('something_new');
    expect(out.detail).toContain('"foo":"bar"');
  });

  it('truncates long unknown-category JSON peeks', () => {
    const longPayload: Record<string, string> = {};
    for (let i = 0; i < 20; i++) longPayload[`k${i}`] = 'v';
    const out = renderStageEvent(makeEntry({ category: 'something_new', payload: longPayload }));
    expect(out.detail?.endsWith('…')).toBe(true);
    expect(out.detail!.length).toBeLessThanOrEqual(80);
  });

  it('omits the JSON peek for empty unknown-category payloads', () => {
    const out = renderStageEvent(makeEntry({ category: 'something_new', payload: {} }));
    expect(out.label).toBe('something_new');
    expect(out.detail).toBeUndefined();
  });
});
