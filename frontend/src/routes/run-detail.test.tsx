import { beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { MemoryRouter, Route, Routes } from 'react-router';
import type { Artifact, AuditEntry, GateView, Run, Stage } from '@/api/types';
import type * as ClientModuleNs from '@/api/client';

type ClientModule = typeof ClientModuleNs;

/*
 * Cross-layer integration test for the run-detail evidence narrative
 * (#1715): mounts the REAL <RunDetail> route with the whole @/api/client
 * module mocked, so the API client seam, the loader, the pure derivations,
 * the section components and the route composition are exercised together.
 * Per-layer unit tests alone would pass while this seam breaks.
 */
vi.mock('@/api/client', async (importOriginal) => {
  const actual = await importOriginal<ClientModule>();
  return {
    ...actual,
    api: {
      getRun: vi.fn(),
      listRunStages: vi.fn(),
      getRunGateView: vi.fn(),
      listRunAudit: vi.fn(),
      listStageArtifacts: vi.fn(),
      getArtifact: vi.fn(),
      listRuns: vi.fn(),
    },
  };
});

import { ApiClientError, api } from '@/api/client';
import { RunDetail } from './run-detail';

const m = vi.mocked(api);
const RUN = 'rrrrrrrr-0000-0000-0000-000000000001';
const MANDATED_ORDER = [
  'Plan',
  'Advisory verdicts',
  'Diff summary',
  'Acceptance',
  'Approvals',
  'Merge',
];

const run: Run = {
  id: RUN,
  repo: 'kuhlman-labs/fishhawk',
  workflow_id: 'feature_change',
  workflow_sha: 'wfsha',
  trigger_source: 'github_issue',
  trigger_ref: 'issue:1715',
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
    // 'acceptance' is absent from the StageType union; the narrative must not depend on it.
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

function entry(
  sequence: number,
  category: string,
  payload: unknown,
  stage_id: string | null = null,
): AuditEntry {
  return {
    id: `e${sequence}`,
    sequence,
    run_id: RUN,
    stage_id,
    ts: '2026-09-28T00:00:00Z',
    category,
    actor_kind: 'user',
    actor_subject: 'github:octo',
    payload,
    prev_hash: null,
    entry_hash: `hash${sequence}-0123456789abcdef0123456789abcdef`,
  };
}

const planArtifact: Artifact = {
  id: 'art-plan',
  stage_id: 'st-plan',
  kind: 'plan',
  schema_version: 'standard_v1',
  content_hash: 'sha256:planplanplanplan',
  created_at: 'T',
};
const planContent = {
  plan_version: 'standard_v1',
  ticket_reference: { type: 'github_issue', url: 'https://example.test/1715', id: '#1715' },
  generated_by: { agent: 'a', model: 'm', timestamp: 't' },
  summary: 'Restructure run detail',
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
      { id: 'c-one', statement: 'first criterion', source: 'explicit' },
      { id: 'c-two', statement: 'second criterion', source: 'explicit' },
    ],
  },
};

let stages: Stage[];
let audit: Record<string, AuditEntry[]>;
let gateView: GateView;
let artifacts: Record<string, Artifact | Error>;
let planStageArtifacts: Artifact[];

