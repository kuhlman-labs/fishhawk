import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import { DeployDocument } from './deploy-document';
import { api } from '@/api/client';
import type { DeploymentArtifactBody } from '@/api/deployment';
import type { Stage } from '@/api/types';

const sampleArtifact: DeploymentArtifactBody = {
  environment: 'production',
  ref: '1234567890abcdef1234567890abcdef12345678',
  external_run_url: 'https://github.com/kuhlman-labs/fishhawk/actions/runs/42',
  outcome: 'succeeded',
};

const baseStage: Stage = {
  id: '00000000-0000-0000-0000-0000000000aa',
  run_id: '00000000-0000-0000-0000-0000000000ab',
  sequence: 3,
  type: 'deploy',
  executor: { kind: 'agent', ref: 'delegating-deploy' },
  state: 'awaiting_deploy_approval',
  started_at: '2026-05-04T20:00:00Z',
  ended_at: null,
  failure_category: null,
  failure_reason: null,
  created_at: '2026-05-04T20:00:00Z',
  updated_at: '2026-05-04T20:05:00Z',
  gate: { type: 'approval', approvers: { any_of: ['founder'] } },
};

function renderDoc(
  stageOverride: Partial<Stage> = {},
  artifact: DeploymentArtifactBody | null = null,
) {
  const stage = { ...baseStage, ...stageOverride };
  const onStageUpdate = vi.fn();
  const onStageRollback = vi.fn();
  render(
    <MemoryRouter>
      <DeployDocument
        artifact={artifact}
        stage={stage}
        runId={stage.run_id}
        onStageUpdate={onStageUpdate}
        onStageRollback={onStageRollback}
      />
    </MemoryRouter>,
  );
  return { onStageUpdate, onStageRollback };
}

