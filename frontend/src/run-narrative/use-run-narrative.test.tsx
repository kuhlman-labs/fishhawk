import { beforeEach, describe, expect, it, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import type { Artifact, AuditEntry, GateView, PaginatedList, Run, Stage } from '@/api/types';

/*
 * The whole @/api/client module is mocked: the loader's default client is
 * the real `api` object, so these tests exercise the production wiring (no
 * injected client) and each case rejects exactly the read it is about.
 */
vi.mock('@/api/client', () => ({
  api: {
    getRun: vi.fn(),
    listRunStages: vi.fn(),
    getRunGateView: vi.fn(),
    listRunAudit: vi.fn(),
    listStageArtifacts: vi.fn(),
    getArtifact: vi.fn(),
  },
}));

import { api } from '@/api/client';
import {
  AUDIT_MAX_PAGES,
  NARRATIVE_AUDIT_CATEGORIES,
  loadAuditEntry,
  loadRunNarrative,
  useAuditEntry,
  useRunNarrative,
} from './use-run-narrative';

const m = vi.mocked(api);
const RUN = 'rrrrrrrr-0000-0000-0000-000000000000';

const run: Run = {
  id: RUN,
  repo: 'o/r',
  workflow_id: 'feature_change',
  workflow_sha: 'sha',
  trigger_source: 'github_issue',
  trigger_ref: 'issue:1',
  state: 'running',
  retry_attempt: 0,
  max_retries_snapshot: 1,
  created_at: 'T',
  updated_at: 'T',
};

function stage(id: string, sequence: number, type: string, state: Stage['state']): Stage {
  return {
    id,
    run_id: RUN,
    sequence,
    // The acceptance stage's wire type is absent from the StageType union
    // (see types.ts); the narrative must not depend on it.
    type: type as Stage['type'],
    executor: { kind: 'agent', ref: 'claude' },
    state,
    started_at: null,
    ended_at: null,
    failure_category: null,
    failure_reason: null,
    created_at: 'T',
    updated_at: 'T',
  };
}

const stages = [
  stage('st-plan', 1, 'plan', 'succeeded'),
  stage('st-impl', 2, 'implement', 'succeeded'),
  stage('st-acc', 3, 'acceptance', 'succeeded'),
  stage('st-rev', 4, 'review', 'awaiting_approval'),
];

let seq = 0;
function entry(category: string, payload: unknown, stage_id: string | null = null): AuditEntry {
  seq++;
  return {
    id: `e${seq}`,
    sequence: seq,
    run_id: RUN,
    stage_id,
    ts: 'T',
    category,
    actor_kind: 'user',
    actor_subject: 'github:octo',
    payload,
    prev_hash: null,
    entry_hash: `hash-${seq}`,
  };
}

const planArtifact: Artifact = {
  id: 'art-plan',
  stage_id: 'st-plan',
  kind: 'plan',
  schema_version: 'standard_v1',
  content_hash: 'sha256:plan',
  created_at: 'T',
};
const planContent = {
  plan_version: 'standard_v1',
  ticket_reference: { type: 'github_issue', url: 'u', id: '1' },
  generated_by: { agent: 'a', model: 'm', timestamp: 't' },
  summary: 'Do it',
  scope: {
    files: [
      { path: 'a.ts', operation: 'modify' },
      { path: 'b.ts', operation: 'create' },
    ],
  },
  approach: [{ step: 1, description: 'x' }],
  verification: {
    test_strategy: 't',
    rollback_plan: 'r',
    acceptance_criteria: [
      { id: 'c-one', statement: 'one', source: 'explicit' },
      { id: 'c-two', statement: 'two', source: 'explicit' },
    ],
  },
};
const acceptanceArtifact: Artifact = {
  id: 'art-acc',
  stage_id: 'st-acc',
  kind: 'acceptance',
  schema_version: null,
  content_hash: 'sha256:acc',
  created_at: 'T',
  // Historical object-keyed criteria: normalized before any guard runs.
  content: { verdict: 'passed', criteria: { 'c-one': { result: 'passed' } } },
};

let audit: Record<string, AuditEntry[]>;
let gateView: GateView;

function resetFixture() {
  seq = 0;
  audit = {
    plan_reviewed: [
      entry(
        'plan_reviewed',
        {
          reviewer_kind: 'agent',
          reviewer_model: 'model-a',
          authority: 'advisory',
          verdict: 'approve_with_concerns',
          concerns: [{ severity: 'high', category: 'correctness', note: 'n1' }],
        },
        'st-plan',
      ),
    ],
    implement_reviewed: [],
    policy_evaluated: [
      entry(
        'policy_evaluated',
        {
          diff: [
            { path: 'a.ts', status: 'M' },
            { path: 'z.ts', status: 'A' },
          ],
        },
        'st-impl',
      ),
    ],
    acceptance_outcome_recorded: [
      entry(
        'acceptance_outcome_recorded',
        {
          artifact_id: 'art-acc',
          content_hash: 'sha256:acc',
          verdict: 'passed',
          criteria_passed: 1,
          criteria_total: 1,
        },
        'st-acc',
      ),
    ],
    approval_submitted: [
      entry(
        'approval_submitted',
        { decision: 'approve', identity: { provider: 'github', subject: 'github:octo' } },
        'st-plan',
      ),
    ],
    approval_predicate_rejected: [entry('approval_predicate_rejected', {}, 'st-plan')],
    pr_merged: [],
    merge_verdict_recorded: [],
    merge_observation_recorded: [],
    pr_closed_without_merge: [],
    run_auto_advanced: [entry('run_auto_advanced', {})],
    run_auto_driven: [entry('run_auto_driven', {})],
  };
  gateView = {
    run_id: RUN,
    open: [
      {
        id: 'c1',
        stage_kind: 'plan',
        origin_review_sequence: 1,
        reviewer_model: 'model-a',
        severity: 'high',
        category: 'correctness',
        state: 'raised',
        note: 'n1',
        has_suggested_patch: false,
      },
    ],
    settled: [],
    suppressed_relitigations: [],
    history_incomplete: false,
  };
}

function wireMocks() {
  m.getRun.mockResolvedValue(run);
  m.listRunStages.mockResolvedValue({ items: stages });
  m.getRunGateView.mockResolvedValue(gateView);
  m.listRunAudit.mockImplementation(async (_runId, params) => {
    const items = audit[params?.category ?? ''];
    if (!items) throw new Error(`unexpected category ${params?.category}`);
    return { items, next_cursor: null };
  });
  m.listStageArtifacts.mockImplementation(async (stageId) =>
    stageId === 'st-plan' ? { items: [planArtifact] } : { items: [] },
  );
  m.getArtifact.mockImplementation(async (id) => {
    if (id === 'art-plan') return { ...planArtifact, content: planContent };
    if (id === 'art-acc') return acceptanceArtifact;
    throw new Error(`no artifact ${id}`);
  });
}

beforeEach(() => {
  vi.resetAllMocks();
  resetFixture();
  wireMocks();
});

describe('loadRunNarrative — happy path', () => {
  it('derives every section with no degrade notes', async () => {
    const n = await loadRunNarrative(RUN);
    expect(n.degraded).toEqual([]);
    expect(n.run.id).toBe(RUN);
    expect(n.sections.plan.model?.summary).toBe('Do it');
    expect(n.sections.verdicts.model?.verdicts[0].concerns[0].lifecycle).toBe('raised');
    expect(n.sections.diff.model).toMatchObject({
      declaredOnly: ['b.ts'],
      stagedOnly: ['z.ts'],
      both: ['a.ts'],
    });
    // Acceptance keys off the outcome entry, not stage.type ('acceptance' is not a StageType).
    expect(n.sections.acceptance.model?.outcome?.verdict).toBe('passed');
    expect(n.sections.acceptance.model?.rows?.map((r) => [r.id, r.status])).toEqual([
      ['c-one', 'passed'],
      ['c-two', 'pending'],
    ]);
    expect(n.sections.acceptance.model?.artifact).toMatchObject({
      kind: 'artifact',
      stageId: 'st-acc',
    });
    expect(n.sections.approvals.model?.[0].identity).toEqual({
      provider: 'github',
      subject: 'github:octo',
    });
    expect(n.sections.merge.model).toEqual({ merge: null });
  });

  it('reads every classifyTransition category, including run_auto_driven and approval_predicate_rejected', async () => {
    const n = await loadRunNarrative(RUN);
    const categories = m.listRunAudit.mock.calls.map(([, p]) => p?.category);
    expect(categories).toEqual(
      expect.arrayContaining([
        'run_auto_driven',
        'approval_predicate_rejected',
        'run_auto_advanced',
      ]),
    );
    expect(new Set(categories)).toEqual(new Set(NARRATIVE_AUDIT_CATEGORIES));
    const tl = n.timeline.model!;
    expect(tl.stages.find((s) => s.stage.id === 'st-rev')?.treatment).toBe('gate');
    expect(
      tl.stages
        .find((s) => s.stage.id === 'st-plan')
        ?.transitions.map((t) => [t.category, t.treatment]),
    ).toEqual([
      ['approval_submitted', 'gate'],
      ['approval_predicate_rejected', 'gate'],
    ]);
    expect(tl.runTransitions.map((t) => [t.category, t.treatment])).toEqual([
      ['run_auto_advanced', 'auto_advance'],
      ['run_auto_driven', 'auto_advance'],
    ]);
  });

  it('bounds each category read to the server limit', async () => {
    await loadRunNarrative(RUN);
    for (const [, p] of m.listRunAudit.mock.calls) expect(p?.limit).toBe(500);
  });
});

describe('loadRunNarrative — per-section degrade', () => {
  it('rejects only when getRun rejects', async () => {
    m.getRun.mockRejectedValue(new Error('404 run'));
    await expect(loadRunNarrative(RUN)).rejects.toThrow('404 run');
  });

  it('a rejected gate view degrades ONLY the verdicts lifecycle, naming the read', async () => {
    m.getRunGateView.mockRejectedValue(new Error('gate_view_unconfigured'));
    const n = await loadRunNarrative(RUN);
    expect(n.degraded).toEqual([
      {
        section: 'verdicts',
        kind: 'read_failed',
        read: 'getRunGateView',
        message: 'concern lifecycle unavailable: gate_view_unconfigured',
      },
    ]);
    expect(n.sections.verdicts.model?.verdicts[0].concerns[0].lifecycle).toBe('unavailable');
    expect(n.sections.plan.model).not.toBeNull();
    expect(n.sections.acceptance.model?.rows).not.toBeNull();
    expect(n.sections.approvals.model).toHaveLength(1);
  });

  it('notes an incomplete gate-view history with its gaps', async () => {
    m.getRunGateView.mockResolvedValue({
      ...gateView,
      history_incomplete: true,
      history_gaps: ['stage_fixup_triggered'],
    });
    const n = await loadRunNarrative(RUN);
    expect(n.sections.verdicts.notes).toEqual([
      expect.objectContaining({
        kind: 'incomplete',
        message: 'concern history incomplete: stage_fixup_triggered',
      }),
    ]);
  });

  it('both review reads rejecting leaves the verdicts section unavailable', async () => {
    const base = m.listRunAudit.getMockImplementation()!;
    m.listRunAudit.mockImplementation(async (id, p) => {
      if (p?.category === 'plan_reviewed' || p?.category === 'implement_reviewed')
        throw new Error('403');
      return base(id, p);
    });
    const n = await loadRunNarrative(RUN);
    expect(n.sections.verdicts.model).toBeNull();
    expect(n.sections.verdicts.notes.map((x) => x.read)).toEqual([
      'audit:plan_reviewed',
      'audit:implement_reviewed',
    ]);
  });

  it('counts an undecodable review verdict', async () => {
    audit.implement_reviewed = [entry('implement_reviewed', 'garbage')];
    const n = await loadRunNarrative(RUN);
    expect(n.sections.verdicts.notes).toEqual([
      expect.objectContaining({
        kind: 'unreadable',
        message: '1 review verdict(s) could not be decoded',
      }),
    ]);
  });

  it('no plan stage → "no plan artifact" and "declared scope unavailable" while the staged column still renders', async () => {
    m.listRunStages.mockResolvedValue({ items: stages.filter((s) => s.type !== 'plan') });
    const n = await loadRunNarrative(RUN);
    expect(n.sections.plan.model).toBeNull();
    expect(n.sections.plan.notes[0]).toMatchObject({
      kind: 'absent',
      message: 'no plan artifact (run has no plan stage)',
    });
    expect(n.sections.diff.notes[0]).toMatchObject({ message: 'declared scope unavailable' });
    expect(n.sections.diff.model).toMatchObject({ declared: null, staged: ['a.ts', 'z.ts'] });
    expect(n.sections.acceptance.notes[0].message).toMatch(/criteria inventory unavailable/);
  });

  it('a plan stage with no plan artifact → "no plan artifact"', async () => {
    m.listStageArtifacts.mockResolvedValue({ items: [] });
    const n = await loadRunNarrative(RUN);
    expect(n.sections.plan.notes[0].message).toBe(
      'no plan artifact (plan stage has no plan artifact)',
    );
  });

  it('a non-standard_v1 plan artifact → unreadable', async () => {
    m.getArtifact.mockImplementation(async (id) =>
      id === 'art-plan'
        ? { ...planArtifact, content: { plan_version: 'standard_v2' } }
        : acceptanceArtifact,
    );
    const n = await loadRunNarrative(RUN);
    expect(n.sections.plan.notes[0]).toMatchObject({ kind: 'unreadable' });
  });

  it('a rejected artifacts read → plan read_failed', async () => {
    m.listStageArtifacts.mockRejectedValue(new Error('boom'));
    const n = await loadRunNarrative(RUN);
    expect(n.sections.plan.notes[0]).toMatchObject({
      kind: 'read_failed',
      message: 'no plan artifact: boom',
    });
  });

  it('a rejected stages read degrades the timeline and the plan, not the page', async () => {
    m.listRunStages.mockRejectedValue(new Error('stages down'));
    const n = await loadRunNarrative(RUN);
    expect(n.stages).toEqual([]);
    expect(n.timeline.model).toBeNull();
    expect(n.timeline.notes[0]).toMatchObject({
      read: 'listRunStages',
      message: 'stages unavailable: stages down',
    });
    expect(n.sections.plan.notes[0].kind).toBe('read_failed');
    expect(n.sections.approvals.model).toHaveLength(1);
  });

  it('no policy_evaluated entry → "staged scope not yet evaluated" and the declared scope still lists', async () => {
    audit.policy_evaluated = [];
    const n = await loadRunNarrative(RUN);
    expect(n.sections.diff.notes).toEqual([
      expect.objectContaining({ kind: 'absent', message: 'staged scope not yet evaluated' }),
    ]);
    expect(n.sections.diff.model).toMatchObject({ declared: ['a.ts', 'b.ts'], staged: null });
  });

  it('an undecodable policy_evaluated payload → staged scope unreadable', async () => {
    audit.policy_evaluated = [entry('policy_evaluated', { passed: true })];
    const n = await loadRunNarrative(RUN);
    expect(n.sections.diff.notes[0]).toMatchObject({ kind: 'unreadable' });
  });

  it('a rejected policy_evaluated read → staged scope unavailable', async () => {
    const base = m.listRunAudit.getMockImplementation()!;
    m.listRunAudit.mockImplementation(async (id, p) => {
      if (p?.category === 'policy_evaluated') throw new Error('403');
      return base(id, p);
    });
    const n = await loadRunNarrative(RUN);
    expect(n.sections.diff.notes[0]).toMatchObject({
      kind: 'read_failed',
      message: 'staged scope unavailable: 403',
    });
  });

  it('acceptance artifact read rejecting keeps the recorded verdict + tallies, rows unavailable', async () => {
    m.getArtifact.mockImplementation(async (id) => {
      if (id === 'art-plan') return { ...planArtifact, content: planContent };
      throw new Error('artifact 500');
    });
    const n = await loadRunNarrative(RUN);
    const acc = n.sections.acceptance;
    expect(acc.model?.outcome?.verdict).toBe('passed');
    expect(acc.model?.outcome?.tallies.total).toBe(1);
    expect(acc.model?.rows).toBeNull();
    expect(acc.notes[0]).toMatchObject({
      kind: 'read_failed',
      message: 'per-criterion evidence unavailable: artifact 500',
    });
  });

  it('an unreadable acceptance artifact (no verdict) → "unreadable acceptance artifact"', async () => {
    m.getArtifact.mockImplementation(async (id) => {
      if (id === 'art-plan') return { ...planArtifact, content: planContent };
      return { ...acceptanceArtifact, content: { pr_number: 1, pr_url: 'u' } };
    });
    const n = await loadRunNarrative(RUN);
    expect(n.sections.acceptance.model?.rows).toBeNull();
    expect(n.sections.acceptance.notes[0].message).toMatch(/^unreadable acceptance artifact/);
  });

  it('no acceptance outcome yet → every plan criterion pending, no artifact read', async () => {
    audit.acceptance_outcome_recorded = [];
    const n = await loadRunNarrative(RUN);
    expect(n.sections.acceptance.model?.outcome).toBeNull();
    expect(n.sections.acceptance.model?.rows?.map((r) => r.status)).toEqual(['pending', 'pending']);
    expect(m.getArtifact).not.toHaveBeenCalledWith('art-acc');
  });

  it('a rejected acceptance outcome read → acceptance unavailable', async () => {
    const base = m.listRunAudit.getMockImplementation()!;
    m.listRunAudit.mockImplementation(async (id, p) => {
      if (p?.category === 'acceptance_outcome_recorded') throw new Error('403');
      return base(id, p);
    });
    const n = await loadRunNarrative(RUN);
    expect(n.sections.acceptance.model).toBeNull();
    expect(n.sections.acceptance.notes[0].kind).toBe('read_failed');
  });

  it('a rejected approvals read → approvals unavailable', async () => {
    const base = m.listRunAudit.getMockImplementation()!;
    m.listRunAudit.mockImplementation(async (id, p) => {
      if (p?.category === 'approval_submitted') throw new Error('403');
      return base(id, p);
    });
    const n = await loadRunNarrative(RUN);
    expect(n.sections.approvals.model).toBeNull();
    expect(n.sections.approvals.notes[0].read).toBe('audit:approval_submitted');
  });

  it('merge: no entry of any category → an explicit not-merged model (merge: null), not a degrade', async () => {
    const n = await loadRunNarrative(RUN);
    expect(n.sections.merge).toEqual({ model: { merge: null }, notes: [] });
  });

  it('merge: one category rejecting is partial; all four rejecting is unavailable', async () => {
    audit.pr_merged = [entry('pr_merged', { pr_url: 'u', merger: 'octo', head_sha: 'abc' })];
    const base = m.listRunAudit.getMockImplementation()!;
    m.listRunAudit.mockImplementation(async (id, p) => {
      if (p?.category === 'merge_observation_recorded') throw new Error('403');
      return base(id, p);
    });
    let n = await loadRunNarrative(RUN);
    expect(n.sections.merge.model?.merge?.outcome?.kind).toBe('merged');
    expect(n.sections.merge.notes.map((x) => x.read)).toEqual(['audit:merge_observation_recorded']);

    m.listRunAudit.mockImplementation(async (id, p) => {
      if (
        [
          'pr_merged',
          'merge_verdict_recorded',
          'merge_observation_recorded',
          'pr_closed_without_merge',
        ].includes(p?.category ?? '')
      ) {
        throw new Error('403');
      }
      return base(id, p);
    });
    n = await loadRunNarrative(RUN);
    expect(n.sections.merge.model).toBeNull();
  });

  it('a rejected transition category degrades the timeline with a note but keeps the stages', async () => {
    const base = m.listRunAudit.getMockImplementation()!;
    m.listRunAudit.mockImplementation(async (id, p) => {
      if (p?.category === 'run_auto_driven') throw new Error('403');
      return base(id, p);
    });
    const n = await loadRunNarrative(RUN);
    expect(n.timeline.model?.stages).toHaveLength(4);
    expect(n.timeline.notes.map((x) => x.read)).toEqual(['audit:run_auto_driven']);
  });

  it('paginates a category until next_cursor is null, and notes truncation at the page bound', async () => {
    const base = m.listRunAudit.getMockImplementation()!;
    let approvalPages = 0;
    m.listRunAudit.mockImplementation(async (id, p): Promise<PaginatedList<AuditEntry>> => {
      if (p?.category === 'approval_submitted') {
        approvalPages++;
        return {
          items: [entry('approval_submitted', { decision: 'approve' })],
          next_cursor: 'more',
        };
      }
      if (p?.category === 'run_auto_advanced' && !p.cursor) {
        return { items: [entry('run_auto_advanced', {})], next_cursor: 'c2' };
      }
      return base(id, p);
    });
    const n = await loadRunNarrative(RUN);
    expect(approvalPages).toBe(AUDIT_MAX_PAGES);
    expect(n.sections.approvals.model).toHaveLength(AUDIT_MAX_PAGES);
    expect(n.sections.approvals.notes).toEqual([
      expect.objectContaining({ kind: 'incomplete', read: 'audit:approval_submitted' }),
    ]);
    // run_auto_advanced took two pages (cursor c2 → the base fixture page) and is NOT truncated.
    expect(m.listRunAudit).toHaveBeenCalledWith(
      RUN,
      expect.objectContaining({ category: 'run_auto_advanced', cursor: 'c2' }),
    );
    expect(n.timeline.notes.filter((x) => x.read === 'audit:run_auto_advanced')).toEqual([]);
  });
});

describe('loadAuditEntry (approval condition 1)', () => {
  it('resolves entry N via since_sequence=N-1&limit=1, independent of pagination', async () => {
    const target = { ...entry('approval_submitted', {}), sequence: 742, entry_hash: 'hash-742' };
    m.listRunAudit.mockResolvedValue({ items: [target], next_cursor: 'x' });
    await expect(loadAuditEntry(RUN, 742)).resolves.toBe(target);
    expect(m.listRunAudit).toHaveBeenCalledWith(RUN, { sinceSequence: 741, limit: 1 });
  });

  it('returns null ("entry not found") when nothing comes back', async () => {
    m.listRunAudit.mockResolvedValue({ items: [], next_cursor: null });
    await expect(loadAuditEntry(RUN, 9999)).resolves.toBeNull();
  });

  it('returns null when the next entry after N-1 is NOT N (a gap)', async () => {
    m.listRunAudit.mockResolvedValue({
      items: [{ ...entry('x', {}), sequence: 743 }],
      next_cursor: null,
    });
    await expect(loadAuditEntry(RUN, 742)).resolves.toBeNull();
  });
});

function NarrativeProbe({ runId }: { runId: string }) {
  const s = useRunNarrative(runId);
  if (s.status === 'loading') return <p>loading</p>;
  if (s.status === 'error') return <p>error: {s.error.message}</p>;
  return (
    <p>
      ok {s.data.run.id} notes={s.data.degraded.length}
    </p>
  );
}

function EntryProbe({ sequence }: { sequence: number | null }) {
  const s = useAuditEntry(RUN, sequence);
  if (s.status !== 'ok') return <p>{s.status}</p>;
  return <p>{s.data ? `entry ${s.data.entry_hash}` : 'entry not found'}</p>;
}

describe('hooks', () => {
  it('useRunNarrative loads through the mocked api module', async () => {
    render(<NarrativeProbe runId={RUN} />);
    await waitFor(() => expect(screen.getByText(`ok ${RUN} notes=0`)).toBeInTheDocument());
  });

  it('useRunNarrative surfaces a getRun failure as the page error', async () => {
    m.getRun.mockRejectedValue(new Error('gone'));
    render(<NarrativeProbe runId={RUN} />);
    await waitFor(() => expect(screen.getByText('error: gone')).toBeInTheDocument());
  });

  it('useAuditEntry resolves a sequence and skips the read for null', async () => {
    m.listRunAudit.mockResolvedValue({
      items: [{ ...entry('x', {}), sequence: 5, entry_hash: 'hash-five' }],
      next_cursor: null,
    });
    const { rerender } = render(<EntryProbe sequence={5} />);
    await waitFor(() => expect(screen.getByText('entry hash-five')).toBeInTheDocument());
    m.listRunAudit.mockClear();
    rerender(<EntryProbe sequence={null} />);
    await waitFor(() => expect(screen.getByText('entry not found')).toBeInTheDocument());
    expect(m.listRunAudit).not.toHaveBeenCalled();
  });
});