function resetFixture() {
  stages = [
    stage('st-plan', 1, 'plan', 'succeeded'),
    stage('st-impl', 2, 'implement', 'succeeded'),
    stage('st-acc', 3, 'acceptance', 'succeeded'),
    stage('st-rev', 4, 'review', 'awaiting_approval'),
  ];
  planStageArtifacts = [planArtifact];
  audit = {
    plan_reviewed: [
      entry(
        3,
        'plan_reviewed',
        {
          reviewer_kind: 'agent',
          reviewer_model: 'model-a',
          authority: 'advisory',
          verdict: 'approve_with_concerns',
          concerns: [{ severity: 'high', category: 'correctness', note: 'check the join' }],
        },
        'st-plan',
      ),
    ],
    policy_evaluated: [
      entry(
        60,
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
        90,
        'acceptance_outcome_recorded',
        {
          artifact_id: 'art-acc',
          content_hash: 'sha256:accaccaccacc',
          verdict: 'passed',
          criteria_passed: 1,
          criteria_failed: 0,
          criteria_skipped: 0,
          criteria_undecidable: 0,
          criteria_total: 1,
        },
        'st-acc',
      ),
    ],
    approval_submitted: [
      // Sequence 120: well past the #audit list's first page (50 entries).
      entry(
        120,
        'approval_submitted',
        {
          decision: 'approve',
          identity: { provider: 'github', subject: 'github:octo' },
          auth_method: 'session',
          channel: 'web',
        },
        'st-plan',
      ),
    ],
    approval_predicate_rejected: [entry(4, 'approval_predicate_rejected', {}, 'st-plan')],
    // The entries the gate view's fix-up/resolution rows name. The narrative
    // never reads these categories; they exist so following a history link
    // resolves a real entry.
    stage_fixup_triggered: [entry(12, 'stage_fixup_triggered', { reason: 'r' }, 'st-impl')],
    concern_resolution_recorded: [
      entry(13, 'concern_resolution_recorded', { resolution: 'claimed_addressed' }, 'st-impl'),
    ],
    run_auto_advanced: [entry(61, 'run_auto_advanced', {}, 'st-impl')],
    run_auto_driven: [entry(62, 'run_auto_driven', {})],
    pr_merged: [
      entry(130, 'pr_merged', {
        pr_url: 'https://github.com/kuhlman-labs/fishhawk/pull/9',
        head_sha: 'deadbeefcafe',
      }),
    ],
  };
  gateView = {
    run_id: RUN,
    open: [
      {
        id: 'c1',
        stage_kind: 'plan',
        origin_review_sequence: 3,
        reviewer_model: 'model-a',
        severity: 'high',
        category: 'correctness',
        state: 'waived',
        note: 'check the join',
        has_suggested_patch: false,
        // Later events than the raising review (#3): each resolves its own entry.
        fixups: [{ sequence: 12, outcome: 'pushed', head_sha: 'abcdef0123456789' }],
        resolutions: [{ sequence: 13, resolution: 'claimed_addressed' }],
      },
    ],
    settled: [],
    suppressed_relitigations: [],
    history_incomplete: false,
  };
  artifacts = {
    'art-plan': { ...planArtifact, content: planContent },
    'art-acc': {
      id: 'art-acc',
      stage_id: 'st-acc',
      kind: 'acceptance',
      schema_version: null,
      content_hash: 'sha256:accaccaccacc',
      created_at: 'T',
      // Historical object-keyed criteria (condition 2); c-two is unrecorded.
      content: { verdict: 'passed', criteria: { 'c-one': { result: 'passed' } } },
    },
  };
}

function allEntries(): AuditEntry[] {
  return Object.values(audit)
    .flat()
    .sort((a, b) => a.sequence - b.sequence);
}

function wireMocks() {
  m.getRun.mockResolvedValue(run);
  m.listRunStages.mockImplementation(async () => ({ items: stages }));
  m.getRunGateView.mockImplementation(async () => gateView);
  m.listRuns.mockResolvedValue({ items: [], next_cursor: null });
  m.listRunAudit.mockImplementation(async (_runId, params) => {
    if (params?.category) return { items: audit[params.category] ?? [], next_cursor: null };
    if (params?.sinceSequence !== undefined) {
      const hit = allEntries().find((e) => e.sequence > params.sinceSequence!);
      return { items: hit ? [hit] : [], next_cursor: null };
    }
    // The raw #audit list: its first page stops at sequence 50.
    const page = Array.from({ length: 50 }, (_, i) => entry(i + 1, 'filler', {}));
    return { items: page, next_cursor: 'page-2' };
  });
  m.listStageArtifacts.mockImplementation(async (stageId) =>
    stageId === 'st-plan' ? { items: planStageArtifacts } : { items: [] },
  );
  m.getArtifact.mockImplementation(async (id) => {
    const a = artifacts[id];
    if (!a) throw new Error(`no artifact ${id}`);
    if (a instanceof Error) throw a;
    return a;
  });
}

beforeEach(() => {
  vi.resetAllMocks();
  resetFixture();
  wireMocks();
});

function renderRoute(path = `/runs/${RUN}`) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <Routes>
        <Route path="runs/:runId" element={<RunDetail />} />
      </Routes>
    </MemoryRouter>,
  );
}

async function renderLoaded(path?: string) {
  const view = renderRoute(path);
  await screen.findByRole('heading', { name: 'Merge' });
  return view;
}

function sectionEl(id: string): HTMLElement {
  const el = document.querySelector<HTMLElement>(`[data-section="${id}"]`);
  if (!el) throw new Error(`section ${id} not rendered`);
  return el;
}

function expectAllSixSections() {
  for (const title of MANDATED_ORDER) {
    expect(screen.getByRole('heading', { name: title })).toBeInTheDocument();
  }
}

