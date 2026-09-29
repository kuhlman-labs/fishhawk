import { describe, expect, it } from 'vitest';
import { render, screen, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import type { AuditEntry, GateView } from '@/api/types';
import { decodeReviewVerdicts, joinConcernLifecycle } from './narrative';
import { VerdictsBlock } from './verdicts-block';

function review(sequence: number, model: string, concerns: unknown[]): AuditEntry {
  return {
    id: `e${sequence}`,
    sequence,
    run_id: 'run-1',
    stage_id: 'st-impl',
    ts: 'T',
    category: 'implement_reviewed',
    actor_kind: 'agent',
    actor_subject: null,
    payload: {
      reviewer_kind: 'agent',
      reviewer_model: model,
      authority: 'advisory',
      verdict: concerns.length ? 'reject' : 'approve',
      concerns,
    },
    prev_hash: null,
    entry_hash: `hash-${sequence}`,
  };
}

const entries = [
  review(10, 'model-a', [
    { severity: 'high', category: 'correctness', note: 'off by one' },
    { severity: 'low', category: 'style', note: 'no ledger row' },
  ]),
  review(11, 'model-b', []),
];

const gateView: GateView = {
  run_id: 'run-1',
  open: [
    {
      id: 'c1',
      stage_kind: 'implement',
      origin_review_sequence: 10,
      reviewer_model: 'model-a',
      severity: 'high',
      category: 'correctness',
      state: 'addressed_pending',
      note: 'off by one',
      has_suggested_patch: false,
      fixups: [{ sequence: 12, outcome: 'pushed', head_sha: 'abcdef0123456789' }],
      resolutions: [{ sequence: 13, resolution: 'claimed_addressed' }],
      disputed: false,
    },
  ],
  settled: [],
  suppressed_relitigations: [],
  history_incomplete: false,
};

function renderBlock(
  gv: GateView | null,
  notes = [] as Parameters<typeof VerdictsBlock>[0]['section']['notes'],
) {
  const verdicts = joinConcernLifecycle(decodeReviewVerdicts(entries).rows, gv);
  return render(
    <MemoryRouter>
      <VerdictsBlock section={{ model: { verdicts }, notes }} />
    </MemoryRouter>,
  );
}

describe('<VerdictsBlock>', () => {
  it('renders one column per verdict with each concern lifecycle adjacent to its concern', () => {
    renderBlock(gateView);
    const columns = screen.getAllByTestId('review-verdict');
    expect(columns).toHaveLength(2);
    const concerns = within(columns[0]).getAllByTestId('verdict-concern');
    expect(within(concerns[0]).getByText('off by one')).toBeInTheDocument();
    expect(within(concerns[0]).getByTestId('concern-lifecycle')).toHaveAttribute(
      'data-lifecycle',
      'addressed_pending',
    );
    // Fix-up and resolution history from the gate-view join.
    const history = within(concerns[0]).getByTestId('concern-history');
    expect(history).toHaveTextContent('fix-up #12: pushed @ abcdef012345');
    expect(history).toHaveTextContent('resolution #13: claimed_addressed');
    // An unjoined concern is visible as `unmatched`, not dropped.
    expect(within(concerns[1]).getByTestId('concern-lifecycle')).toHaveAttribute(
      'data-lifecycle',
      'unmatched',
    );
    expect(within(columns[1]).getByText('No concerns raised.')).toBeInTheDocument();
    for (const col of columns) {
      expect(within(col).getByTestId('evidence-link').getAttribute('href')).toMatch(
        /\?entry=1[01]#/,
      );
    }
  });

  it('marks every concern lifecycle-unavailable and shows the note when the gate view failed', () => {
    renderBlock(null, [
      {
        section: 'verdicts',
        kind: 'read_failed',
        read: 'getRunGateView',
        message: 'concern lifecycle unavailable: 503',
      },
    ]);
    expect(screen.getByRole('note')).toHaveTextContent('concern lifecycle unavailable');
    for (const b of screen.getAllByTestId('concern-lifecycle')) {
      expect(b).toHaveAttribute('data-lifecycle', 'unavailable');
      expect(b).toHaveTextContent('lifecycle unavailable');
    }
  });

  it('renders an explicit empty state with no verdicts, and notes alone with no model', () => {
    const { unmount } = render(
      <MemoryRouter>
        <VerdictsBlock section={{ model: { verdicts: [] }, notes: [] }} />
      </MemoryRouter>,
    );
    expect(screen.getByText('No review verdicts recorded yet.')).toBeInTheDocument();
    unmount();
    render(
      <MemoryRouter>
        <VerdictsBlock
          section={{
            model: null,
            notes: [
              {
                section: 'verdicts',
                kind: 'read_failed',
                read: 'audit:plan_reviewed',
                message: 'x',
              },
            ],
          }}
        />
      </MemoryRouter>,
    );
    expect(screen.getByRole('note')).toHaveTextContent('x');
    expect(screen.queryByTestId('review-verdict')).not.toBeInTheDocument();
  });
});
