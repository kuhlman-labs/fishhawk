import { describe, expect, it, vi } from 'vitest';
import { render, screen, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import type { Artifact, AuditEntry, Run, Stage } from '@/api/types';
import { AcceptanceBlock } from './acceptance-block';
import type { AcceptanceOutcome } from './narrative';
import { loadRunNarrative, type AcceptanceModel, type NarrativeClient } from './use-run-narrative';

const outcome: AcceptanceOutcome = {
  verdict: 'failed',
  failureMode: 'criteria_failed',
  artifactId: 'art-acc',
  contentHash: 'sha256:acc',
  stageId: 'st-acc',
  tallies: { passed: 1, failed: 1, skipped: 0, undecidable: 1, total: 3 },
  evidence: {
    kind: 'audit',
    runId: 'run-1',
    sequence: 90,
    entryHash: 'hash-90',
    category: 'acceptance_outcome_recorded',
  },
};

function renderBlock(model: AcceptanceModel | null, notes: string[] = []) {
  return render(
    <MemoryRouter>
      <AcceptanceBlock
        section={{
          model,
          notes: notes.map((message) => ({
            section: 'acceptance',
            kind: 'read_failed',
            read: 'x',
            message,
          })),
        }}
      />
    </MemoryRouter>,
  );
}

describe('<AcceptanceBlock>', () => {
  it('renders one row per criterion mapping the wire result, pending when unrecorded', () => {
    renderBlock({
      outcome,
      body: null,
      artifact: {
        kind: 'artifact',
        runId: 'run-1',
        stageId: 'st-acc',
        artifactId: 'art-acc',
        artifactKind: 'acceptance',
        contentHash: 'sha256:acc',
      },
      rows: [
        { id: 'c1', statement: 'one', status: 'passed', inPlan: true },
        { id: 'c2', statement: 'two', status: 'failed', inPlan: true, observed: 'got 500' },
        {
          id: 'c3',
          statement: 'three',
          status: 'undecidable',
          inPlan: true,
          undecidableReason: 'no target',
        },
        { id: 'c4', statement: 'four', status: 'pending', inPlan: true },
        { id: 'cx', statement: '', status: 'skipped', inPlan: false },
      ],
    });
    const rows = screen.getAllByTestId('acceptance-row');
    expect(rows.map((r) => within(r).getByTestId('acceptance-row-verdict').textContent)).toEqual([
      'pass',
      'fail',
      'undecidable',
      'pending',
      'skipped',
    ]);
    expect(within(rows[1]).getByText('observed: got 500')).toBeInTheDocument();
    expect(within(rows[2]).getByText('undecidable: no target')).toBeInTheDocument();
    expect(within(rows[4]).getByText('(not in plan)')).toBeInTheDocument();
    // Recorded rows link to the acceptance artifact; a pending row has no evidence yet.
    expect(within(rows[0]).getByTestId('evidence-link')).toHaveAttribute(
      'href',
      '/runs/run-1/stages/st-acc',
    );
    expect(within(rows[3]).queryByTestId('evidence-link')).not.toBeInTheDocument();
    expect(screen.getByTestId('acceptance-verdict')).toHaveTextContent('failed');
    expect(screen.getByText('criteria_failed')).toBeInTheDocument();
  });

  it('artifact unreadable → verdict and tallies from the audit row, no criterion table', () => {
    renderBlock({ outcome, rows: null, body: null, artifact: null }, [
      'per-criterion evidence unavailable: boom',
    ]);
    expect(screen.getByTestId('acceptance-tallies')).toHaveTextContent(
      '1 passed · 1 failed · 0 skipped · 1 undecidable · 3 total',
    );
    expect(screen.getByRole('note')).toHaveTextContent('per-criterion evidence unavailable');
    expect(screen.queryByTestId('acceptance-row')).not.toBeInTheDocument();
  });

  it('no outcome yet → explicit empty state and every plan criterion pending', () => {
    renderBlock({
      outcome: null,
      body: null,
      artifact: null,
      rows: [{ id: 'c1', statement: 'one', status: 'pending', inPlan: true }],
    });
    expect(screen.getByText('No acceptance outcome recorded yet.')).toBeInTheDocument();
    expect(screen.getByTestId('acceptance-row')).toHaveAttribute('data-status', 'pending');
  });
});

describe('<AcceptanceBlock> through the loader guard (isAcceptanceArtifact)', () => {
  it('a pull_request-shaped artifact under the acceptance id renders the unreadable fallback', async () => {
    const RUN = 'run-1';
    const planStage = { id: 'st-plan', sequence: 1, type: 'plan', state: 'succeeded' } as Stage;
    const outcomeEntry: AuditEntry = {
      id: 'e90',
      sequence: 90,
      run_id: RUN,
      stage_id: 'st-acc',
      ts: 'T',
      category: 'acceptance_outcome_recorded',
      actor_kind: 'system',
      actor_subject: null,
      payload: { artifact_id: 'art-acc', verdict: 'passed', criteria_passed: 1, criteria_total: 1 },
      prev_hash: null,
      entry_hash: 'hash-90',
    };
    // Built by hand, never passed through the guard in setup: no `verdict` key.
    const prShaped: Artifact = {
      id: 'art-acc',
      stage_id: 'st-acc',
      kind: 'acceptance',
      schema_version: null,
      content_hash: 'sha256:pr',
      created_at: 'T',
      content: {
        pr_number: 9,
        pr_url: 'https://github.com/o/r/pull/9',
        branch: 'b',
        head_sha: 'h',
        base_sha: 'b',
        title: 't',
        files_changed_count: 2,
      },
    };
    const client: NarrativeClient = {
      getRun: vi.fn().mockResolvedValue({ id: RUN } as Run),
      listRunStages: vi.fn().mockResolvedValue({ items: [planStage] }),
      getRunGateView: vi.fn().mockRejectedValue(new Error('n/a')),
      listRunAudit: vi.fn().mockImplementation(async (_r: string, p?: { category?: string }) => ({
        items: p?.category === 'acceptance_outcome_recorded' ? [outcomeEntry] : [],
        next_cursor: null,
      })),
      listStageArtifacts: vi.fn().mockResolvedValue({ items: [] }),
      getArtifact: vi.fn().mockResolvedValue(prShaped),
    };
    const n = await loadRunNarrative(RUN, client);
    render(
      <MemoryRouter>
        <AcceptanceBlock section={n.sections.acceptance} />
      </MemoryRouter>,
    );
    expect(screen.getByText(/unreadable acceptance artifact/)).toBeInTheDocument();
    expect(screen.getByTestId('acceptance-verdict')).toHaveTextContent('passed');
    expect(screen.queryByTestId('acceptance-row')).not.toBeInTheDocument();
  });
});
