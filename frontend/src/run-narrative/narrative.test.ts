import { describe, expect, it } from 'vitest';
import { decodeAcceptanceArtifact } from '@/api/acceptance';
import type { Artifact, AuditEntry, GateView, Stage } from '@/api/types';
import {
  classifyStage,
  classifyTransition,
  decodeAcceptanceOutcome,
  decodeApproval,
  decodePolicyDiff,
  decodeReviewVerdict,
  decodeReviewVerdicts,
  deriveAcceptanceRows,
  deriveApprovals,
  deriveGateTimeline,
  deriveMerge,
  derivePlan,
  deriveScopeDivergence,
  evidenceHref,
  evidenceRefFor,
  joinConcernLifecycle,
  latestAcceptanceOutcome,
  parseEntryParam,
  readPlanAcceptanceCriteria,
  safeExternalHref,
  sequenceEvidenceRef,
} from './narrative';

const RUN = 'rrrrrrrr-0000-0000-0000-000000000000';

function entry(
  sequence: number,
  category: string,
  payload: unknown,
  extra: Partial<AuditEntry> = {},
): AuditEntry {
  return {
    id: `e${sequence}`,
    sequence,
    run_id: RUN,
    stage_id: null,
    ts: `2026-09-28T00:00:${String(sequence).padStart(2, '0')}Z`,
    category,
    actor_kind: 'user',
    actor_subject: 'github:octo',
    payload,
    prev_hash: null,
    entry_hash: `hash-${sequence}`,
    ...extra,
  };
}

function stage(
  id: string,
  sequence: number,
  state: Stage['state'],
  type: Stage['type'] = 'implement',
): Stage {
  return {
    id,
    run_id: RUN,
    sequence,
    type,
    executor: { kind: 'agent', ref: 'claude' },
    state,
    started_at: null,
    ended_at: null,
    failure_category: null,
    failure_reason: null,
    created_at: '2026-09-28T00:00:00Z',
    updated_at: '2026-09-28T00:00:00Z',
  };
}

const planContent = {
  plan_version: 'standard_v1',
  ticket_reference: { type: 'github_issue', url: 'https://github.com/o/r/issues/1', id: '1' },
  generated_by: { agent: 'claude', model: 'm', timestamp: 't' },
  summary: 'Do the thing',
  scope: {
    files: [
      { path: 'a.ts', operation: 'delete' },
      { path: 'b.ts', operation: 'create' },
      { path: 'c.ts', operation: 'modify' },
    ],
  },
  approach: [
    { step: 1, description: 'one' },
    { step: 2, description: 'two' },
  ],
  verification: {
    test_strategy: 'ts',
    rollback_plan: 'rb',
    acceptance_criteria: [
      { id: 'lists-runs', statement: 'lists runs', source: 'explicit' },
      { id: 'rejects-bad-id', statement: 'rejects bad ids', source: 'explicit', blocking: false },
    ],
  },
};

const planArtifact: Artifact = {
  id: 'art-plan',
  stage_id: 'st-plan',
  kind: 'plan',
  schema_version: 'standard_v1',
  content_hash: 'sha256:plan',
  content: planContent,
  created_at: '2026-09-28T00:00:00Z',
};

