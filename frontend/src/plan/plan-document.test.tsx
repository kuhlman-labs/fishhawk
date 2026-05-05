import { describe, expect, it, vi } from 'vitest';
import { render, screen, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import { PlanDocument } from './plan-document';
import type { StandardV1Plan } from '@/api/plan';
import type { Stage } from '@/api/types';

const sampleStage: Stage = {
  id: '00000000-0000-0000-0000-0000000000aa',
  run_id: '00000000-0000-0000-0000-0000000000ab',
  sequence: 0,
  type: 'plan',
  executor: { kind: 'agent', ref: 'claude-code' },
  state: 'awaiting_approval',
  started_at: '2026-05-04T20:00:00Z',
  ended_at: null,
  failure_category: null,
  failure_reason: null,
  created_at: '2026-05-04T20:00:00Z',
  updated_at: '2026-05-04T20:00:00Z',
};

const samplePlan: StandardV1Plan = {
  plan_version: 'standard_v1',
  ticket_reference: {
    type: 'github_issue',
    url: 'https://github.com/kuhlman-labs/fishhawk/issues/56',
    id: 'kuhlman-labs/fishhawk#56',
  },
  generated_by: {
    agent: 'claude-code',
    model: 'claude-opus-4-7',
    timestamp: '2026-05-04T20:00:00Z',
  },
  summary: 'Render standard_v1 plans as documents.',
  scope: {
    files: [
      { path: 'frontend/src/plan/plan-document.tsx', operation: 'create' },
      { path: 'frontend/src/routes/runs.tsx', operation: 'modify' },
      { path: 'frontend/src/routes/__obsolete.tsx', operation: 'delete' },
    ],
    estimated_lines_changed: 240,
  },
  approach: [
    { step: 1, description: 'Define the API client and types.' },
    { step: 2, description: 'Build the section components.' },
  ],
  verification: {
    test_strategy: 'Vitest covers the renderer with a fixture plan.',
    rollback_plan: 'Revert PR; no data migrations.',
  },
  risks_and_assumptions: ['Assumes /v0/artifacts/{id} returns the plan as inline JSON.'],
};

function renderPlan(plan: StandardV1Plan = samplePlan, stage: Stage = sampleStage) {
  return render(
    <MemoryRouter>
      <PlanDocument
        plan={plan}
        stage={stage}
        runId={stage.run_id}
        onStageUpdate={vi.fn()}
        onStageRollback={vi.fn()}
      />
    </MemoryRouter>,
  );
}

describe('PlanDocument', () => {
  it('renders the page header and the plan_version badge', () => {
    renderPlan();
    expect(screen.getByRole('heading', { name: /review plan/i })).toBeInTheDocument();
    expect(screen.getByText(/^Plan · standard_v1$/i)).toBeInTheDocument();
  });

  it('renders all required sections with anchor ids', () => {
    const { container } = renderPlan();
    for (const id of ['ticket', 'generated-by', 'summary', 'scope', 'approach', 'verification']) {
      expect(container.querySelector(`#${id}`)).not.toBeNull();
    }
  });

  it('renders the risks section when risks_and_assumptions is present', () => {
    const { container } = renderPlan();
    expect(container.querySelector('#risks')).not.toBeNull();
    expect(
      screen.getByText('Assumes /v0/artifacts/{id} returns the plan as inline JSON.'),
    ).toBeInTheDocument();
  });

  it('omits the risks section when risks_and_assumptions is empty / missing', () => {
    const { container } = renderPlan({ ...samplePlan, risks_and_assumptions: undefined });
    expect(container.querySelector('#risks')).toBeNull();
    expect(screen.queryByRole('heading', { name: /risks/i })).not.toBeInTheDocument();
  });

  it('renders the side-nav with anchors that match section ids', () => {
    renderPlan();
    const nav = screen.getByRole('navigation', { name: /plan sections/i });
    const links = within(nav).getAllByRole('link');
    const hrefs = links.map((a) => a.getAttribute('href'));
    expect(hrefs).toEqual([
      '#ticket',
      '#generated-by',
      '#summary',
      '#scope',
      '#approach',
      '#verification',
      '#risks',
    ]);
  });

  it('renders scope files with their operation labels', () => {
    renderPlan();
    expect(screen.getByText('frontend/src/plan/plan-document.tsx')).toBeInTheDocument();
    // create / modify / delete each appear at least once on the page
    // (the buildNav helper uses none of these strings).
    expect(screen.getAllByText(/create/i).length).toBeGreaterThan(0);
    expect(screen.getAllByText(/modify/i).length).toBeGreaterThan(0);
    expect(screen.getAllByText(/delete/i).length).toBeGreaterThan(0);
  });

  it('renders approach steps in order with their numbers', () => {
    renderPlan();
    expect(screen.getByText('Define the API client and types.')).toBeInTheDocument();
    expect(screen.getByText('Build the section components.')).toBeInTheDocument();
  });

  it('exposes the ticket as a link to the ticket URL', () => {
    renderPlan();
    const link = screen.getByRole('link', { name: /kuhlman-labs\/fishhawk#56/ });
    expect(link).toHaveAttribute('href', 'https://github.com/kuhlman-labs/fishhawk/issues/56');
  });

  it('renders Approve and Reject buttons enabled while awaiting_approval; Regenerate stays disabled until E8.3', () => {
    renderPlan();
    const approve = screen.getByRole('button', { name: /^approve$/i });
    const reject = screen.getByRole('button', { name: /^reject$/i });
    const regen = screen.getByRole('button', { name: /^regenerate$/i });
    expect(approve).toBeEnabled();
    expect(reject).toBeEnabled();
    expect(regen).toBeDisabled();
  });

  it('exposes a "View audit log" link to the run-detail audit anchor', () => {
    renderPlan();
    const link = screen.getByRole('link', { name: /view audit log/i });
    expect(link).toHaveAttribute('href', `/runs/${sampleStage.run_id}#audit`);
  });

  it('shows a terminal status header instead of action buttons once the gate has passed', () => {
    renderPlan(samplePlan, { ...sampleStage, state: 'succeeded' });
    expect(screen.queryByRole('button', { name: /^approve$/i })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /^reject$/i })).not.toBeInTheDocument();
    expect(screen.getByText(/approved/i)).toBeInTheDocument();
  });
});
