import { describe, expect, it } from 'vitest';
import { render, screen, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import { DiffSummaryBlock } from './diff-summary-block';
import { deriveScopeDivergence, type EvidenceRef } from './narrative';

const staged: EvidenceRef = {
  kind: 'audit',
  runId: 'run-1',
  sequence: 60,
  entryHash: 'hash-60',
  category: 'policy_evaluated',
};

function renderDiff(model: ReturnType<typeof deriveScopeDivergence>, notes: string[] = []) {
  return render(
    <MemoryRouter>
      <DiffSummaryBlock
        section={{
          model,
          notes: notes.map((message) => ({
            section: 'diff',
            kind: 'absent',
            read: 'x',
            message,
          })),
        }}
        declaredEvidence={null}
        stagedEvidence={staged}
      />
    </MemoryRouter>,
  );
}

function divergence(path: string) {
  return document.querySelector(`[data-testid="scope-path"][data-path="${path}"]`);
}

describe('<DiffSummaryBlock>', () => {
  it('marks each path present on only one side', () => {
    renderDiff(
      deriveScopeDivergence(
        [
          { path: 'a.ts', operation: 'modify' },
          { path: 'b.ts', operation: 'create' },
        ],
        {
          diff: [
            { path: 'a.ts', status: 'M' },
            { path: 'z.ts', status: 'A' },
          ],
        },
      ),
    );
    const declared = screen.getByTestId('declared-scope');
    const stagedCol = screen.getByTestId('staged-scope');
    expect(within(declared).getByText('declared, not staged')).toBeInTheDocument();
    expect(within(stagedCol).getByText('staged, not declared')).toBeInTheDocument();
    expect(within(declared).getByText('b.ts').closest('li')).toHaveAttribute(
      'data-divergence',
      'declared_only',
    );
    expect(within(stagedCol).getByText('z.ts').closest('li')).toHaveAttribute(
      'data-divergence',
      'staged_only',
    );
    expect(divergence('a.ts')).toHaveAttribute('data-divergence', 'both');
    expect(within(stagedCol).getByTestId('evidence-link')).toHaveAttribute(
      'href',
      '/runs/run-1?entry=60#entry-60',
    );
  });

  it('a planned rename (delete old + create new) folded against old_path is not divergent', () => {
    renderDiff(
      deriveScopeDivergence(
        [
          { path: 'a.ts', operation: 'delete' },
          { path: 'b.ts', operation: 'create' },
        ],
        { diff: [{ path: 'b.ts', status: 'R', old_path: 'a.ts' }] },
      ),
    );
    expect(screen.queryByText('declared, not staged')).not.toBeInTheDocument();
    expect(screen.queryByText('staged, not declared')).not.toBeInTheDocument();
  });

  it('declared scope unavailable → note, staged column still renders without divergence claims', () => {
    renderDiff(deriveScopeDivergence(null, { diff: [{ path: 'z.ts', status: 'A' }] }), [
      'declared scope unavailable',
    ]);
    expect(screen.getByText('declared scope unavailable')).toBeInTheDocument();
    expect(
      within(screen.getByTestId('declared-scope')).getByText('Unavailable.'),
    ).toBeInTheDocument();
    expect(divergence('z.ts')).toHaveAttribute('data-divergence', 'unknown');
    expect(screen.queryByText('staged, not declared')).not.toBeInTheDocument();
  });

  it('staged scope not yet evaluated → note, declared column still listed', () => {
    renderDiff(deriveScopeDivergence([{ path: 'a.ts', operation: 'modify' }], undefined), [
      'staged scope not yet evaluated',
    ]);
    expect(screen.getByText('staged scope not yet evaluated')).toBeInTheDocument();
    expect(
      within(screen.getByTestId('staged-scope')).getByText('Unavailable.'),
    ).toBeInTheDocument();
    expect(divergence('a.ts')).toHaveAttribute('data-divergence', 'unknown');
  });
});