describe('evidence references (approval condition 1)', () => {
  it('links an audit-backed claim to ?entry=<sequence> plus an in-page anchor, never a bare #audit', () => {
    const ref = evidenceRefFor(entry(742, 'approval_submitted', {}), RUN);
    expect(ref).toEqual({
      kind: 'audit',
      runId: RUN,
      sequence: 742,
      entryHash: 'hash-742',
      category: 'approval_submitted',
    });
    expect(evidenceHref(ref)).toBe(`/runs/${RUN}?entry=742#entry-742`);
    expect(evidenceHref(ref)).not.toMatch(/#audit$/);
  });

  it('links an artifact-backed claim to its stage page', () => {
    const ref = evidenceRefFor(planArtifact, RUN);
    expect(ref.kind).toBe('artifact');
    expect(evidenceHref(ref)).toBe(`/runs/${RUN}/stages/st-plan`);
  });

  it.each([
    ['742', 742],
    [null, null],
    ['', null],
    ['0', null],
    ['-1', null],
    ['1e3', null],
    ['12abc', null],
    ['99999999999999999999', null],
  ])('parseEntryParam(%j) → %j', (raw, want) => {
    expect(parseEntryParam(raw)).toBe(want);
  });
});

describe('derivePlan / readPlanAcceptanceCriteria', () => {
  it('derives the plan block model with its criteria inventory', () => {
    const m = derivePlan(planArtifact, RUN);
    expect(m?.summary).toBe('Do the thing');
    expect(m?.ticketId).toBe('1');
    expect(m?.scopeFiles).toHaveLength(3);
    expect(m?.approachStepCount).toBe(2);
    expect(m?.acceptanceCriteria).toEqual([
      { id: 'lists-runs', statement: 'lists runs', blocking: true, skipExpected: false },
      { id: 'rejects-bad-id', statement: 'rejects bad ids', blocking: false, skipExpected: false },
    ]);
  });

  it('returns null for a non-standard_v1 artifact', () => {
    expect(
      derivePlan({ ...planArtifact, content: { plan_version: 'standard_v2' } }, RUN),
    ).toBeNull();
  });

  it('returns [] criteria for a plan that declares none, or a malformed verification', () => {
    expect(readPlanAcceptanceCriteria({ verification: {} })).toEqual([]);
    expect(readPlanAcceptanceCriteria({ verification: 'x' })).toEqual([]);
    expect(readPlanAcceptanceCriteria(null)).toEqual([]);
    expect(
      readPlanAcceptanceCriteria({
        verification: { acceptance_criteria: [{ statement: 'no id' }] },
      }),
    ).toEqual([]);
  });
});

const reviewPayload = {
  reviewer_kind: 'agent',
  reviewer_model: 'model-a',
  authority: 'advisory',
  verdict: 'approve_with_concerns',
  concerns: [
    { severity: 'high', category: 'correctness', note: 'off by one' },
    { severity: 'low', category: 'style', note: 'rename x' },
    { severity: 'medium', category: 'tests', note: 'never ledgered' },
  ],
  free_form: 'looks fine',
};

describe('decodeReviewVerdict(s)', () => {
  it('decodes a plan_reviewed payload with its entry provenance', () => {
    const row = decodeReviewVerdict(
      entry(10, 'plan_reviewed', reviewPayload, { stage_id: 'st-plan' }),
    );
    expect(row?.stageKind).toBe('plan');
    expect(row?.reviewerModel).toBe('model-a');
    expect(row?.concerns).toHaveLength(3);
    expect(row?.sequence).toBe(10);
    expect(row?.entryHash).toBe('hash-10');
    expect(row?.stageId).toBe('st-plan');
  });

  it.each([
    ['a non-object payload', 'nope'],
    ['a payload with no verdict', { concerns: [] }],
    ['a non-array concerns', { verdict: 'approve', concerns: 'x' }],
    ['a concern with no note', { verdict: 'approve', concerns: [{ severity: 'low' }] }],
  ])('returns null (never throws) for %s', (_label, payload) => {
    expect(decodeReviewVerdict(entry(1, 'implement_reviewed', payload))).toBeNull();
  });

  it('returns null for a non-review category', () => {
    expect(decodeReviewVerdict(entry(1, 'approval_submitted', reviewPayload))).toBeNull();
  });

  it('counts undecodable entries instead of dropping them silently, oldest first', () => {
    const { rows, undecodable } = decodeReviewVerdicts([
      entry(20, 'implement_reviewed', reviewPayload),
      entry(5, 'plan_reviewed', reviewPayload),
      entry(7, 'plan_reviewed', 'garbage'),
    ]);
    expect(rows.map((r) => r.sequence)).toEqual([5, 20]);
    expect(undecodable).toBe(1);
  });
});

describe('joinConcernLifecycle', () => {
  const verdict = decodeReviewVerdict(entry(30, 'implement_reviewed', reviewPayload))!;
  const gateView: GateView = {
    run_id: RUN,
    open: [
      {
        id: 'c-open',
        stage_kind: 'implement',
        origin_review_sequence: 30,
        reviewer_model: 'model-a',
        severity: 'high',
        category: 'correctness',
        state: 'addressed_pending',
        note: 'off by one',
        has_suggested_patch: false,
        fixups: [{ sequence: 31, outcome: 'pushed', head_sha: 'abc' }],
        resolutions: [{ sequence: 33, resolution: 'reopened' }],
        disputed: true,
      },
    ],
    settled: [
      {
        id: 'c-settled',
        stage_kind: 'implement',
        state: 'waived',
        severity: 'low',
        category: 'style',
        reviewer_model: 'model-a',
        note: 'rename x',
        state_reason: 'cosmetic',
      },
    ],
    suppressed_relitigations: [],
    history_incomplete: false,
  };

  it('joins open and settled rows and marks an unjoined concern `unmatched`', () => {
    const [joined] = joinConcernLifecycle([verdict], gateView);
    expect(joined.concerns.map((c) => c.lifecycle)).toEqual([
      'addressed_pending',
      'waived',
      'unmatched',
    ]);
    expect(joined.concerns[0].fixups).toEqual([
      { sequence: 31, outcome: 'pushed', head_sha: 'abc' },
    ]);
    expect(joined.concerns[0].resolutions).toHaveLength(1);
    expect(joined.concerns[0].disputed).toBe(true);
    expect(joined.concerns[0].concernId).toBe('c-open');
    expect(joined.concerns[1].stateReason).toBe('cosmetic');
    expect(joined.concerns[2].concernId).toBeUndefined();
  });

  it('does not join an open row raised by a DIFFERENT review sequence', () => {
    const other = decodeReviewVerdict(entry(99, 'implement_reviewed', reviewPayload))!;
    const [joined] = joinConcernLifecycle([other], { ...gateView, settled: [] });
    expect(joined.concerns[0].lifecycle).toBe('unmatched');
  });

  it('joins each ledger row at most once (a near-duplicate pair cannot both claim it)', () => {
    const dup = decodeReviewVerdict(
      entry(40, 'implement_reviewed', {
        ...reviewPayload,
        concerns: [
          { severity: 'low', category: 'style', note: 'rename x' },
          { severity: 'low', category: 'style', note: 'rename x' },
        ],
      }),
    )!;
    const [joined] = joinConcernLifecycle([dup], gateView);
    expect(joined.concerns.map((c) => c.lifecycle)).toEqual(['waived', 'unmatched']);
  });

  it('labels every concern `unavailable` when the gate view could not be read', () => {
    const [joined] = joinConcernLifecycle([verdict], null);
    expect(joined.concerns.every((c) => c.lifecycle === 'unavailable')).toBe(true);
  });

  it('labels a joined row with an unrecognised state `unknown_state`, keeping the raw state', () => {
    const gv: GateView = { ...gateView, open: [{ ...gateView.open[0], state: 'brand_new' }] };
    const [joined] = joinConcernLifecycle([verdict], gv);
    expect(joined.concerns[0].lifecycle).toBe('unknown_state');
    expect(joined.concerns[0].rawState).toBe('brand_new');
  });
});

describe('deriveScopeDivergence', () => {
  const declared = [
    { path: 'a.ts', operation: 'delete' as const },
    { path: 'b.ts', operation: 'create' as const },
  ];

  it('folds a rename old_path so a planned delete+create rename is NOT a divergence', () => {
    const d = deriveScopeDivergence(declared, {
      diff: [{ path: 'b.ts', status: 'R', old_path: 'a.ts' }],
    });
    expect(d.declaredOnly).toEqual([]);
    expect(d.stagedOnly).toEqual([]);
    expect(d.both).toEqual(['a.ts', 'b.ts']);
  });

  it('does not fold old_path for a copy (the source file is untouched)', () => {
    const d = deriveScopeDivergence(declared, {
      diff: [{ path: 'b.ts', status: 'C', old_path: 'a.ts' }],
    });
    expect(d.declaredOnly).toEqual(['a.ts']);
  });

  it('marks paths present in only one set', () => {
    const d = deriveScopeDivergence(
      [
        { path: 'x.ts', operation: 'modify' },
        { path: 'y.ts', operation: 'modify' },
      ],
      {
        diff: [
          { path: 'y.ts', status: 'M' },
          { path: 'z.ts', status: 'A' },
        ],
      },
    );
    expect(d.declaredOnly).toEqual(['x.ts']);
    expect(d.stagedOnly).toEqual(['z.ts']);
    expect(d.both).toEqual(['y.ts']);
  });

  it('keeps the staged column when the declared scope is unavailable', () => {
    const d = deriveScopeDivergence(null, { diff: [{ path: 'y.ts', status: 'M' }] });
    expect(d.declared).toBeNull();
    expect(d.staged).toEqual(['y.ts']);
    expect(d.declaredOnly).toEqual([]);
  });

  it('keeps the declared column when no policy_evaluated payload exists', () => {
    const d = deriveScopeDivergence(declared, undefined);
    expect(d.declared).toEqual(['a.ts', 'b.ts']);
    expect(d.staged).toBeNull();
    expect(d.declaredOnly).toEqual([]);
  });

  it.each([
    ['a non-object', 'x'],
    ['no diff array', { passed: true }],
    ['a diff entry with no path', { diff: [{ status: 'M' }] }],
  ])('decodePolicyDiff returns null for %s', (_label, payload) => {
    expect(decodePolicyDiff(payload)).toBeNull();
  });
});

describe('acceptance outcome + rows (approval condition 2)', () => {
  const outcomePayload = {
    artifact_id: 'art-acc',
    content_hash: 'sha256:acc',
    verdict: 'failed',
    failure_mode: 'assertion_fail',
    criteria_passed: 1,
    criteria_failed: 1,
    criteria_skipped: 0,
    criteria_undecidable: 0,
    criteria_total: 2,
  };

  it('decodes the outcome tallies and keeps the entry as evidence', () => {
    const o = decodeAcceptanceOutcome(
      entry(50, 'acceptance_outcome_recorded', outcomePayload, { stage_id: 'st-acc' }),
    );
    expect(o?.verdict).toBe('failed');
    expect(o?.tallies).toEqual({ passed: 1, failed: 1, skipped: 0, undecidable: 0, total: 2 });
    expect(o?.stageId).toBe('st-acc');
    expect(o?.evidence).toMatchObject({ kind: 'audit', sequence: 50 });
  });

  it('picks the newest decodable outcome', () => {
    const o = latestAcceptanceOutcome([
      entry(50, 'acceptance_outcome_recorded', outcomePayload),
      entry(60, 'acceptance_outcome_recorded', { ...outcomePayload, verdict: 'passed' }),
      entry(70, 'acceptance_outcome_recorded', 'garbage'),
    ]);
    expect(o?.verdict).toBe('passed');
  });

  it('returns null for a payload with no artifact_id', () => {
    expect(
      decodeAcceptanceOutcome(entry(1, 'acceptance_outcome_recorded', { verdict: 'passed' })),
    ).toBeNull();
  });

  const planCriteria = readPlanAcceptanceCriteria(planContent);

  it('renders a pending row for a plan criterion missing from the acceptance artifact', () => {
    const body = decodeAcceptanceArtifact({
      verdict: 'passed',
      criteria: [{ id: 'lists-runs', result: 'passed' }],
    })!;
    const rows = deriveAcceptanceRows(planCriteria, body);
    expect(rows.map((r) => [r.id, r.status])).toEqual([
      ['lists-runs', 'passed'],
      ['rejects-bad-id', 'pending'],
    ]);
  });

  it('renders rows from a historical object-keyed artifact through the one decoding contract', () => {
    const body = decodeAcceptanceArtifact({
      verdict: 'failed',
      criteria: {
        'rejects-bad-id': { result: 'failed', observed: '500' },
        'lists-runs': { result: 'passed' },
      },
    })!;
    const rows = deriveAcceptanceRows(planCriteria, body);
    expect(rows.map((r) => [r.id, r.status])).toEqual([
      ['lists-runs', 'passed'],
      ['rejects-bad-id', 'failed'],
    ]);
    expect(rows[1].observed).toBe('500');
  });

  it('every plan criterion is pending when there is no artifact yet', () => {
    expect(deriveAcceptanceRows(planCriteria, null).map((r) => r.status)).toEqual([
      'pending',
      'pending',
    ]);
  });

  it('appends an artifact criterion the plan does not declare, flagged inPlan=false', () => {
    const body = decodeAcceptanceArtifact({
      verdict: 'passed',
      criteria: [{ id: 'extra', result: 'undecidable', undecidable_reason: 'no target' }],
    })!;
    const rows = deriveAcceptanceRows(planCriteria, body);
    expect(rows.at(-1)).toEqual({
      id: 'extra',
      statement: '',
      status: 'undecidable',
      inPlan: false,
      undecidableReason: 'no target',
    });
  });
});

describe('approvals', () => {
  it('decodes identity, auth_method, channel and the delegation fields when present', () => {
    const row = decodeApproval(
      entry(80, 'approval_submitted', {
        decision: 'approve',
        approver: 'github:octo',
        identity: { provider: 'github', subject: 'github:octo' },
        auth_method: 'session',
        channel: 'interactive',
        delegated: 'may_approve',
        on_behalf_of: 'github:boss',
        comment: 'ship it',
      }),
    );
    expect(row).toMatchObject({
      decision: 'approve',
      identity: { provider: 'github', subject: 'github:octo' },
      authMethod: 'session',
      channel: 'interactive',
      delegated: 'may_approve',
      onBehalfOf: 'github:boss',
      comment: 'ship it',
    });
  });

  it('leaves auth_method and channel ABSENT (not undefined-valued keys) when the payload omits them', () => {
    const row = decodeApproval(
      entry(81, 'approval_submitted', {
        decision: 'approve',
        identity: { provider: '', subject: 'octo' },
      }),
    )!;
    expect('authMethod' in row).toBe(false);
    expect('channel' in row).toBe(false);
    expect(row.identity).toEqual({ subject: 'octo' });
  });

  it('treats an empty-string auth_method as absent', () => {
    const row = decodeApproval(
      entry(82, 'approval_submitted', { decision: 'reject', auth_method: '' }),
    )!;
    expect('authMethod' in row).toBe(false);
    expect('identity' in row).toBe(false);
  });

  it('skips undecodable approvals and orders the rest oldest first', () => {
    const rows = deriveApprovals([
      entry(90, 'approval_submitted', { decision: 'approve' }),
      entry(85, 'approval_submitted', { decision: 'reject' }),
      entry(86, 'approval_submitted', { nope: true }),
    ]);
    expect(rows.map((r) => r.sequence)).toEqual([85, 90]);
  });
});

describe('deriveMerge', () => {
  it('returns null when there is no merge-related entry', () => {
    expect(deriveMerge([])).toBeNull();
  });

  it('keeps the verdict separate from the outcome (a verdict alone is not a merge)', () => {
    const m = deriveMerge([entry(100, 'merge_verdict_recorded', { verdict: 'ship', pr_url: 'u' })]);
    expect(m?.outcome).toBeNull();
    expect(m?.verdict?.verdict).toBe('ship');
  });

  it('reads pr_merged as merged with the head sha and merger', () => {
    const m = deriveMerge([
      entry(100, 'merge_verdict_recorded', { verdict: 'ship' }),
      entry(101, 'pr_merged', { pr_url: 'https://x/pull/1', merger: 'octo', head_sha: 'deadbeef' }),
    ]);
    expect(m?.outcome).toMatchObject({
      kind: 'merged',
      source: 'pr_merged',
      commitSha: 'deadbeef',
      actor: 'octo',
    });
    expect(m?.verdict?.verdict).toBe('ship');
  });

  it('newest terminal outcome wins', () => {
    const m = deriveMerge([
      entry(101, 'pr_closed_without_merge', { pr_url: 'u', closer: 'octo' }),
      entry(110, 'merge_observation_recorded', {
        pull_request_url: 'u',
        merge_commit_sha: 'cafe',
        merged_at: 'T',
      }),
    ]);
    expect(m?.outcome).toMatchObject({
      kind: 'merged',
      source: 'merge_observation_recorded',
      commitSha: 'cafe',
      ts: 'T',
    });
  });

  it('reads pr_closed_without_merge as closed', () => {
    const m = deriveMerge([entry(101, 'pr_closed_without_merge', { pr_url: 'u', closer: 'octo' })]);
    expect(m?.outcome?.kind).toBe('closed_without_merge');
  });
});

describe('classifyTransition / classifyStage / deriveGateTimeline (approval condition 3)', () => {
  it.each([
    ['approval_submitted', 'gate'],
    ['acceptance_outcome_recorded', 'gate'],
    ['approval_predicate_rejected', 'gate'],
    ['run_auto_advanced', 'auto_advance'],
    ['run_auto_driven', 'auto_advance'],
    ['plan_reviewed', 'other'],
  ])('classifyTransition(%s) → %s', (category, want) => {
    expect(classifyTransition({ category })).toBe(want);
  });

  it.each([
    ['awaiting_approval', 'gate'],
    ['awaiting_deploy_approval', 'gate'],
    ['running', 'other'],
    ['succeeded', 'other'],
  ] as const)('classifyStage(%s) → %s', (state, want) => {
    expect(classifyStage({ state })).toBe(want);
  });

  it('composes a timeline with a parked gate stage, a predicate-rejected gate and a run_auto_driven auto-advance', () => {
    const stages = [
      stage('st-impl', 2, 'awaiting_approval'),
      stage('st-plan', 1, 'succeeded', 'plan'),
    ];
    const t = deriveGateTimeline(stages, [
      entry(5, 'approval_predicate_rejected', {}, { stage_id: 'st-plan' }),
      entry(3, 'approval_submitted', {}, { stage_id: 'st-plan' }),
      entry(6, 'run_auto_driven', {}),
      entry(7, 'plan_reviewed', {}, { stage_id: 'st-plan' }),
    ]);
    expect(t.stages.map((s) => [s.stage.id, s.treatment])).toEqual([
      ['st-plan', 'other'],
      ['st-impl', 'gate'],
    ]);
    // The parked stage is a gate from its STATE alone — it has no audit entry.
    expect(t.stages[1].transitions).toEqual([]);
    expect(t.stages[0].transitions.map((x) => [x.category, x.treatment])).toEqual([
      ['approval_submitted', 'gate'],
      ['approval_predicate_rejected', 'gate'],
    ]);
    expect(t.runTransitions.map((x) => [x.category, x.treatment])).toEqual([
      ['run_auto_driven', 'auto_advance'],
    ]);
    expect(t.runTransitions[0].evidence).toMatchObject({ kind: 'audit', sequence: 6 });
  });
});

describe('safeExternalHref', () => {
  it('passes http(s) urls through and refuses every other value', () => {
    expect(safeExternalHref('https://example.test/a?b=1#c')).toBe('https://example.test/a?b=1#c');
    expect(safeExternalHref('http://example.test/a')).toBe('http://example.test/a');
    for (const hostile of [
      'javascript:alert(1)',
      '  JavaScript:alert(1)',
      'jAvAsCrIpT:alert(1)',
      'data:text/html;base64,PHNjcmlwdD4=',
      'vbscript:msgbox(1)',
      'file:///etc/passwd',
      // Not absolute: no scheme to trust, so it is not rendered as a link.
      '/relative/path',
      'example.test/a',
      '',
    ]) {
      expect(safeExternalHref(hostile), hostile).toBeNull();
    }
    expect(safeExternalHref(undefined)).toBeNull();
    expect(safeExternalHref(null)).toBeNull();
  });
});

describe('sequenceEvidenceRef', () => {
  it('resolves a sequence-only claim through the same ?entry= path', () => {
    const ref = sequenceEvidenceRef('run-1', 12);
    expect(ref).toEqual({ kind: 'audit', runId: 'run-1', sequence: 12 });
    expect(evidenceHref(ref)).toBe('/runs/run-1?entry=12#entry-12');
  });
});
