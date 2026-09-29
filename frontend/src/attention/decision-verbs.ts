import type { AttentionItemKind } from '@/api/types';

/*
 * The SINGLE SOURCE of which decision verbs each attention-queue kind's
 * panel may offer (E40.2 / #1717). The panels render their submit buttons
 * FROM this record, so it is the machine-checkable form of the issue's
 * "no drive-plane verbs in the queue" criterion: a test holds the record
 * to account in both directions (every declared verb is rendered, and
 * nothing beyond it), and `isDrivePlaneVerb` proves no declared verb is a
 * re-execution verb. Adding a drive-plane verb here goes red.
 *
 * attend_human_led_campaign maps to [] — it is work for a human to lead,
 * not a decision to submit, and has no decision endpoint.
 */
export const DECISION_VERBS: Record<AttentionItemKind, readonly string[]> = {
  plan_gate: ['Approve', 'Reject'],
  scope_amendment: ['Approve', 'Deny'],
  acceptance_disposition: ['Record arbitration'],
  split_verdict: ['Waive', 'Defer'],
  paged_concern: ['Waive', 'Defer'],
  attend_human_led_campaign: [],
};

/*
 * Drive-plane verbs — re-execution / routing actions that must NEVER appear
 * as a queue affordance (dispatch, retry, fix-up, re-run, regenerate, run a
 * stage, reap). `isDrivePlaneVerb` is a case-insensitive, word-boundary
 * predicate: the queue's tests assert it matches NO label on any rendered
 * button or link, covering EVERY control, not just the DECISION_VERBS ones.
 */
export const DRIVE_PLANE_VERBS = [
  'dispatch',
  'retry',
  'fixup',
  'fix-up',
  'rerun',
  're-run',
  'regenerate',
  'run stage',
  'reap',
] as const;

const DRIVE_PLANE_PATTERN = new RegExp(
  `\\b(${DRIVE_PLANE_VERBS.map((v) => v.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')).join('|')})\\b`,
  'i',
);

export function isDrivePlaneVerb(label: string): boolean {
  return DRIVE_PLANE_PATTERN.test(label);
}