describe('<DeployDocument>', () => {
  beforeEach(() => vi.restoreAllMocks());
  afterEach(() => vi.restoreAllMocks());

  it('renders the deploy eyebrow and stage heading', () => {
    renderDoc();
    expect(screen.getByRole('heading', { name: /deploy stage/i })).toBeInTheDocument();
    expect(screen.getByText(/^Deploy · stage$/i)).toBeInTheDocument();
  });

  // (a) Pre-execution gate: approve/reject affordance + parked badge.
  it('shows the approval gate buttons and the parked badge at awaiting_deploy_approval', () => {
    renderDoc();
    expect(screen.getByText('awaiting_deploy_approval')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /^approve$/i })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /^reject$/i })).toBeInTheDocument();
  });

  it('renders the inline note (and still the gate) when no artifact has landed yet', () => {
    renderDoc();
    expect(screen.getByText(/no deployment artifact yet/i)).toBeInTheDocument();
    // The gate is still visible so the operator can act pre-execution.
    expect(screen.getByRole('button', { name: /^approve$/i })).toBeInTheDocument();
  });

  // (b) In-flight: no approve/reject affordance, in-flight status line.
  it('shows the in-flight state and no approve/reject buttons at awaiting_deployment', () => {
    renderDoc({ state: 'awaiting_deployment' });
    expect(screen.getByText('awaiting_deployment')).toBeInTheDocument();
    expect(screen.getByText(/deploying/i)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /^approve$/i })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /^reject$/i })).not.toBeInTheDocument();
  });

  // (c) Terminal succeeded deploy renders the artifact fields.
  it('renders the deployment artifact fields on a succeeded deploy', () => {
    renderDoc({ state: 'succeeded' }, sampleArtifact);
    expect(screen.getByText('production')).toBeInTheDocument();
    expect(screen.getByText(sampleArtifact.ref)).toBeInTheDocument();
    expect(screen.getByTestId('deploy-outcome')).toHaveTextContent('succeeded');
    const link = screen.getByRole('link', { name: /view external run/i });
    expect(link).toHaveAttribute('href', sampleArtifact.external_run_url);
  });

  it('renders the rollback handle and action when present', () => {
    renderDoc(
      { state: 'succeeded' },
      {
        ...sampleArtifact,
        outcome: 'rolled_back',
        rollback_handle: 'rollback-token-7',
        rollback_action: 'completed',
      },
    );
    expect(screen.getByText('rollback-token-7')).toBeInTheDocument();
    expect(screen.getByText('completed')).toBeInTheDocument();
    expect(screen.getByTestId('deploy-outcome')).toHaveTextContent('rolled_back');
  });

  it('renders the failed outcome badge on a failed deploy', () => {
    renderDoc({ state: 'failed', failure_category: 'C' }, { ...sampleArtifact, outcome: 'failed' });
    expect(screen.getByTestId('deploy-outcome')).toHaveTextContent('failed');
  });

  // (d) Approve flow applies the optimistic awaiting_deployment update
  // and reconciles against the server-returned stage.
  it('approves with an optimistic awaiting_deployment update and reconciles on the server stage', async () => {
    const deploying: Stage = { ...baseStage, state: 'awaiting_deployment' };
    const spy = vi.spyOn(api, 'submitApproval').mockResolvedValue(deploying);
    const { onStageUpdate, onStageRollback } = renderDoc();

    fireEvent.click(screen.getByRole('button', { name: /^approve$/i }));
    fireEvent.change(screen.getByLabelText(/approve comment/i), { target: { value: 'ship it' } });
    fireEvent.click(screen.getByRole('button', { name: /confirm approve/i }));

    await waitFor(() => expect(onStageUpdate).toHaveBeenCalledTimes(2));
    // Optimistic target is awaiting_deployment (NOT succeeded).
    expect(onStageUpdate.mock.calls[0][0]).toMatchObject({ state: 'awaiting_deployment' });
    expect(onStageUpdate.mock.calls[1][0]).toEqual(deploying);
    expect(onStageRollback).not.toHaveBeenCalled();
    expect(spy).toHaveBeenCalledWith(baseStage.id, { decision: 'approve', comment: 'ship it' });
  });

  // (d') Reject path → optimistic failed-D, reconciled on the server stage.
  it('rejects with an optimistic failed-D update', async () => {
    const rejected: Stage = {
      ...baseStage,
      state: 'failed',
      failure_category: 'D',
      failure_reason: 'gate rejected',
    };
    vi.spyOn(api, 'submitApproval').mockResolvedValue(rejected);
    const { onStageUpdate, onStageRollback } = renderDoc();

    fireEvent.click(screen.getByRole('button', { name: /^reject$/i }));
    fireEvent.click(screen.getByRole('button', { name: /confirm reject/i }));

    await waitFor(() => expect(onStageUpdate).toHaveBeenCalledTimes(2));
    expect(onStageUpdate.mock.calls[0][0]).toMatchObject({
      state: 'failed',
      failure_category: 'D',
    });
    expect(onStageUpdate.mock.calls[1][0]).toEqual(rejected);
    expect(onStageRollback).not.toHaveBeenCalled();
  });

  // (d'') Error path → rollback + surfaced error, approve button returns.
  it('rolls back the optimistic update and surfaces the error when the call fails', async () => {
    vi.spyOn(api, 'submitApproval').mockRejectedValue(new Error('boom'));
    const { onStageRollback } = renderDoc();

    fireEvent.click(screen.getByRole('button', { name: /^approve$/i }));
    fireEvent.click(screen.getByRole('button', { name: /confirm approve/i }));

    await waitFor(() => expect(onStageRollback).toHaveBeenCalledWith(baseStage));
    expect(screen.getByRole('alert')).toHaveTextContent(/boom/);
    expect(screen.getByRole('button', { name: /^approve$/i })).toBeEnabled();
  });

  it('cancels the confirmation without firing a request', () => {
    const spy = vi.spyOn(api, 'submitApproval');
    renderDoc();
    fireEvent.click(screen.getByRole('button', { name: /^approve$/i }));
    fireEvent.click(screen.getByRole('button', { name: /^cancel$/i }));
    expect(screen.getByRole('button', { name: /^approve$/i })).toBeEnabled();
    expect(spy).not.toHaveBeenCalled();
  });
});
