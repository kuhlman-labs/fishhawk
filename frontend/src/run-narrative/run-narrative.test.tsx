import { describe, expect, it } from 'vitest';
import { render, screen, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import type { AuditEntry, Run, Stage } from '@/api/types';
import { deriveGateTimeline } from './narrative';
import { RunNarrativeView } from './run-narrative';
import type { RunNarrative } from './use-run-narrative';

const RUN = 'run-1';

function stage(id: string, sequence: number, type: string, state: Stage['state']): Stage {
  return {
    id,
    run_id: RUN,
    sequence,
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

function entry(sequence: number, category: string, stage_id: string | null): AuditEntry {
  return {
    id: `e${sequence}`,
    sequence,
    run_id: RUN,
    stage_id,
    ts: 'T',
    category,
    actor_kind: 'system',
    actor_subject: null,
    payload: {},
    prev_hash: null,
    entry_hash: `hash-${sequence}`,
  };
}

const stages = [
  stage('st-plan', 1, 'plan', 'succeeded'),
  stage('st-impl', 2, 'implement', 'succeeded'),
  stage('st-rev', 3, 'review', 'awaiting_approval'),
];

function narrative(): RunNarrative {
  const timeline = deriveGateTimeline(stages, [
    entry(5, 'approval_submitted', 'st-plan'),
    entry(6, 'approval_predicate_rejected', 'st-plan'),
    entry(7, 'run_auto_advanced', 'st-impl'),
    entry(8, 'run_auto_driven', null),
  ]);
  return {
    run: { id: RUN } as Run,
    stages,
    sections: {
      plan: {
        model: null,
        notes: [{ section: 'plan', kind: 'absent', read: 'p', message: 'no plan artifact' }],
      },
      verdicts: { model: { verdicts: [] }, notes: [] },
      diff: {
        model: { declared: null, staged: null, declaredOnly: [], stagedOnly: [], both: [] },
        notes: [],
      },
      acceptance: { model: { outcome: null, rows: [], body: null, artifact: null }, notes: [] },
      approvals: { model: [], notes: [] },
      merge: { model: { merge: null }, notes: [] },
    },
    timeline: { model: timeline, notes: [] },
    degraded: [],
  };
}

function renderView() {
  return render(
    <MemoryRouter>
      <RunNarrativeView narrative={narrative()} stagedEvidence={null} />
    </MemoryRouter>,
  );
}

describe('<RunNarrativeView>', () => {
  it('renders the six sections in the mandated order, then the gate timeline', () => {
    renderView();
    const sections = Array.from(document.querySelectorAll('[data-testid="narrative-section"]')).map(
      (el) => el.getAttribute('data-section'),
    );
    expect(sections).toEqual(['plan', 'verdicts', 'diff', 'acceptance', 'approvals', 'merge']);
    const headings = screen.getAllByRole('heading', { level: 2 }).map((h) => h.textContent);
    expect(headings).toEqual([
      'Plan',
      'Advisory verdicts',
      'Diff summary',
      'Acceptance',
      'Approvals',
      'Merge',
      'Gate timeline',
    ]);
  });

  it('composed timeline: gate, auto-advance and a parked stage each get their treatment', () => {
    renderView();
    const treatment = (category: string) =>
      document
        .querySelector(`[data-testid="timeline-transition"][data-category="${category}"]`)
        ?.querySelector('[data-testid="transition-marker"]')
        ?.getAttribute('data-treatment');
    expect(treatment('approval_submitted')).toBe('gate');
    expect(treatment('approval_predicate_rejected')).toBe('gate');
    expect(treatment('run_auto_advanced')).toBe('auto_advance');
    // A run-level (no stage) auto-drive renders under run-level transitions.
    expect(treatment('run_auto_driven')).toBe('auto_advance');
    expect(screen.getByText('Run-level transitions')).toBeInTheDocument();

    const parked = document.querySelector('[data-stage-id="st-rev"]') as HTMLElement;
    expect(parked).toHaveAttribute('data-treatment', 'gate');
    expect(within(parked).getByTestId('transition-marker')).toHaveAttribute(
      'data-treatment',
      'gate',
    );
    const impl = document.querySelector('[data-stage-id="st-impl"]') as HTMLElement;
    expect(impl).toHaveAttribute('data-treatment', 'other');
    expect(within(impl).queryByText('Parked at gate')).not.toBeInTheDocument();
    // Stage deep links are preserved.
    expect(screen.getByRole('link', { name: 'Review review stage' })).toHaveAttribute(
      'href',
      `/runs/${RUN}/stages/st-rev`,
    );
  });

  it('a failed stages read renders the timeline note instead of the list', () => {
    const n = narrative();
    n.timeline = {
      model: null,
      notes: [
        {
          section: 'timeline',
          kind: 'read_failed',
          read: 'listRunStages',
          message: 'stages unavailable: x',
        },
      ],
    };
    render(
      <MemoryRouter>
        <RunNarrativeView narrative={n} stagedEvidence={null} />
      </MemoryRouter>,
    );
    expect(screen.getByText('stages unavailable: x')).toBeInTheDocument();
    expect(screen.queryByTestId('timeline-stage')).not.toBeInTheDocument();
  });
});