describe('<RunDetail> evidence narrative', () => {
  it('renders the six sections in the mandated DOM order, then the timeline and #audit', async () => {
    await renderLoaded();
    const names = screen.getAllByRole('heading').map((h) => h.textContent ?? '');
    const six = names.filter((n) => MANDATED_ORDER.includes(n));
    expect(six).toEqual(MANDATED_ORDER);
    // Timeline follows the six sections; the raw audit list stays last.
    expect(names.indexOf('Gate timeline')).toBeGreaterThan(names.indexOf('Merge'));
    expect(names.indexOf('Audit log')).toBeGreaterThan(names.indexOf('Gate timeline'));
    // Header survives.
    expect(screen.getByRole('heading', { name: 'kuhlman-labs/fishhawk' })).toBeInTheDocument();
    // Each section rendered its body.
    expect(within(sectionEl('plan')).getByText('Restructure run detail')).toBeInTheDocument();
    expect(within(sectionEl('verdicts')).getByTestId('concern-lifecycle')).toHaveAttribute(
      'data-lifecycle',
      'waived',
    );
    expect(within(sectionEl('merge')).getByTestId('merge-state')).toHaveAttribute(
      'data-state',
      'merged',
    );
    // #audit raw list is mounted and unchanged.
    const auditBlock = document.getElementById('audit');
    expect(auditBlock).not.toBeNull();
    expect(await within(auditBlock!).findByText('#50')).toBeInTheDocument();
  });

  it('gives gates and auto-advances distinct treatments, including a parked stage by STATE', async () => {
    await renderLoaded();
    const byCategory = (c: string) =>
      document.querySelector(`[data-testid="timeline-transition"][data-category="${c}"]`);
    const marker = (c: string) =>
      byCategory(c)
        ?.querySelector('[data-testid="transition-marker"]')
        ?.getAttribute('data-treatment');
    expect(marker('approval_submitted')).toBe('gate');
    expect(marker('approval_predicate_rejected')).toBe('gate');
    expect(marker('acceptance_outcome_recorded')).toBe('gate');
    expect(marker('run_auto_advanced')).toBe('auto_advance');
    expect(marker('run_auto_driven')).toBe('auto_advance');
    // The review stage is parked at awaiting_approval with NO audit entry: gate from state.
    const parked = document.querySelector('[data-testid="timeline-stage"][data-stage-id="st-rev"]');
    expect(parked).toHaveAttribute('data-treatment', 'gate');
    expect(within(parked as HTMLElement).getByText('Parked at gate')).toBeInTheDocument();
    const done = document.querySelector('[data-testid="timeline-stage"][data-stage-id="st-impl"]');
    expect(done).toHaveAttribute('data-treatment', 'other');
    // Stage deep links survive inside the timeline.
    expect(screen.getByRole('link', { name: 'Review plan stage' })).toHaveAttribute(
      'href',
      `/runs/${RUN}/stages/st-plan`,
    );
  });

  it('renders object-keyed acceptance criteria through the real route, pending for an unrecorded plan criterion', async () => {
    await renderLoaded();
    const rows = within(sectionEl('acceptance')).getAllByTestId('acceptance-row');
    expect(
      rows.map((r) => [r.getAttribute('data-criterion'), r.getAttribute('data-status')]),
    ).toEqual([
      ['c-one', 'passed'],
      ['c-two', 'pending'],
    ]);
    expect(within(rows[1]).getByTestId('acceptance-row-verdict')).toHaveTextContent('pending');
  });

  it('sweeps every evidence link: entry-resolving or artifact-backed, never a bare #audit', async () => {
    await renderLoaded();
    const links = screen.getAllByTestId('evidence-link');
    expect(links.length).toBeGreaterThanOrEqual(8);
    let auditLinks = 0;
    for (const l of links) {
      const href = l.getAttribute('href') ?? '';
      if (l.getAttribute('data-evidence-kind') === 'audit') {
        auditLinks++;
        const m2 = /^\/runs\/[^/?#]+\?entry=(\d+)#entry-(\d+)$/.exec(href);
        expect(m2, href).not.toBeNull();
        expect(m2![1]).toBe(m2![2]);
      } else {
        expect(href).toMatch(/^\/runs\/[^/?#]+\/stages\/[^/?#]+$/);
      }
      expect(href.endsWith('#audit')).toBe(false);
    }
    expect(auditLinks).toBeGreaterThan(0);
    // The staged column links to the exact policy_evaluated entry it came from.
    const staged = within(sectionEl('diff')).getByTestId('staged-scope');
    expect(within(staged).getByTestId('evidence-link')).toHaveAttribute(
      'href',
      `/runs/${RUN}?entry=60#entry-60`,
    );
  });

  it('following an evidence link to an entry beyond the first audit page renders that entry', async () => {
    await renderLoaded();
    expect(screen.queryByTestId('evidence-panel')).not.toBeInTheDocument();
    const approvalLink = within(sectionEl('approvals')).getByTestId('evidence-link');
    fireEvent.click(approvalLink);
    const panel = await screen.findByTestId('evidence-panel');
    await waitFor(() =>
      expect(within(panel).getByTestId('evidence-entry-hash')).toHaveTextContent(
        'hash120-0123456789abcdef0123456789abcdef',
      ),
    );
    expect(within(panel).getByText('approval_submitted')).toBeInTheDocument();
    expect(panel).toHaveAttribute('id', 'entry-120');
    expect(m.listRunAudit).toHaveBeenCalledWith(RUN, { sinceSequence: 119, limit: 1 });
  });

  it('following a concern fix-up history link resolves THAT entry, not the review that raised it', async () => {
    await renderLoaded();
    const concern = within(sectionEl('verdicts')).getByTestId('verdict-concern');
    const fixupLink = within(within(concern).getByTestId('concern-fixup')).getByTestId(
      'evidence-link',
    );
    expect(fixupLink).toHaveAttribute('href', `/runs/${RUN}?entry=12#entry-12`);
    fireEvent.click(fixupLink);
    let panel = await screen.findByTestId('evidence-panel');
    await waitFor(() =>
      expect(within(panel).getByTestId('evidence-entry-hash')).toHaveTextContent(
        'hash12-0123456789abcdef0123456789abcdef',
      ),
    );
    expect(within(panel).getByText('stage_fixup_triggered')).toBeInTheDocument();
    expect(m.listRunAudit).toHaveBeenCalledWith(RUN, { sinceSequence: 11, limit: 1 });

    const resolutionLink = within(within(concern).getByTestId('concern-resolution')).getByTestId(
      'evidence-link',
    );
    expect(resolutionLink).toHaveAttribute('href', `/runs/${RUN}?entry=13#entry-13`);
    fireEvent.click(resolutionLink);
    panel = await screen.findByTestId('evidence-panel');
    await waitFor(() =>
      expect(within(panel).getByTestId('evidence-entry-hash')).toHaveTextContent(
        'hash13-0123456789abcdef0123456789abcdef',
      ),
    );
    expect(within(panel).getByText('concern_resolution_recorded')).toBeInTheDocument();
  });

  it('renders a named "entry not found" state for a sequence with no entry', async () => {
    await renderLoaded(`/runs/${RUN}?entry=999`);
    expect(await screen.findByTestId('evidence-entry-not-found')).toHaveTextContent(
      'Audit entry #999 not found',
    );
  });
});

describe('<RunDetail> untrusted-value and unavailable-evidence handling', () => {
  it('a failed merge read with no terminal outcome renders indeterminate, never "Not merged"', async () => {
    audit.pr_merged = [];
    const base = m.listRunAudit.getMockImplementation()!;
    m.listRunAudit.mockImplementation(async (id, p) => {
      if (p?.category === 'pr_merged') throw new ApiClientError(403, {}, 'forbidden');
      return base(id, p);
    });
    await renderLoaded();
    const merge = sectionEl('merge');
    const state = within(merge).getByTestId('merge-state');
    expect(state).toHaveAttribute('data-state', 'indeterminate');
    expect(state).not.toHaveTextContent('Not merged');
    expect(within(merge).getByRole('note')).toHaveTextContent('pr_merged unavailable');
    expectAllSixSections();
  });

  it('never renders a javascript: href from an agent-authored plan or an audit payload', async () => {
    const hostile = 'javascript:alert(1)';
    artifacts['art-plan'] = {
      ...planArtifact,
      content: {
        ...planContent,
        ticket_reference: { type: 'github_issue', url: hostile, id: '#1715' },
      },
    };
    audit.pr_merged = [entry(130, 'pr_merged', { pr_url: hostile, head_sha: 'deadbeefcafe' })];
    await renderLoaded();
    for (const a of document.querySelectorAll('a')) {
      expect(a.getAttribute('href') ?? '').not.toMatch(/^\s*javascript:/i);
    }
    // The values are still visible to the operator, as inert text.
    expect(within(sectionEl('plan')).getByText('#1715')).toBeInTheDocument();
    expect(within(sectionEl('merge')).getByTestId('merge-pr-unlinked')).toHaveTextContent(hostile);
  });
});

describe('<RunDetail> degrade modes (one section each, never the page)', () => {
  it('(1) gate view 503 → verdicts note "concern lifecycle unavailable", other sections render', async () => {
    m.getRunGateView.mockRejectedValue(
      new ApiClientError(503, { error: 'gate_view_unconfigured' }, 'gate view unconfigured'),
    );
    await renderLoaded();
    const verdicts = sectionEl('verdicts');
    expect(within(verdicts).getByRole('note')).toHaveTextContent(/concern lifecycle unavailable/);
    // The verdict itself still renders, its concern marked lifecycle-unavailable.
    expect(within(verdicts).getByTestId('verdict-value')).toHaveTextContent(
      'approve_with_concerns',
    );
    expect(within(verdicts).getByTestId('concern-lifecycle')).toHaveAttribute(
      'data-lifecycle',
      'unavailable',
    );
    expectAllSixSections();
    expect(within(sectionEl('plan')).getByText('Restructure run detail')).toBeInTheDocument();
    expect(within(sectionEl('acceptance')).getByTestId('acceptance-verdict')).toHaveTextContent(
      'passed',
    );
    expect(within(sectionEl('approvals')).getByTestId('approval')).toBeInTheDocument();
    expect(within(sectionEl('merge')).getByTestId('merge-state')).toHaveAttribute(
      'data-state',
      'merged',
    );
  });

  it('(2) no plan artifact → plan "no plan artifact", diff "declared scope unavailable", staged column still renders', async () => {
    planStageArtifacts = [];
    await renderLoaded();
    expect(within(sectionEl('plan')).getByRole('note')).toHaveTextContent(/no plan artifact/);
    const diff = sectionEl('diff');
    expect(within(diff).getByText('declared scope unavailable')).toBeInTheDocument();
    const staged = within(diff).getByTestId('staged-scope');
    expect(within(staged).getByText('z.ts')).toBeInTheDocument();
    expectAllSixSections();
  });

  it('(3) no policy_evaluated entry → "staged scope not yet evaluated", declared scope still listed', async () => {
    audit.policy_evaluated = [];
    await renderLoaded();
    const diff = sectionEl('diff');
    expect(within(diff).getByText('staged scope not yet evaluated')).toBeInTheDocument();
    const declared = within(diff).getByTestId('declared-scope');
    expect(within(declared).getByText('a.ts')).toBeInTheDocument();
    expect(within(declared).getByText('b.ts')).toBeInTheDocument();
    expectAllSixSections();
  });

  it('(4) acceptance artifact read fails → recorded verdict and tallies still render, per-criterion note', async () => {
    artifacts['art-acc'] = new Error('artifact store down');
    await renderLoaded();
    const acc = sectionEl('acceptance');
    expect(within(acc).getByTestId('acceptance-verdict')).toHaveTextContent('passed');
    expect(within(acc).getByTestId('acceptance-tallies')).toHaveTextContent('1 passed');
    expect(within(acc).getByTestId('acceptance-tallies')).toHaveTextContent('1 total');
    expect(within(acc).getByRole('note')).toHaveTextContent(/per-criterion evidence unavailable/);
    expect(within(acc).queryAllByTestId('acceptance-row')).toHaveLength(0);
  });

  it('(5) approval without auth_method/channel → identity only, no Auth method / Channel rows', async () => {
    audit.approval_submitted = [
      entry(
        120,
        'approval_submitted',
        { decision: 'approve', identity: { provider: 'github', subject: 'github:octo' } },
        'st-plan',
      ),
    ];
    await renderLoaded();
    const approvals = sectionEl('approvals');
    expect(within(approvals).getByText('github · github:octo')).toBeInTheDocument();
    expect(within(approvals).queryByText('Auth method')).not.toBeInTheDocument();
    expect(within(approvals).queryByText('Channel')).not.toBeInTheDocument();
  });

  it('(5, present) approval with auth_method/channel → both rows with values', async () => {
    await renderLoaded();
    const approvals = sectionEl('approvals');
    expect(within(approvals).getByText('Auth method')).toBeInTheDocument();
    expect(within(approvals).getByText('session')).toBeInTheDocument();
    expect(within(approvals).getByText('Channel')).toBeInTheDocument();
    expect(within(approvals).getByText('web')).toBeInTheDocument();
  });

  it('(6) no merge entry of any category → explicit not-merged state', async () => {
    delete audit.pr_merged;
    await renderLoaded();
    const state = within(sectionEl('merge')).getByTestId('merge-state');
    expect(state).toHaveAttribute('data-state', 'not_merged');
    expect(state).toHaveTextContent('Not merged');
  });

  it('getRun failing is the only page-level failure', async () => {
    m.getRun.mockRejectedValue(new Error('run gone'));
    renderRoute();
    expect(await screen.findByRole('alert')).toHaveTextContent("Couldn't load run.");
  });
});
