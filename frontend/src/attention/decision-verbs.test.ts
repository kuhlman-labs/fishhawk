import { describe, expect, it } from 'vitest';
import type { AttentionItemKind } from '@/api/types';
import { DECISION_VERBS, isDrivePlaneVerb } from './decision-verbs';

const ALL_KINDS: AttentionItemKind[] = [
  'plan_gate',
  'scope_amendment',
  'acceptance_disposition',
  'split_verdict',
  'paged_concern',
  'attend_human_led_campaign',
];

describe('DECISION_VERBS', () => {
  it('is exhaustive over all six attention-item kinds', () => {
    expect(Object.keys(DECISION_VERBS).sort()).toEqual([...ALL_KINDS].sort());
  });

  it('maps attend_human_led_campaign to an empty verb list (no decision endpoint)', () => {
    expect(DECISION_VERBS.attend_human_led_campaign).toEqual([]);
  });

  it('offers NO drive-plane verb in any kind (the no-drive-plane-verbs criterion)', () => {
    for (const kind of ALL_KINDS) {
      for (const verb of DECISION_VERBS[kind]) {
        expect(isDrivePlaneVerb(verb), `${kind} verb "${verb}" is a drive-plane verb`).toBe(false);
      }
    }
  });
});

describe('isDrivePlaneVerb', () => {
  it('matches re-execution / routing verbs (case-insensitive, word-boundary)', () => {
    for (const label of ['Retry', 'Dispatch', 'Fix-up', 'Regenerate', 're-run', 'Reap failure']) {
      expect(isDrivePlaneVerb(label), `"${label}" should match`).toBe(true);
    }
  });

  it('does NOT match the decision verbs the panels render', () => {
    for (const label of ['Approve', 'Reject', 'Deny', 'Waive', 'Defer', 'Record arbitration']) {
      expect(isDrivePlaneVerb(label), `"${label}" should not match`).toBe(false);
    }
  });
});
