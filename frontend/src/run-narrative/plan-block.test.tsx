import { describe, expect, it } from 'vitest';
import { render, screen } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import { PlanBlock } from './plan-block';
import type { PlanModel } from './narrative';

const plan: PlanModel = {
  summary: 'Restructure run detail',
  ticketUrl: 'https://example.test/1715',
  ticketId: '#1715',
  scopeFiles: [
    { path: 'a.ts', operation: 'modify' },
    { path: 'b.ts', operation: 'create' },
  ],
  approachStepCount: 3,
  acceptanceCriteria: [],
  evidence: {
    kind: 'artifact',
    runId: 'run-1',
    stageId: 'st-plan',
    artifactId: 'art-plan',
    artifactKind: 'plan',
    contentHash: 'sha256:plan',
  },
};

describe('<PlanBlock>', () => {
  it('renders the summary, ticket, counts and an evidence link to the plan stage', () => {
    render(
      <MemoryRouter>
        <PlanBlock section={{ model: plan, notes: [] }} />
      </MemoryRouter>,
    );
    expect(screen.getByRole('heading', { name: 'Plan' })).toBeInTheDocument();
    expect(screen.getByText('Restructure run detail')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: '#1715' })).toHaveAttribute(
      'href',
      'https://example.test/1715',
    );
    expect(screen.getByTestId('plan-scope-count')).toHaveTextContent('2 files');
    expect(screen.getByTestId('plan-approach-count')).toHaveTextContent('3 steps');
    expect(screen.getByTestId('evidence-link')).toHaveAttribute(
      'href',
      '/runs/run-1/stages/st-plan',
    );
    expect(screen.queryByRole('note')).not.toBeInTheDocument();
  });

  it('renders the degrade note in place of the body when there is no plan artifact', () => {
    render(
      <MemoryRouter>
        <PlanBlock
          section={{
            model: null,
            notes: [
              {
                section: 'plan',
                kind: 'absent',
                read: 'plan artifact',
                message: 'no plan artifact (run has no plan stage)',
              },
            ],
          }}
        />
      </MemoryRouter>,
    );
    expect(screen.getByRole('note')).toHaveTextContent('no plan artifact');
    expect(screen.queryByTestId('plan-scope-count')).not.toBeInTheDocument();
  });
});
